package ecs

// Tests for capacity providers, network overrides,
// drain stopTimeout, and the extended container spec surface.

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/moepig/arena/internal/store"
)

type fakeECS struct {
	runInputs []*awsecs.RunTaskInput
	regInputs []*awsecs.RegisterTaskDefinitionInput
}

func (f *fakeECS) RunTask(_ context.Context, in *awsecs.RunTaskInput, _ ...func(*awsecs.Options)) (*awsecs.RunTaskOutput, error) {
	f.runInputs = append(f.runInputs, in)
	return &awsecs.RunTaskOutput{Tasks: []ecstypes.Task{{TaskArn: aws.String("arn:task/1")}}}, nil
}

func (f *fakeECS) StopTask(context.Context, *awsecs.StopTaskInput, ...func(*awsecs.Options)) (*awsecs.StopTaskOutput, error) {
	return &awsecs.StopTaskOutput{}, nil
}

func (f *fakeECS) RegisterTaskDefinition(_ context.Context, in *awsecs.RegisterTaskDefinitionInput, _ ...func(*awsecs.Options)) (*awsecs.RegisterTaskDefinitionOutput, error) {
	f.regInputs = append(f.regInputs, in)
	return &awsecs.RegisterTaskDefinitionOutput{
		TaskDefinition: &ecstypes.TaskDefinition{TaskDefinitionArn: aws.String("arn:taskdef/1")},
	}, nil
}

func testFleet(templateJSON string) *store.Fleet {
	return &store.Fleet{
		ID: "f1", Name: "f1", SpecHash: "h1",
		TemplateJSON: templateJSON,
	}
}

func TestLaunchExtendedContainerSpec(t *testing.T) {
	api := &fakeECS{}
	l := NewLauncher(api, Config{Cluster: "c", Subnets: []string{"sub-1"}, SecurityGroups: []string{"sg-1"}})
	fleet := testFleet(`{
	  "spec": {
	    "containers": [
	      {"name": "game", "image": "img:1",
	       "command": ["/srv/game"], "args": ["-mode", "ranked"],
	       "workingDirectory": "/srv",
	       "secrets": [{"name": "API_KEY", "valueFrom": "arn:aws:ssm:key"}],
	       "healthCheck": {"command": ["CMD-SHELL", "true"], "intervalSeconds": 15},
	       "mountPoints": [{"volume": "save", "containerPath": "/save"}]},
	      {"name": "metrics", "image": "img:agent", "resources": {"cpu": "64", "memory": "128"}}
	    ],
	    "gameContainer": "game",
	    "volumes": [{"name": "save", "efs": {"fileSystemId": "fs-1"}}],
	    "ports": [{"name": "game", "containerPort": 7777}]
	  }
	}`)
	fleet.DrainGraceSeconds = 90

	if _, err := l.Launch(context.Background(), fleet, "gs-1"); err != nil {
		t.Fatal(err)
	}
	if len(api.regInputs) != 1 {
		t.Fatalf("registered %d task defs, want 1", len(api.regInputs))
	}
	reg := api.regInputs[0]
	if len(reg.ContainerDefinitions) != 3 {
		t.Fatalf("containers = %d, want game + metrics + sidecar", len(reg.ContainerDefinitions))
	}
	game := reg.ContainerDefinitions[0]
	if aws.ToString(game.Name) != "game" || !aws.ToBool(game.Essential) {
		t.Errorf("game container = %v/%v, want essential 'game'", game.Name, game.Essential)
	}
	if got := game.EntryPoint; len(got) != 1 || got[0] != "/srv/game" {
		t.Errorf("entrypoint = %v", got)
	}
	if got := game.Command; len(got) != 2 || got[0] != "-mode" {
		t.Errorf("command(args) = %v", got)
	}
	if aws.ToString(game.WorkingDirectory) != "/srv" {
		t.Errorf("working dir = %v", game.WorkingDirectory)
	}
	if len(game.Secrets) != 1 || aws.ToString(game.Secrets[0].ValueFrom) != "arn:aws:ssm:key" {
		t.Errorf("secrets = %v", game.Secrets)
	}
	if game.HealthCheck == nil || aws.ToInt32(game.HealthCheck.Interval) != 15 {
		t.Errorf("health check = %v", game.HealthCheck)
	}
	if aws.ToInt32(game.StopTimeout) != 90 {
		t.Errorf("stop timeout = %v, want drain_grace_seconds 90", game.StopTimeout)
	}
	if len(game.MountPoints) != 1 || aws.ToString(game.MountPoints[0].SourceVolume) != "save" {
		t.Errorf("mount points = %v", game.MountPoints)
	}
	// ARENA_PORT_* contract.
	foundPortEnv := false
	for _, e := range game.Environment {
		if aws.ToString(e.Name) == "ARENA_PORT_GAME" && aws.ToString(e.Value) == "7777" {
			foundPortEnv = true
		}
	}
	if !foundPortEnv {
		t.Error("game container missing ARENA_PORT_GAME=7777")
	}
	aux := reg.ContainerDefinitions[1]
	if aws.ToBool(aux.Essential) {
		t.Error("auxiliary container must not be essential")
	}
	if len(reg.Volumes) != 1 || reg.Volumes[0].EfsVolumeConfiguration == nil {
		t.Errorf("volumes = %v, want one EFS volume", reg.Volumes)
	}
}

func TestLaunchCapacityAndNetworkOverrides(t *testing.T) {
	api := &fakeECS{}
	l := NewLauncher(api, Config{Cluster: "c", Subnets: []string{"sub-default"}, AssignPublicIP: true})
	fleet := testFleet(`{"spec": {"container": {"image": "img:1"}}}`)
	fleet.CapacityJSON = `{"providers": [{"name": "FARGATE_SPOT", "weight": 4}, {"name": "FARGATE", "weight": 1, "base": 2}]}`
	fleet.NetworkJSON = `{"assignPublicIp": false, "subnets": ["sub-a"], "securityGroups": ["sg-a"]}`

	if _, err := l.Launch(context.Background(), fleet, "gs-1"); err != nil {
		t.Fatal(err)
	}
	run := api.runInputs[0]
	if run.LaunchType != "" {
		t.Errorf("launch type = %q, want capacity provider strategy instead", run.LaunchType)
	}
	if len(run.CapacityProviderStrategy) != 2 || aws.ToString(run.CapacityProviderStrategy[0].CapacityProvider) != "FARGATE_SPOT" {
		t.Errorf("capacity strategy = %v", run.CapacityProviderStrategy)
	}
	vpc := run.NetworkConfiguration.AwsvpcConfiguration
	if vpc.AssignPublicIp != ecstypes.AssignPublicIpDisabled {
		t.Errorf("assign public ip = %v, want fleet override (disabled)", vpc.AssignPublicIp)
	}
	if len(vpc.Subnets) != 1 || vpc.Subnets[0] != "sub-a" {
		t.Errorf("subnets = %v, want fleet override", vpc.Subnets)
	}
}

func TestLaunchDefaultsWithoutOverrides(t *testing.T) {
	api := &fakeECS{}
	l := NewLauncher(api, Config{Cluster: "c", Subnets: []string{"sub-default"}, AssignPublicIP: true})
	if _, err := l.Launch(context.Background(), testFleet(`{"spec": {"container": {"image": "img:1"}}}`), "gs-1"); err != nil {
		t.Fatal(err)
	}
	run := api.runInputs[0]
	if run.LaunchType != ecstypes.LaunchTypeFargate {
		t.Errorf("launch type = %q, want FARGATE", run.LaunchType)
	}
	vpc := run.NetworkConfiguration.AwsvpcConfiguration
	if vpc.AssignPublicIp != ecstypes.AssignPublicIpEnabled || vpc.Subnets[0] != "sub-default" {
		t.Errorf("network = %v, want controller defaults", vpc)
	}
	// Single-container sugar becomes "gameserver" + sidecar.
	reg := api.regInputs[0]
	if len(reg.ContainerDefinitions) != 2 || aws.ToString(reg.ContainerDefinitions[0].Name) != "gameserver" {
		t.Errorf("containers = %v", reg.ContainerDefinitions)
	}
}
