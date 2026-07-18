package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
)

// ManagedByLabel marks resources applied by arenactl; --prune only ever
// touches resources carrying it.
const (
	ManagedByLabel = "arena.dev/managed-by"
	ManagedByValue = "arenactl"
)

// Manifest is one decoded Fleet document, already converted to the API
// types (arena.v1.FleetSpec).
type Manifest struct {
	Namespace string
	Name      string
	Labels    map[string]string
	Spec      *arenav1.FleetSpec
	// Source is "file (doc N)" for error reporting.
	Source string
}

// ---------------------------------------------------------------------------
// Document shape: flat, ECS-flavored. Field names deliberately mirror the
// ECS task definition (containerDefinitions, portMappings, containerPort,
// healthCheck), the ECS service definition (desiredCount), and Application
// Auto Scaling (minCapacity/maxCapacity), so a fleet definition converts
// from/to existing ECS JSON by renaming almost nothing.

type fleetDoc struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
	// Tags are the fleet's labels (the management marker is attached here).
	Tags map[string]string `yaml:"tags,omitempty"`

	// DesiredCount ⇔ ECS service desiredCount. Omit while autoScaling is
	// enabled — the Autoscale reconciler owns it.
	DesiredCount *int32 `yaml:"desiredCount,omitempty"`

	// Scheduling: "packed" | "distributed" (EC2 placement only).
	Scheduling string `yaml:"scheduling,omitempty"`

	TaskDefinition *taskDefinitionDoc `yaml:"taskDefinition,omitempty"`
	AutoScaling    *autoScalingDoc    `yaml:"autoScaling,omitempty"`

	// Strategy controls how template changes roll out.
	// Omitted = server default (RollingUpdate, 25%/25%).
	Strategy *strategyDoc `yaml:"strategy,omitempty"`

	// AllocationOverflow labels/annotates Allocated servers that exceed the
	// desired count during scale-down or an update.
	AllocationOverflow *allocationOverflowDoc `yaml:"allocationOverflow,omitempty"`

	// Capacity selects the ECS capacityProviderStrategy —
	// the arena equivalent of Agones eviction.safe. Omitted = controller
	// default.
	Capacity *capacityDoc `yaml:"capacity,omitempty"`

	// Network overrides subnets/securityGroups/assignPublicIp for launched
	// tasks. Omitted = controller default.
	Network *networkDoc `yaml:"network,omitempty"`

	// DrainGraceSeconds is the ECS stopTimeout for the game container: the
	// SIGTERM → SIGKILL window used by Spot interruption and scale-down.
	// Fargate caps this at 120. 0 = ECS default (30).
	DrainGraceSeconds int32 `yaml:"drainGraceSeconds,omitempty"`
}

type strategyDoc struct {
	Type          string            `yaml:"type"` // "rollingUpdate" (default) | "recreate"
	RollingUpdate *rollingUpdateDoc `yaml:"rollingUpdate,omitempty"`
}

type rollingUpdateDoc struct {
	MaxSurge       string `yaml:"maxSurge,omitempty"`
	MaxUnavailable string `yaml:"maxUnavailable,omitempty"`
	// DrainTimeoutSeconds force-drains remaining old-generation Allocated
	// servers once an update has run this long. 0 (default) waits
	// indefinitely for sessions to end, matching Agones.
	DrainTimeoutSeconds int64 `yaml:"drainTimeoutSeconds,omitempty"`
}

type allocationOverflowDoc struct {
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

type capacityDoc struct {
	Providers []capacityProviderDoc `yaml:"providers,omitempty"`
}

type capacityProviderDoc struct {
	// e.g. "FARGATE", "FARGATE_SPOT", or a named EC2 capacity provider.
	Name   string `yaml:"name"`
	Weight int32  `yaml:"weight,omitempty"`
	Base   int32  `yaml:"base,omitempty"`
}

type networkDoc struct {
	// Defaults to true (direct-connect model) when omitted.
	AssignPublicIP *bool    `yaml:"assignPublicIp,omitempty"`
	SecurityGroups []string `yaml:"securityGroups,omitempty"`
	Subnets        []string `yaml:"subnets,omitempty"`
}

type taskDefinitionDoc struct {
	// CPU / Memory are the ECS task-level strings ("1024", "2048"), applied
	// to the game container's Resources (the sidecar's own reservation is
	// subtracted from it at launch).
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
	// Tags become GameServer labels (allocation selectors match on them).
	Tags map[string]string `yaml:"tags,omitempty"`
	// Annotations is an arena extension (free-form GameServer metadata).
	Annotations map[string]string `yaml:"annotations,omitempty"`
	// One entry = the single-container form. Multiple entries need
	// gameContainer to say which one is the game process (SDK identity,
	// ports). The SDK sidecar is injected by the
	// controller and never appears in the definition either way.
	ContainerDefinitions []containerDoc `yaml:"containerDefinitions"`
	// GameContainer names the game container when there is more than one;
	// ignored (and unnecessary) for the single-container form.
	GameContainer string `yaml:"gameContainer,omitempty"`
	// Volumes are mountable by name via containerDefinitions[].mountPoints.
	Volumes []volumeDoc `yaml:"volumes,omitempty"`
}

type containerDoc struct {
	Name         string      `yaml:"name"`
	Image        string      `yaml:"image"`
	Environment  []envVarDoc `yaml:"environment,omitempty"`
	PortMappings []portDoc   `yaml:"portMappings,omitempty"`
	// HealthCheck is the SDK heartbeat expectation (arena's own heartbeat
	// model, not a Docker healthcheck) — only meaningful on the game
	// container. See containerHealthCheck for the ECS-native equivalent.
	HealthCheck *healthDoc `yaml:"healthCheck,omitempty"`

	// Entrypoint / arguments / working directory.
	Command          []string `yaml:"command,omitempty"`
	Args             []string `yaml:"args,omitempty"`
	WorkingDirectory string   `yaml:"workingDirectory,omitempty"`

	// Secrets Manager / SSM references injected as environment variables.
	// Values never appear in manifests.
	Secrets []secretDoc `yaml:"secrets,omitempty"`

	// ContainerHealthCheck is the ECS-native container healthcheck (a
	// command run inside the container) — early launch-failure detection,
	// a different role from the SDK heartbeat in HealthCheck above.
	ContainerHealthCheck *containerHealthCheckDoc `yaml:"containerHealthCheck,omitempty"`

	// MountPoints reference taskDefinition.volumes entries by name.
	MountPoints []mountPointDoc `yaml:"mountPoints,omitempty"`

	// Resources are per-container cpu/memory soft limits for non-game
	// containers in a multi-container task. The game container's sizing
	// instead comes from the task-level cpu/memory above.
	Resources *resourcesDoc `yaml:"resources,omitempty"`
}

type secretDoc struct {
	Name string `yaml:"name"`
	// ValueFrom is a Secrets Manager or SSM parameter ARN.
	ValueFrom string `yaml:"valueFrom"`
}

// containerHealthCheckDoc mirrors the ECS container healthCheck vocabulary
// literally (a command, unlike the SDK-heartbeat HealthCheck field).
type containerHealthCheckDoc struct {
	Command            []string `yaml:"command,omitempty"`
	IntervalSeconds    int32    `yaml:"intervalSeconds,omitempty"`
	TimeoutSeconds     int32    `yaml:"timeoutSeconds,omitempty"`
	Retries            int32    `yaml:"retries,omitempty"`
	StartPeriodSeconds int32    `yaml:"startPeriodSeconds,omitempty"`
}

type mountPointDoc struct {
	// Volume names a taskDefinition.volumes entry.
	Volume        string `yaml:"volume"`
	ContainerPath string `yaml:"containerPath"`
	ReadOnly      bool   `yaml:"readOnly,omitempty"`
}

type resourcesDoc struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}

// getCPU/getMemory are nil-safe (proto-getter style) for Encode's use on a
// possibly-absent per-container resources block.
func (r *resourcesDoc) getCPU() string {
	if r == nil {
		return ""
	}
	return r.CPU
}

func (r *resourcesDoc) getMemory() string {
	if r == nil {
		return ""
	}
	return r.Memory
}

type volumeDoc struct {
	Name string   `yaml:"name"`
	EFS  *efsDoc  `yaml:"efs,omitempty"`
	Host *hostDoc `yaml:"host,omitempty"`
}

type efsDoc struct {
	FileSystemID  string `yaml:"fileSystemId"`
	RootDirectory string `yaml:"rootDirectory,omitempty"`
}

type hostDoc struct {
	SourcePath string `yaml:"sourcePath"`
}

type envVarDoc struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type portDoc struct {
	Name          string `yaml:"name"`
	ContainerPort int32  `yaml:"containerPort"`
	Protocol      string `yaml:"protocol"` // "udp" (default) | "tcp"
}

// healthDoc mirrors the ECS container healthCheck vocabulary, applied to
// arena's heartbeat model: startPeriod ⇔ initial delay, interval ⇔ expected
// heartbeat interval, retries ⇔ missed beats before Unhealthy.
type healthDoc struct {
	StartPeriod int32 `yaml:"startPeriod,omitempty"`
	Interval    int32 `yaml:"interval,omitempty"`
	Retries     int32 `yaml:"retries,omitempty"`
}

type autoScalingDoc struct {
	Enabled     bool       `yaml:"enabled"`
	MinCapacity int32      `yaml:"minCapacity,omitempty"`
	MaxCapacity int32      `yaml:"maxCapacity,omitempty"`
	Policy      *policyDoc `yaml:"policy,omitempty"`
}

type policyDoc struct {
	Type     string          `yaml:"type"` // "buffer" | "schedule" | "webhook" | "counter" | "chain"
	Buffer   *bufferDoc      `yaml:"buffer,omitempty"`
	Schedule []scheduleDoc   `yaml:"schedule,omitempty"`
	Webhook  *webhookDoc     `yaml:"webhook,omitempty"`
	Counter  *counterDoc     `yaml:"counter,omitempty"`
	Chain    []chainEntryDoc `yaml:"chain,omitempty"`
}

type bufferDoc struct {
	BufferSize    int32 `yaml:"bufferSize,omitempty"`
	BufferPercent int32 `yaml:"bufferPercent,omitempty"`
}

type scheduleDoc struct {
	Cron         string `yaml:"cron"`
	DesiredCount int32  `yaml:"desiredCount"`
}

type webhookDoc struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

// counterDoc mirrors arenav1.CounterPolicy: keep a Counter's
// aggregate available capacity (capacity - count) above bufferSize (or
// bufferPercent of aggregate capacity).
type counterDoc struct {
	Key           string `yaml:"key"`
	BufferSize    int64  `yaml:"bufferSize,omitempty"`
	BufferPercent int32  `yaml:"bufferPercent,omitempty"`
}

// chainEntryDoc mirrors arenav1.ChainEntry: entries are
// tried in order, the first whose schedule window is active (or with no
// schedule at all — "always") wins. A chain policy nested inside another
// chain entry is rejected by the API layer's validation.
type chainEntryDoc struct {
	Schedule *chainScheduleDoc `yaml:"schedule,omitempty"`
	Policy   *policyDoc        `yaml:"policy"`
}

type chainScheduleDoc struct {
	Cron            string `yaml:"cron"`
	DurationSeconds int64  `yaml:"durationSeconds,omitempty"`
}

// ---------------------------------------------------------------------------
// Decode

// envPattern matches ${VAR} only — bare $VAR is left alone, and structural
// templating stays in external tools.
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv substitutes ${VAR} references from the environment. Unset
// variables are an error (fail fast instead of applying a broken manifest).
func ExpandEnv(data []byte) ([]byte, error) {
	var missing []string
	out := envPattern.ReplaceAllFunc(data, func(m []byte) []byte {
		name := string(envPattern.FindSubmatch(m)[1])
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return m
		}
		return []byte(v)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("undefined environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Decode parses one YAML stream (possibly multi-document) into Manifests.
// Unknown fields are errors, so a typo never silently becomes a no-op. The
// management marker is attached to every decoded manifest.
func Decode(data []byte, source string) ([]Manifest, error) {
	expanded, err := ExpandEnv(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}

	var out []Manifest
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	for docIndex := 0; ; docIndex++ {
		// Two-step decode: pull the raw node to skip empty documents, then
		// re-decode strictly (KnownFields is a Decoder-only option).
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("%s (doc %d): %w", source, docIndex, err)
		}
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return nil, fmt.Errorf("%s (doc %d): %w", source, docIndex, err)
		}
		strict := yaml.NewDecoder(bytes.NewReader(raw))
		strict.KnownFields(true)
		var doc fleetDoc
		if err := strict.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				continue // empty document between separators
			}
			return nil, fmt.Errorf("%s (doc %d): %w", source, docIndex, err)
		}

		m, err := doc.toManifest(fmt.Sprintf("%s (doc %d)", source, docIndex))
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
}

func (d *fleetDoc) toManifest(source string) (*Manifest, error) {
	if d.Name == "" {
		return nil, fmt.Errorf("%s: name is required", source)
	}
	spec, err := d.toSpec(source)
	if err != nil {
		return nil, err
	}

	labels := d.Tags
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ManagedByLabel] = ManagedByValue

	return &Manifest{
		Namespace: d.Namespace,
		Name:      d.Name,
		Labels:    labels,
		Spec:      spec,
		Source:    source,
	}, nil
}

func (d *fleetDoc) toSpec(source string) (*arenav1.FleetSpec, error) {
	spec := &arenav1.FleetSpec{Replicas: d.DesiredCount}

	switch strings.ToLower(d.Scheduling) {
	case "":
	case "packed":
		spec.Scheduling = arenav1.FleetSpec_SCHEDULING_PACKED
	case "distributed":
		spec.Scheduling = arenav1.FleetSpec_SCHEDULING_DISTRIBUTED
	default:
		return nil, fmt.Errorf("%s: scheduling %q (want packed or distributed)", source, d.Scheduling)
	}

	if td := d.TaskDefinition; td != nil {
		tmpl, err := td.toTemplate(source)
		if err != nil {
			return nil, err
		}
		spec.Template = tmpl
	}

	if as := d.AutoScaling; as != nil {
		conv, err := as.toProto(source)
		if err != nil {
			return nil, err
		}
		spec.Autoscaling = conv
	}

	if s := d.Strategy; s != nil {
		strat, err := s.toProto(source)
		if err != nil {
			return nil, err
		}
		spec.Strategy = strat
	}
	if o := d.AllocationOverflow; o != nil {
		spec.AllocationOverflow = &arenav1.AllocationOverflow{Labels: o.Labels, Annotations: o.Annotations}
	}
	if c := d.Capacity; c != nil {
		spec.Capacity = c.toProto()
	}
	if n := d.Network; n != nil {
		spec.Network = n.toProto()
	}
	spec.DrainGraceSeconds = d.DrainGraceSeconds
	return spec, nil
}

func (s *strategyDoc) toProto(source string) (*arenav1.Strategy, error) {
	out := &arenav1.Strategy{}
	switch strings.ToLower(s.Type) {
	case "", "rollingupdate":
		out.Type = arenav1.Strategy_TYPE_ROLLING_UPDATE
	case "recreate":
		out.Type = arenav1.Strategy_TYPE_RECREATE
	default:
		return nil, fmt.Errorf("%s: strategy type %q (want rollingUpdate or recreate)", source, s.Type)
	}
	if ru := s.RollingUpdate; ru != nil {
		out.RollingUpdate = &arenav1.RollingUpdate{
			MaxSurge: ru.MaxSurge, MaxUnavailable: ru.MaxUnavailable, DrainTimeoutSeconds: ru.DrainTimeoutSeconds,
		}
	}
	return out, nil
}

func (c *capacityDoc) toProto() *arenav1.Capacity {
	out := &arenav1.Capacity{}
	for _, p := range c.Providers {
		out.Providers = append(out.Providers, &arenav1.CapacityProvider{
			Name: p.Name, Weight: p.Weight, Base: p.Base,
		})
	}
	return out
}

func (n *networkDoc) toProto() *arenav1.Network {
	return &arenav1.Network{
		AssignPublicIp: n.AssignPublicIP,
		SecurityGroups: n.SecurityGroups,
		Subnets:        n.Subnets,
	}
}

// toTemplate converts the taskDefinition into a GameServerTemplate. A single
// containerDefinitions entry uses the "container" sugar field (unchanged
// behavior); more than one uses "containers" + gameContainer. Ports and
// the SDK heartbeat spec are fleet-spec-level concepts and
// are hoisted from the game container only — other containers can't declare
// them.
func (td *taskDefinitionDoc) toTemplate(source string) (*arenav1.GameServerTemplate, error) {
	if len(td.ContainerDefinitions) == 0 {
		return nil, fmt.Errorf("%s: taskDefinition needs at least one containerDefinition", source)
	}

	containers := make([]*arenav1.ContainerSpec, len(td.ContainerDefinitions))
	for i, c := range td.ContainerDefinitions {
		cs, err := c.toProto(fmt.Sprintf("%s.containerDefinitions[%d]", source, i))
		if err != nil {
			return nil, err
		}
		containers[i] = cs
	}

	gsSpec := &arenav1.GameServerSpec{}
	gameIdx := 0
	if len(containers) == 1 {
		gsSpec.Container = containers[0]
	} else {
		gsSpec.Containers = containers
		gsSpec.GameContainer = td.GameContainer
		if td.GameContainer == "" {
			return nil, fmt.Errorf("%s: taskDefinition.gameContainer is required with more than one containerDefinition", source)
		}
		gameIdx = -1
		for i, c := range td.ContainerDefinitions {
			if c.Name == td.GameContainer {
				gameIdx = i
				break
			}
		}
		if gameIdx < 0 {
			return nil, fmt.Errorf("%s: taskDefinition.gameContainer %q does not match any containerDefinition", source, td.GameContainer)
		}
	}
	gameDoc := td.ContainerDefinitions[gameIdx]

	if td.CPU != "" || td.Memory != "" {
		if gameDoc.Resources != nil {
			return nil, fmt.Errorf("%s: game container %q: set either taskDefinition.cpu/memory or its own resources, not both", source, gameDoc.Name)
		}
		containers[gameIdx].Resources = &arenav1.Resources{Cpu: td.CPU, Memory: td.Memory}
	}

	for _, p := range gameDoc.PortMappings {
		proto := arenav1.Port_PROTOCOL_UDP
		switch strings.ToLower(p.Protocol) {
		case "", "udp":
		case "tcp":
			proto = arenav1.Port_PROTOCOL_TCP
		default:
			return nil, fmt.Errorf("%s: portMappings protocol %q (want udp or tcp)", source, p.Protocol)
		}
		gsSpec.Ports = append(gsSpec.Ports, &arenav1.PortSpec{
			Name:          p.Name,
			ContainerPort: p.ContainerPort,
			Protocol:      proto,
		})
	}
	if hc := gameDoc.HealthCheck; hc != nil {
		gsSpec.Health = &arenav1.HealthSpec{
			InitialDelaySeconds: hc.StartPeriod,
			PeriodSeconds:       hc.Interval,
			FailureThreshold:    hc.Retries,
		}
	}
	for i, c := range td.ContainerDefinitions {
		if i == gameIdx {
			continue
		}
		if len(c.PortMappings) > 0 {
			return nil, fmt.Errorf("%s: containerDefinitions[%d] (%s): portMappings only supported on the game container (%s)", source, i, c.Name, gameDoc.Name)
		}
		if c.HealthCheck != nil {
			return nil, fmt.Errorf("%s: containerDefinitions[%d] (%s): healthCheck only supported on the game container (%s)", source, i, c.Name, gameDoc.Name)
		}
	}

	for _, v := range td.Volumes {
		gsSpec.Volumes = append(gsSpec.Volumes, v.toProto())
	}

	tmpl := &arenav1.GameServerTemplate{Spec: gsSpec}
	if len(td.Tags) > 0 || len(td.Annotations) > 0 {
		tmpl.Metadata = &arenav1.TemplateMetadata{Labels: td.Tags, Annotations: td.Annotations}
	}
	return tmpl, nil
}

func (c *containerDoc) toProto(source string) (*arenav1.ContainerSpec, error) {
	cs := &arenav1.ContainerSpec{
		Name:             c.Name,
		Image:            c.Image,
		Command:          c.Command,
		Args:             c.Args,
		WorkingDirectory: c.WorkingDirectory,
	}
	for _, e := range c.Environment {
		cs.Environment = append(cs.Environment, &arenav1.EnvVar{Name: e.Name, Value: e.Value})
	}
	if c.Resources != nil {
		cs.Resources = &arenav1.Resources{Cpu: c.Resources.CPU, Memory: c.Resources.Memory}
	}
	for _, s := range c.Secrets {
		if s.Name == "" || s.ValueFrom == "" {
			return nil, fmt.Errorf("%s: secrets need name and valueFrom", source)
		}
		cs.Secrets = append(cs.Secrets, &arenav1.SecretRef{Name: s.Name, ValueFrom: s.ValueFrom})
	}
	if hc := c.ContainerHealthCheck; hc != nil {
		cs.HealthCheck = &arenav1.ContainerHealthCheck{
			Command:            hc.Command,
			IntervalSeconds:    hc.IntervalSeconds,
			TimeoutSeconds:     hc.TimeoutSeconds,
			Retries:            hc.Retries,
			StartPeriodSeconds: hc.StartPeriodSeconds,
		}
	}
	for _, m := range c.MountPoints {
		cs.MountPoints = append(cs.MountPoints, &arenav1.MountPoint{
			Volume: m.Volume, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly,
		})
	}
	return cs, nil
}

func (v *volumeDoc) toProto() *arenav1.VolumeSpec {
	out := &arenav1.VolumeSpec{Name: v.Name}
	if v.EFS != nil {
		out.Efs = &arenav1.EFSVolume{FileSystemId: v.EFS.FileSystemID, RootDirectory: v.EFS.RootDirectory}
	}
	if v.Host != nil {
		out.Host = &arenav1.HostVolume{SourcePath: v.Host.SourcePath}
	}
	return out
}

func (as *autoScalingDoc) toProto(source string) (*arenav1.Autoscaling, error) {
	out := &arenav1.Autoscaling{
		Enabled:     as.Enabled,
		MinReplicas: as.MinCapacity,
		MaxReplicas: as.MaxCapacity,
	}
	if as.Policy == nil {
		return out, nil
	}
	policy, err := as.Policy.toProto(source)
	if err != nil {
		return nil, err
	}
	out.Policy = policy
	return out, nil
}

// toProto converts one policyDoc, recursing into chain entries (each
// carries its own nested policyDoc).
func (p *policyDoc) toProto(source string) (*arenav1.AutoscalingPolicy, error) {
	policy := &arenav1.AutoscalingPolicy{}
	switch strings.ToLower(p.Type) {
	case "buffer":
		policy.Type = arenav1.AutoscalingPolicy_TYPE_BUFFER
	case "schedule":
		policy.Type = arenav1.AutoscalingPolicy_TYPE_SCHEDULE
	case "webhook":
		policy.Type = arenav1.AutoscalingPolicy_TYPE_WEBHOOK
	case "counter":
		policy.Type = arenav1.AutoscalingPolicy_TYPE_COUNTER
	case "chain":
		policy.Type = arenav1.AutoscalingPolicy_TYPE_CHAIN
	default:
		return nil, fmt.Errorf("%s: autoScaling policy type %q (want buffer, schedule, webhook, counter or chain)", source, p.Type)
	}
	if p.Buffer != nil {
		policy.Buffer = &arenav1.BufferPolicy{
			BufferSize:    p.Buffer.BufferSize,
			BufferPercent: p.Buffer.BufferPercent,
		}
	}
	for _, s := range p.Schedule {
		policy.Schedule = append(policy.Schedule, &arenav1.SchedulePolicy{
			Cron: s.Cron, Replicas: s.DesiredCount,
		})
	}
	if p.Webhook != nil {
		policy.Webhook = &arenav1.WebhookPolicy{Url: p.Webhook.URL, Headers: p.Webhook.Headers}
	}
	if p.Counter != nil {
		policy.Counter = &arenav1.CounterPolicy{
			Key: p.Counter.Key, BufferSize: p.Counter.BufferSize, BufferPercent: p.Counter.BufferPercent,
		}
	}
	for i, ce := range p.Chain {
		if ce.Policy == nil {
			return nil, fmt.Errorf("%s: chain[%d].policy is required", source, i)
		}
		entryPolicy, err := ce.Policy.toProto(fmt.Sprintf("%s.chain[%d]", source, i))
		if err != nil {
			return nil, err
		}
		entry := &arenav1.ChainEntry{Policy: entryPolicy}
		if ce.Schedule != nil {
			entry.Schedule = &arenav1.ChainSchedule{Cron: ce.Schedule.Cron, DurationSeconds: ce.Schedule.DurationSeconds}
		}
		policy.Chain = append(policy.Chain, entry)
	}
	return policy, nil
}

// ---------------------------------------------------------------------------
// Encode (arenactl get: export the current state as an applyable definition)

// Encode renders a Fleet as a fleet definition YAML. Server-owned fields
// (status, version, id, generation) never appear, and desiredCount is
// dropped while the autoscaler owns it — the export must round-trip through
// apply without being rejected.
func Encode(f *arenav1.Fleet) ([]byte, error) {
	spec := f.GetSpec()
	doc := fleetDoc{
		Name:      f.GetName(),
		Namespace: f.GetNamespace(),
		Tags:      f.GetLabels(),
	}
	if spec.Replicas != nil && !spec.GetAutoscaling().GetEnabled() {
		doc.DesiredCount = spec.Replicas
	}
	switch spec.GetScheduling() {
	case arenav1.FleetSpec_SCHEDULING_PACKED:
		doc.Scheduling = "packed"
	case arenav1.FleetSpec_SCHEDULING_DISTRIBUTED:
		doc.Scheduling = "distributed"
	}

	if tmpl := spec.GetTemplate(); tmpl != nil {
		td := &taskDefinitionDoc{
			Tags:        tmpl.GetMetadata().GetLabels(),
			Annotations: tmpl.GetMetadata().GetAnnotations(),
		}

		gsSpec := tmpl.GetSpec()
		gameIdx := 0
		if len(gsSpec.GetContainers()) > 0 {
			for i, cs := range gsSpec.GetContainers() {
				td.ContainerDefinitions = append(td.ContainerDefinitions, containerToDoc(cs))
				if cs.GetName() == gsSpec.GetGameContainer() {
					gameIdx = i
				}
			}
			td.GameContainer = gsSpec.GetGameContainer()
		} else {
			cd := containerToDoc(gsSpec.GetContainer())
			if cd.Name == "" {
				cd.Name = "gameserver"
			}
			td.ContainerDefinitions = []containerDoc{cd}
		}

		// The game container's sizing/ports/health are hoisted to the
		// task-level / fleet-spec-level fields below (see toTemplate); its
		// own resources block is redundant and dropped from the export.
		td.CPU = td.ContainerDefinitions[gameIdx].Resources.getCPU()
		td.Memory = td.ContainerDefinitions[gameIdx].Resources.getMemory()
		td.ContainerDefinitions[gameIdx].Resources = nil

		for _, p := range gsSpec.GetPorts() {
			proto := "udp"
			if p.GetProtocol() == arenav1.Port_PROTOCOL_TCP {
				proto = "tcp"
			}
			td.ContainerDefinitions[gameIdx].PortMappings = append(td.ContainerDefinitions[gameIdx].PortMappings,
				portDoc{Name: p.GetName(), ContainerPort: p.GetContainerPort(), Protocol: proto})
		}
		if h := gsSpec.GetHealth(); h != nil {
			td.ContainerDefinitions[gameIdx].HealthCheck = &healthDoc{
				StartPeriod: h.GetInitialDelaySeconds(), Interval: h.GetPeriodSeconds(), Retries: h.GetFailureThreshold(),
			}
		}
		for _, v := range gsSpec.GetVolumes() {
			vd := volumeDoc{Name: v.GetName()}
			if e := v.GetEfs(); e != nil {
				vd.EFS = &efsDoc{FileSystemID: e.GetFileSystemId(), RootDirectory: e.GetRootDirectory()}
			}
			if h := v.GetHost(); h != nil {
				vd.Host = &hostDoc{SourcePath: h.GetSourcePath()}
			}
			td.Volumes = append(td.Volumes, vd)
		}
		doc.TaskDefinition = td
	}

	if as := spec.GetAutoscaling(); as != nil {
		asDoc := &autoScalingDoc{
			Enabled:     as.GetEnabled(),
			MinCapacity: as.GetMinReplicas(),
			MaxCapacity: as.GetMaxReplicas(),
		}
		if p := as.GetPolicy(); p != nil {
			asDoc.Policy = policyToDoc(p)
		}
		doc.AutoScaling = asDoc
	}

	if strat := spec.GetStrategy(); strat != nil {
		sd := &strategyDoc{}
		switch strat.GetType() {
		case arenav1.Strategy_TYPE_RECREATE:
			sd.Type = "recreate"
		default:
			sd.Type = "rollingUpdate"
		}
		if ru := strat.GetRollingUpdate(); ru != nil {
			sd.RollingUpdate = &rollingUpdateDoc{
				MaxSurge: ru.GetMaxSurge(), MaxUnavailable: ru.GetMaxUnavailable(), DrainTimeoutSeconds: ru.GetDrainTimeoutSeconds(),
			}
		}
		doc.Strategy = sd
	}
	if o := spec.GetAllocationOverflow(); o != nil {
		doc.AllocationOverflow = &allocationOverflowDoc{Labels: o.GetLabels(), Annotations: o.GetAnnotations()}
	}
	if c := spec.GetCapacity(); c != nil {
		cd := &capacityDoc{}
		for _, p := range c.GetProviders() {
			cd.Providers = append(cd.Providers, capacityProviderDoc{Name: p.GetName(), Weight: p.GetWeight(), Base: p.GetBase()})
		}
		doc.Capacity = cd
	}
	if n := spec.GetNetwork(); n != nil {
		doc.Network = &networkDoc{AssignPublicIP: n.AssignPublicIp, SecurityGroups: n.GetSecurityGroups(), Subnets: n.GetSubnets()}
	}
	doc.DrainGraceSeconds = spec.GetDrainGraceSeconds()

	return yaml.Marshal(&doc)
}

// containerToDoc converts one ContainerSpec, independent of whether it ends
// up as the single-container form or a containers[] entry.
func containerToDoc(cs *arenav1.ContainerSpec) containerDoc {
	c := containerDoc{
		Name:             cs.GetName(),
		Image:            cs.GetImage(),
		Command:          cs.GetCommand(),
		Args:             cs.GetArgs(),
		WorkingDirectory: cs.GetWorkingDirectory(),
	}
	for _, e := range cs.GetEnvironment() {
		c.Environment = append(c.Environment, envVarDoc{Name: e.GetName(), Value: e.GetValue()})
	}
	if r := cs.GetResources(); r != nil {
		c.Resources = &resourcesDoc{CPU: r.GetCpu(), Memory: r.GetMemory()}
	}
	for _, s := range cs.GetSecrets() {
		c.Secrets = append(c.Secrets, secretDoc{Name: s.GetName(), ValueFrom: s.GetValueFrom()})
	}
	if hc := cs.GetHealthCheck(); hc != nil {
		c.ContainerHealthCheck = &containerHealthCheckDoc{
			Command: hc.GetCommand(), IntervalSeconds: hc.GetIntervalSeconds(), TimeoutSeconds: hc.GetTimeoutSeconds(),
			Retries: hc.GetRetries(), StartPeriodSeconds: hc.GetStartPeriodSeconds(),
		}
	}
	for _, m := range cs.GetMountPoints() {
		c.MountPoints = append(c.MountPoints, mountPointDoc{Volume: m.GetVolume(), ContainerPath: m.GetContainerPath(), ReadOnly: m.GetReadOnly()})
	}
	return c
}

// policyToDoc mirrors (*policyDoc).toProto, recursing into chain entries.
func policyToDoc(p *arenav1.AutoscalingPolicy) *policyDoc {
	pd := &policyDoc{}
	switch p.GetType() {
	case arenav1.AutoscalingPolicy_TYPE_BUFFER:
		pd.Type = "buffer"
	case arenav1.AutoscalingPolicy_TYPE_SCHEDULE:
		pd.Type = "schedule"
	case arenav1.AutoscalingPolicy_TYPE_WEBHOOK:
		pd.Type = "webhook"
	case arenav1.AutoscalingPolicy_TYPE_COUNTER:
		pd.Type = "counter"
	case arenav1.AutoscalingPolicy_TYPE_CHAIN:
		pd.Type = "chain"
	}
	if b := p.GetBuffer(); b != nil {
		pd.Buffer = &bufferDoc{BufferSize: b.GetBufferSize(), BufferPercent: b.GetBufferPercent()}
	}
	for _, s := range p.GetSchedule() {
		pd.Schedule = append(pd.Schedule, scheduleDoc{Cron: s.GetCron(), DesiredCount: s.GetReplicas()})
	}
	if w := p.GetWebhook(); w != nil {
		pd.Webhook = &webhookDoc{URL: w.GetUrl(), Headers: w.GetHeaders()}
	}
	if c := p.GetCounter(); c != nil {
		pd.Counter = &counterDoc{Key: c.GetKey(), BufferSize: c.GetBufferSize(), BufferPercent: c.GetBufferPercent()}
	}
	for _, ce := range p.GetChain() {
		entry := chainEntryDoc{Policy: policyToDoc(ce.GetPolicy())}
		if s := ce.GetSchedule(); s != nil {
			entry.Schedule = &chainScheduleDoc{Cron: s.GetCron(), DurationSeconds: s.GetDurationSeconds()}
		}
		pd.Chain = append(pd.Chain, entry)
	}
	return pd
}

// ---------------------------------------------------------------------------
// Load

// Load reads manifests from files and directories (recursive, .yaml/.yml).
func Load(paths []string) ([]Manifest, error) {
	var out []Manifest
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			ms, err := loadFile(p)
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			ms, err := loadFile(path)
			if err != nil {
				return err
			}
			out = append(out, ms...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no manifests found")
	}
	return out, nil
}

func loadFile(path string) ([]Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data, path)
}
