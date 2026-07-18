// Package ecs wraps the AWS ECS client: idempotent RunTask (ClientToken =
// gameserver_id, StartedBy = "arena:{gameserver_id}"), StopTask, task
// definition registration cached per fleet spec_hash, and token-bucket rate
// limiting for scale-ups.
package ecs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/encoding/protojson"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/store"
)

// startedByPrefix ties Tasks back to GameServers without relying on the
// task_arn write-back (orphan detection).
const startedByPrefix = "arena:"

// StartedBy formats the RunTask startedBy field for a GameServer.
func StartedBy(gsID string) string { return startedByPrefix + gsID }

// ParseStartedBy extracts the GameServer ID from a task's startedBy field.
func ParseStartedBy(s string) (string, bool) {
	id, ok := strings.CutPrefix(s, startedByPrefix)
	return id, ok && id != ""
}

// API is the AWS ECS surface the launcher uses.
type API interface {
	RunTask(ctx context.Context, in *awsecs.RunTaskInput, opts ...func(*awsecs.Options)) (*awsecs.RunTaskOutput, error)
	StopTask(ctx context.Context, in *awsecs.StopTaskInput, opts ...func(*awsecs.Options)) (*awsecs.StopTaskOutput, error)
	RegisterTaskDefinition(ctx context.Context, in *awsecs.RegisterTaskDefinitionInput, opts ...func(*awsecs.Options)) (*awsecs.RegisterTaskDefinitionOutput, error)
}

// Config is the environment the launcher runs game servers in.
type Config struct {
	Cluster          string
	Subnets          []string
	SecurityGroups   []string
	AssignPublicIP   bool
	ExecutionRoleARN string
	TaskRoleARN      string // GameServer task role: CloudWatch Logs only
	SidecarImage     string
	GatewayEndpoint  string // arena-api SDK Gateway URL handed to the sidecar
	LogGroup         string
	Region           string
	// RunTasksPerSecond smooths scale-ups under the ECS RunTask rate limit.
	// Zero means a conservative default of 5/s.
	RunTasksPerSecond float64
}

// Launcher starts and stops GameServer tasks.
type Launcher struct {
	api     API
	cfg     Config
	limiter *rate.Limiter

	mu       sync.Mutex
	taskDefs map[string]string // spec_hash → task definition ARN
}

// NewLauncher returns a Launcher.
func NewLauncher(api API, cfg Config) *Launcher {
	rps := cfg.RunTasksPerSecond
	if rps <= 0 {
		rps = 5
	}
	return &Launcher{
		api:      api,
		cfg:      cfg,
		limiter:  rate.NewLimiter(rate.Limit(rps), int(rps)),
		taskDefs: map[string]string{},
	}
}

// Launch runs one GameServer task for the fleet, idempotently (ClientToken =
// gameserver_id, so a controller restart cannot double-run). Returns the
// task ARN when ECS reports it synchronously ("" otherwise — the RUNNING
// event carries it either way).
func (l *Launcher) Launch(ctx context.Context, fleet *store.Fleet, gsID string) (string, error) {
	if err := l.limiter.Wait(ctx); err != nil {
		return "", err
	}
	taskDef, err := l.taskDefinitionFor(ctx, fleet)
	if err != nil {
		return "", err
	}

	// Fleet-level network overrides; controller defaults
	// otherwise.
	subnets, sgs, assignPublic := l.cfg.Subnets, l.cfg.SecurityGroups, l.cfg.AssignPublicIP
	if fleet.NetworkJSON != "" {
		nw := &arenav1.Network{}
		if err := protojson.Unmarshal([]byte(fleet.NetworkJSON), nw); err != nil {
			return "", fmt.Errorf("fleet %s network: %w", fleet.ID, err)
		}
		if len(nw.GetSubnets()) > 0 {
			subnets = nw.GetSubnets()
		}
		if len(nw.GetSecurityGroups()) > 0 {
			sgs = nw.GetSecurityGroups()
		}
		if nw.AssignPublicIp != nil {
			assignPublic = nw.GetAssignPublicIp()
		}
	}
	assign := ecstypes.AssignPublicIpDisabled
	if assignPublic {
		assign = ecstypes.AssignPublicIpEnabled
	}

	in := &awsecs.RunTaskInput{
		Cluster:        aws.String(l.cfg.Cluster),
		TaskDefinition: aws.String(taskDef),
		ClientToken:    aws.String(gsID),
		StartedBy:      aws.String(StartedBy(gsID)),
		PropagateTags:  ecstypes.PropagateTagsTaskDefinition,
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        subnets,
				SecurityGroups: sgs,
				AssignPublicIp: assign,
			},
		},
		Overrides: &ecstypes.TaskOverride{
			ContainerOverrides: []ecstypes.ContainerOverride{{
				Name: aws.String("arena-sidecar"),
				Environment: []ecstypes.KeyValuePair{
					{Name: aws.String("ARENA_GAMESERVER_ID"), Value: aws.String(gsID)},
					{Name: aws.String("ARENA_GATEWAY_ENDPOINT"), Value: aws.String(l.cfg.GatewayEndpoint)},
				},
			}},
		},
	}
	// Capacity providers replace the launch type — the
	// arena equivalent of Agones eviction.safe: Spot-heavy strategies accept
	// interruption (paired with the spot interruption drain), On-Demand-only refuses it.
	if strategy := l.capacityStrategy(fleet); len(strategy) > 0 {
		in.CapacityProviderStrategy = strategy
	} else {
		in.LaunchType = ecstypes.LaunchTypeFargate
	}
	out, err := l.api.RunTask(ctx, in)
	if err != nil {
		// Leave the Scheduled record in place: no RUNNING event will come,
		// and resync marks it Unhealthy after the startup timeout.
		return "", err
	}
	if len(out.Failures) > 0 {
		f := out.Failures[0]
		return "", fmt.Errorf("RunTask failure: %s (%s)", aws.ToString(f.Reason), aws.ToString(f.Detail))
	}
	if len(out.Tasks) > 0 {
		return aws.ToString(out.Tasks[0].TaskArn), nil
	}
	return "", nil
}

// Stop stops a task; a missing task is treated as already stopped.
func (l *Launcher) Stop(ctx context.Context, taskARN, reason string) error {
	if taskARN == "" {
		return nil
	}
	_, err := l.api.StopTask(ctx, &awsecs.StopTaskInput{
		Cluster: aws.String(l.cfg.Cluster),
		Task:    aws.String(taskARN),
		Reason:  aws.String(reason),
	})
	if err != nil && strings.Contains(err.Error(), "was not found") {
		return nil
	}
	return err
}

// capacityStrategy parses the fleet's capacity providers.
func (l *Launcher) capacityStrategy(fleet *store.Fleet) []ecstypes.CapacityProviderStrategyItem {
	if fleet.CapacityJSON == "" {
		return nil
	}
	cap := &arenav1.Capacity{}
	if err := protojson.Unmarshal([]byte(fleet.CapacityJSON), cap); err != nil {
		return nil // validated at the API; a corrupt record falls back to defaults
	}
	var out []ecstypes.CapacityProviderStrategyItem
	for _, p := range cap.GetProviders() {
		out = append(out, ecstypes.CapacityProviderStrategyItem{
			CapacityProvider: aws.String(p.GetName()),
			Weight:           p.GetWeight(),
			Base:             p.GetBase(),
		})
	}
	return out
}

// taskDefinitionFor registers (once per spec_hash) and returns the task
// definition ARN, synthesizing the sidecar container into the template
// (sidecar injection). Multi-container specs map 1:1 onto
// containerDefinitions; the game container additionally
// carries the port mappings, drain stopTimeout, and ARENA_PORT_* variables.
func (l *Launcher) taskDefinitionFor(ctx context.Context, fleet *store.Fleet) (string, error) {
	l.mu.Lock()
	if arn, ok := l.taskDefs[fleet.SpecHash]; ok {
		l.mu.Unlock()
		return arn, nil
	}
	l.mu.Unlock()

	tmpl := &arenav1.GameServerTemplate{}
	if fleet.TemplateJSON != "" {
		if err := protojson.Unmarshal([]byte(fleet.TemplateJSON), tmpl); err != nil {
			return "", fmt.Errorf("fleet %s template: %w", fleet.ID, err)
		}
	}
	spec := tmpl.GetSpec()
	containers := convert.Containers(spec)
	game := convert.GameContainer(spec)
	if game.GetImage() == "" {
		return "", fmt.Errorf("fleet %s template has no game container image", fleet.ID)
	}

	// Task sizing comes from the game container's resources; auxiliary
	// containers share the task allocation via their own reservations.
	cpu := game.GetResources().GetCpu()
	if cpu == "" {
		cpu = "1024"
	}
	memory := game.GetResources().GetMemory()
	if memory == "" {
		memory = "2048"
	}

	// Container-level splits: the sidecar gets a small fixed reservation and
	// the game container the remainder, so a runaway game process cannot
	// starve the sidecar's heartbeats.
	const sidecarCPU, sidecarMemory = 128, 256
	taskCPU, _ := strconv.Atoi(cpu)
	taskMemory, _ := strconv.Atoi(memory)
	gameCPU, gameMemory := int32(taskCPU-sidecarCPU), int32(taskMemory-sidecarMemory)
	if gameCPU <= 0 {
		gameCPU = int32(taskCPU)
	}
	if gameMemory <= 0 {
		gameMemory = int32(taskMemory)
	}

	var defs []ecstypes.ContainerDefinition
	for _, c := range containers {
		isGame := c.GetName() == game.GetName()
		def := ecstypes.ContainerDefinition{
			Name:             aws.String(c.GetName()),
			Image:            aws.String(c.GetImage()),
			Essential:        aws.Bool(isGame),
			LogConfiguration: l.logConfig(c.GetName()),
		}
		if isGame {
			def.Cpu, def.Memory = gameCPU, aws.Int32(gameMemory)
			if fleet.DrainGraceSeconds > 0 {
				def.StopTimeout = aws.Int32(fleet.DrainGraceSeconds)
			}
		} else if r := c.GetResources(); r.GetCpu() != "" || r.GetMemory() != "" {
			if v, err := strconv.Atoi(r.GetCpu()); err == nil {
				def.Cpu = int32(v)
			}
			if v, err := strconv.Atoi(r.GetMemory()); err == nil {
				def.Memory = aws.Int32(int32(v))
			}
		}
		if len(c.GetCommand()) > 0 {
			def.EntryPoint = c.GetCommand()
		}
		if len(c.GetArgs()) > 0 {
			def.Command = c.GetArgs()
		}
		if c.GetWorkingDirectory() != "" {
			def.WorkingDirectory = aws.String(c.GetWorkingDirectory())
		}
		for _, e := range c.GetEnvironment() {
			def.Environment = append(def.Environment, ecstypes.KeyValuePair{
				Name: aws.String(e.GetName()), Value: aws.String(e.GetValue()),
			})
		}
		for _, s := range c.GetSecrets() {
			def.Secrets = append(def.Secrets, ecstypes.Secret{
				Name: aws.String(s.GetName()), ValueFrom: aws.String(s.GetValueFrom()),
			})
		}
		if hc := c.GetHealthCheck(); len(hc.GetCommand()) > 0 {
			def.HealthCheck = &ecstypes.HealthCheck{
				Command:     hc.GetCommand(),
				Interval:    positiveOrNil(hc.GetIntervalSeconds()),
				Timeout:     positiveOrNil(hc.GetTimeoutSeconds()),
				Retries:     positiveOrNil(hc.GetRetries()),
				StartPeriod: positiveOrNil(hc.GetStartPeriodSeconds()),
			}
		}
		for _, m := range c.GetMountPoints() {
			def.MountPoints = append(def.MountPoints, ecstypes.MountPoint{
				SourceVolume:  aws.String(m.GetVolume()),
				ContainerPath: aws.String(m.GetContainerPath()),
				ReadOnly:      aws.Bool(m.GetReadOnly()),
			})
		}
		if isGame {
			for _, p := range spec.GetPorts() {
				proto := ecstypes.TransportProtocolUdp
				if p.GetProtocol() == arenav1.Port_PROTOCOL_TCP {
					proto = ecstypes.TransportProtocolTcp
				}
				def.PortMappings = append(def.PortMappings, ecstypes.PortMapping{
					ContainerPort: aws.Int32(p.GetContainerPort()),
					Protocol:      proto,
				})
				// Passthrough contract: the game learns its
				// ports from the environment instead of hardcoding them.
				def.Environment = append(def.Environment, ecstypes.KeyValuePair{
					Name:  aws.String("ARENA_PORT_" + envName(p.GetName())),
					Value: aws.String(strconv.Itoa(int(p.GetContainerPort()))),
				})
			}
		}
		defs = append(defs, def)
	}

	defs = append(defs, ecstypes.ContainerDefinition{
		Name:             aws.String("arena-sidecar"),
		Image:            aws.String(l.cfg.SidecarImage),
		Essential:        aws.Bool(true),
		Cpu:              sidecarCPU,
		Memory:           aws.Int32(sidecarMemory),
		LogConfiguration: l.logConfig("sidecar"),
	})

	var volumes []ecstypes.Volume
	for _, v := range spec.GetVolumes() {
		vol := ecstypes.Volume{Name: aws.String(v.GetName())}
		if efs := v.GetEfs(); efs != nil {
			vol.EfsVolumeConfiguration = &ecstypes.EFSVolumeConfiguration{
				FileSystemId:  aws.String(efs.GetFileSystemId()),
				RootDirectory: aws.String(efs.GetRootDirectory()),
			}
		} else if h := v.GetHost(); h != nil {
			vol.Host = &ecstypes.HostVolumeProperties{SourcePath: aws.String(h.GetSourcePath())}
		}
		volumes = append(volumes, vol)
	}

	family := "arena-gs-" + fleet.Name + "-" + fleet.SpecHash
	out, err := l.api.RegisterTaskDefinition(ctx, &awsecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(family),
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		Cpu:                     aws.String(cpu),
		Memory:                  aws.String(memory),
		ExecutionRoleArn:        aws.String(l.cfg.ExecutionRoleARN),
		TaskRoleArn:             aws.String(l.cfg.TaskRoleARN),
		ContainerDefinitions:    defs,
		Volumes:                 volumes,
	})
	if err != nil {
		return "", fmt.Errorf("register task definition %s: %w", family, err)
	}
	arn := aws.ToString(out.TaskDefinition.TaskDefinitionArn)

	l.mu.Lock()
	l.taskDefs[fleet.SpecHash] = arn
	l.mu.Unlock()
	return arn, nil
}

func positiveOrNil(v int32) *int32 {
	if v <= 0 {
		return nil
	}
	return aws.Int32(v)
}

// envName turns a port name into an environment variable suffix
// ("game-udp" → "GAME_UDP").
func envName(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, "-", "_"))
}

func (l *Launcher) logConfig(streamPrefix string) *ecstypes.LogConfiguration {
	if l.cfg.LogGroup == "" {
		return nil
	}
	return &ecstypes.LogConfiguration{
		LogDriver: ecstypes.LogDriverAwslogs,
		Options: map[string]string{
			"awslogs-group":         l.cfg.LogGroup,
			"awslogs-region":        l.cfg.Region,
			"awslogs-stream-prefix": streamPrefix,
		},
	}
}
