package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
)

const sampleManifest = `
name: shooter-jp
namespace: default
tags:
  game: shooter
scheduling: distributed
taskDefinition:
  cpu: "1024"
  memory: "2048"
  tags:
    version: ${IMAGE_TAG}
  containerDefinitions:
    - name: gameserver
      image: myregistry/gameserver:${IMAGE_TAG}
      environment:
        - name: MODE
          value: battle
      portMappings:
        - name: game
          containerPort: 7777
          protocol: udp
      healthCheck:
        startPeriod: 30
        interval: 10
        retries: 3
autoScaling:
  enabled: true
  minCapacity: 2
  maxCapacity: 20
  policy:
    type: buffer
    buffer:
      bufferSize: 3
`

func TestDecode(t *testing.T) {
	t.Setenv("IMAGE_TAG", "v1.2.3")
	ms, err := Decode([]byte(sampleManifest), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("decoded %d manifests, want 1", len(ms))
	}
	m := ms[0]
	if m.Namespace != "default" || m.Name != "shooter-jp" {
		t.Errorf("identity = %s/%s", m.Namespace, m.Name)
	}
	if m.Labels["game"] != "shooter" {
		t.Errorf("labels = %v", m.Labels)
	}
	if m.Labels[ManagedByLabel] != ManagedByValue {
		t.Error("management marker not attached")
	}
	if m.Spec.GetScheduling() != arenav1.FleetSpec_SCHEDULING_DISTRIBUTED {
		t.Errorf("scheduling = %v", m.Spec.GetScheduling())
	}
	if m.Spec.Replicas != nil {
		t.Error("desiredCount must stay unset when omitted (ownership rules)")
	}

	c := m.Spec.GetTemplate().GetSpec().GetContainer()
	if c.GetImage() != "myregistry/gameserver:v1.2.3" {
		t.Errorf("image = %q (env expansion)", c.GetImage())
	}
	if c.GetResources().GetCpu() != "1024" || c.GetResources().GetMemory() != "2048" {
		t.Errorf("resources = %v", c.GetResources())
	}
	if len(c.GetEnvironment()) != 1 || c.GetEnvironment()[0].GetName() != "MODE" {
		t.Errorf("environment = %v", c.GetEnvironment())
	}
	if got := m.Spec.GetTemplate().GetMetadata().GetLabels()["version"]; got != "v1.2.3" {
		t.Errorf("task definition tags → gameserver labels: %q", got)
	}

	ports := m.Spec.GetTemplate().GetSpec().GetPorts()
	if len(ports) != 1 || ports[0].GetContainerPort() != 7777 || ports[0].GetProtocol() != arenav1.Port_PROTOCOL_UDP {
		t.Errorf("ports = %v", ports)
	}
	h := m.Spec.GetTemplate().GetSpec().GetHealth()
	if h.GetInitialDelaySeconds() != 30 || h.GetPeriodSeconds() != 10 || h.GetFailureThreshold() != 3 {
		t.Errorf("healthCheck mapping = %v", h)
	}

	as := m.Spec.GetAutoscaling()
	if !as.GetEnabled() || as.GetMinReplicas() != 2 || as.GetMaxReplicas() != 20 {
		t.Errorf("autoScaling = %v", as)
	}
	if as.GetPolicy().GetType() != arenav1.AutoscalingPolicy_TYPE_BUFFER || as.GetPolicy().GetBuffer().GetBufferSize() != 3 {
		t.Errorf("policy = %v", as.GetPolicy())
	}
}

func TestDecodeDesiredCount(t *testing.T) {
	doc := `
name: fixed
desiredCount: 5
taskDefinition:
  containerDefinitions:
    - { name: gameserver, image: img:v1 }
`
	ms, err := Decode([]byte(doc), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].Spec.GetReplicas() != 5 {
		t.Errorf("replicas = %d, want 5", ms[0].Spec.GetReplicas())
	}
	// Explicit zero is preserved (scale to zero), distinct from omitted.
	ms, err = Decode([]byte(strings.Replace(doc, "desiredCount: 5", "desiredCount: 0", 1)), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].Spec.Replicas == nil || *ms[0].Spec.Replicas != 0 {
		t.Errorf("explicit desiredCount: 0 lost (replicas=%v)", ms[0].Spec.Replicas)
	}
}

func TestDecodeMultiDocument(t *testing.T) {
	doc := `
name: a
---
name: b
---
`
	ms, err := Decode([]byte(doc), "multi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Name != "a" || ms[1].Name != "b" {
		t.Fatalf("decoded %+v", ms)
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := map[string]string{
		"missing name":             "namespace: default",
		"unknown field":            "name: x\ndesiredcount: 3", // wrong case = unknown
		"k8s-style doc":            "apiVersion: arena/v1\nkind: Fleet\nmetadata: {name: x}",
		"bad scheduling":           "name: x\nscheduling: binpack",
		"bad protocol":             "name: x\ntaskDefinition: {containerDefinitions: [{name: g, image: i, portMappings: [{name: p, containerPort: 1, protocol: sctp}]}]}",
		"multi container no game":  "name: x\ntaskDefinition: {containerDefinitions: [{name: a, image: i}, {name: b, image: i}]}",
		"multi container bad game": "name: x\ntaskDefinition: {containerDefinitions: [{name: a, image: i}, {name: b, image: i}], gameContainer: c}",
		"zero containers":          "name: x\ntaskDefinition: {containerDefinitions: []}",
		"sidecar container ports":  "name: x\ntaskDefinition: {containerDefinitions: [{name: a, image: i}, {name: b, image: i, portMappings: [{name: p, containerPort: 1}]}], gameContainer: a}",
		"sidecar container health": "name: x\ntaskDefinition: {containerDefinitions: [{name: a, image: i}, {name: b, image: i, healthCheck: {interval: 5}}], gameContainer: a}",
		"bad policy type":          "name: x\nautoScaling: {enabled: true, policy: {type: cpu}}",
	}
	for label, doc := range cases {
		if _, err := Decode([]byte(doc), "t.yaml"); err == nil {
			t.Errorf("%s: want error", label)
		}
	}
}

func TestExpandEnvUndefinedVariableFails(t *testing.T) {
	doc := "name: x\ntaskDefinition: {containerDefinitions: [{name: g, image: 'img:${UNSET_VAR_12345}'}]}"
	_, err := Decode([]byte(doc), "t.yaml")
	if err == nil || !strings.Contains(err.Error(), "UNSET_VAR_12345") {
		t.Fatalf("err = %v, want undefined-variable failure", err)
	}
}

// TestEncodeRoundTrip: `arenactl get` output must decode back to the same
// spec (modulo the autoscaler-owned desiredCount, which is dropped).
func TestEncodeRoundTrip(t *testing.T) {
	t.Setenv("IMAGE_TAG", "v1.2.3")
	ms, err := Decode([]byte(sampleManifest), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	orig := ms[0]

	out, err := Encode(&arenav1.Fleet{
		Name:      orig.Name,
		Namespace: orig.Namespace,
		Labels:    orig.Labels,
		Spec:      orig.Spec,
	})
	if err != nil {
		t.Fatal(err)
	}

	back, err := Decode(out, "exported.yaml")
	if err != nil {
		t.Fatalf("exported definition does not decode: %v\n%s", err, out)
	}
	if back[0].Name != orig.Name || back[0].Namespace != orig.Namespace {
		t.Errorf("identity changed: %s/%s", back[0].Namespace, back[0].Name)
	}
	if !proto.Equal(back[0].Spec, orig.Spec) {
		t.Errorf("spec did not round-trip:\noriginal: %v\nre-decoded: %v", orig.Spec, back[0].Spec)
	}
}

// TestDecodeCounterAndChainPolicies verifies the counter and chain
// autoscaler policy types round-trip through the YAML manifest, including a
// nested chain entry and webhook headers.
func TestDecodeCounterAndChainPolicies(t *testing.T) {
	const doc = `
name: rooms-fleet
autoScaling:
  enabled: true
  minCapacity: 1
  maxCapacity: 50
  policy:
    type: chain
    chain:
      - schedule: {cron: "0 18 * * *", durationSeconds: 21600}
        policy:
          type: webhook
          webhook: {url: "https://example.com/scale", headers: {Authorization: "Bearer x"}}
      - policy:
          type: counter
          counter: {key: rooms, bufferSize: 10}
`
	ms, err := Decode([]byte(doc), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy := ms[0].Spec.GetAutoscaling().GetPolicy()
	if policy.GetType() != arenav1.AutoscalingPolicy_TYPE_CHAIN {
		t.Fatalf("type = %v, want TYPE_CHAIN", policy.GetType())
	}
	if len(policy.GetChain()) != 2 {
		t.Fatalf("chain entries = %d, want 2", len(policy.GetChain()))
	}
	first := policy.GetChain()[0]
	if first.GetSchedule().GetCron() != "0 18 * * *" || first.GetSchedule().GetDurationSeconds() != 21600 {
		t.Errorf("first entry schedule = %+v", first.GetSchedule())
	}
	if first.GetPolicy().GetType() != arenav1.AutoscalingPolicy_TYPE_WEBHOOK {
		t.Errorf("first entry policy type = %v, want TYPE_WEBHOOK", first.GetPolicy().GetType())
	}
	if got := first.GetPolicy().GetWebhook(); got.GetUrl() != "https://example.com/scale" || got.GetHeaders()["Authorization"] != "Bearer x" {
		t.Errorf("webhook = %+v", got)
	}
	second := policy.GetChain()[1]
	if second.GetSchedule() != nil {
		t.Errorf("second entry schedule = %+v, want nil (always active)", second.GetSchedule())
	}
	if got := second.GetPolicy().GetCounter(); got.GetKey() != "rooms" || got.GetBufferSize() != 10 {
		t.Errorf("counter = %+v", got)
	}
}

// TestEncodeChainPolicyRoundTrip: `arenactl get` output for a Chain/Counter
// policy must decode back to the same spec.
func TestEncodeChainPolicyRoundTrip(t *testing.T) {
	orig := &arenav1.Autoscaling{
		Enabled: true, MinReplicas: 1, MaxReplicas: 10,
		Policy: &arenav1.AutoscalingPolicy{
			Type: arenav1.AutoscalingPolicy_TYPE_CHAIN,
			Chain: []*arenav1.ChainEntry{
				{
					Schedule: &arenav1.ChainSchedule{Cron: "0 18 * * *", DurationSeconds: 21600},
					Policy: &arenav1.AutoscalingPolicy{
						Type:    arenav1.AutoscalingPolicy_TYPE_WEBHOOK,
						Webhook: &arenav1.WebhookPolicy{Url: "https://example.com/scale", Headers: map[string]string{"X-Key": "v"}},
					},
				},
				{
					Policy: &arenav1.AutoscalingPolicy{
						Type:    arenav1.AutoscalingPolicy_TYPE_COUNTER,
						Counter: &arenav1.CounterPolicy{Key: "rooms", BufferPercent: 20},
					},
				},
			},
		},
	}
	out, err := Encode(&arenav1.Fleet{Name: "x", Spec: &arenav1.FleetSpec{Autoscaling: orig}})
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(out, "exported.yaml")
	if err != nil {
		t.Fatalf("exported definition does not decode: %v\n%s", err, out)
	}
	if !proto.Equal(back[0].Spec.GetAutoscaling(), orig) {
		t.Errorf("autoscaling did not round-trip:\noriginal: %v\nre-decoded: %v\nyaml:\n%s",
			orig, back[0].Spec.GetAutoscaling(), out)
	}
}

// TestEncodeDropsAutoscaledDesiredCount: exporting an autoscaled fleet must
// not pin the autoscaler's current replicas into the definition.
func TestEncodeDropsAutoscaledDesiredCount(t *testing.T) {
	out, err := Encode(&arenav1.Fleet{
		Name: "auto",
		Spec: &arenav1.FleetSpec{
			Replicas:    proto.Int32(7),
			Autoscaling: &arenav1.Autoscaling{Enabled: true, MinReplicas: 1, MaxReplicas: 10},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "desiredCount") {
		t.Errorf("autoscaled export pinned desiredCount:\n%s", out)
	}

	// A user-owned fleet keeps it.
	out, err = Encode(&arenav1.Fleet{
		Name: "manual",
		Spec: &arenav1.FleetSpec{Replicas: proto.Int32(7)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "desiredCount: 7") {
		t.Errorf("manual export lost desiredCount:\n%s", out)
	}
}

func TestLoadDirectoryRecursive(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, name string) {
		if err := os.WriteFile(path, []byte("name: "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "a.yaml"), "a")
	write(filepath.Join(sub, "b.yml"), "b")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}

	ms, err := Load([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("loaded %d manifests, want 2 (README ignored)", len(ms))
	}
}

// TestDecodeMultiContainer verifies multiple containerDefinitions
// with gameContainer designation; ports/health/resources hoist from the game
// container only, sidecar-like containers get their own command/args/
// secrets/mountPoints/containerHealthCheck, and volumes are mountable.
func TestDecodeMultiContainer(t *testing.T) {
	const doc = `
name: multi
taskDefinition:
  cpu: "1024"
  memory: "2048"
  gameContainer: gameserver
  volumes:
    - name: assets
      efs: {fileSystemId: fs-123, rootDirectory: /assets}
    - name: scratch
      host: {sourcePath: /mnt/scratch}
  containerDefinitions:
    - name: gameserver
      image: myregistry/gameserver:v1
      portMappings:
        - name: game
          containerPort: 7777
      healthCheck: {startPeriod: 30, interval: 10, retries: 3}
      mountPoints:
        - {volume: assets, containerPath: /data/assets, readOnly: true}
    - name: log-forwarder
      image: myregistry/forwarder:v1
      command: ["/bin/forwarder"]
      args: ["--verbose"]
      workingDirectory: /app
      resources: {cpu: "128", memory: "128"}
      secrets:
        - {name: API_KEY, valueFrom: "arn:aws:secretsmanager:region:acct:secret:key"}
      containerHealthCheck:
        command: ["CMD-SHELL", "curl -f http://localhost/health || exit 1"]
        intervalSeconds: 15
        timeoutSeconds: 5
        retries: 3
        startPeriodSeconds: 10
      mountPoints:
        - {volume: scratch, containerPath: /tmp/scratch}
`
	ms, err := Decode([]byte(doc), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := ms[0].Spec.GetTemplate().GetSpec()
	if spec.GetContainer() != nil {
		t.Fatalf("Container (single-form) set = %v, want nil for multi-container", spec.GetContainer())
	}
	if len(spec.GetContainers()) != 2 {
		t.Fatalf("containers = %d, want 2", len(spec.GetContainers()))
	}
	if spec.GetGameContainer() != "gameserver" {
		t.Errorf("game_container = %q, want gameserver", spec.GetGameContainer())
	}

	game := spec.GetContainers()[0]
	if game.GetResources().GetCpu() != "1024" || game.GetResources().GetMemory() != "2048" {
		t.Errorf("game container resources = %+v, want hoisted task-level cpu/memory", game.GetResources())
	}
	if len(spec.GetPorts()) != 1 || spec.GetPorts()[0].GetContainerPort() != 7777 {
		t.Errorf("ports = %+v, want hoisted from the game container", spec.GetPorts())
	}
	if spec.GetHealth().GetPeriodSeconds() != 10 || spec.GetHealth().GetFailureThreshold() != 3 {
		t.Errorf("health = %+v, want hoisted from the game container's healthCheck", spec.GetHealth())
	}
	if len(game.GetMountPoints()) != 1 || game.GetMountPoints()[0].GetVolume() != "assets" {
		t.Errorf("game container mount points = %+v", game.GetMountPoints())
	}

	sidecar := spec.GetContainers()[1]
	if sidecar.GetResources().GetCpu() != "128" || sidecar.GetResources().GetMemory() != "128" {
		t.Errorf("sidecar resources = %+v, want its own resources block", sidecar.GetResources())
	}
	if len(sidecar.GetCommand()) != 1 || sidecar.GetCommand()[0] != "/bin/forwarder" {
		t.Errorf("sidecar command = %v", sidecar.GetCommand())
	}
	if len(sidecar.GetArgs()) != 1 || sidecar.GetArgs()[0] != "--verbose" {
		t.Errorf("sidecar args = %v", sidecar.GetArgs())
	}
	if sidecar.GetWorkingDirectory() != "/app" {
		t.Errorf("sidecar workingDirectory = %q", sidecar.GetWorkingDirectory())
	}
	if len(sidecar.GetSecrets()) != 1 || sidecar.GetSecrets()[0].GetName() != "API_KEY" {
		t.Errorf("sidecar secrets = %+v", sidecar.GetSecrets())
	}
	if hc := sidecar.GetHealthCheck(); hc.GetIntervalSeconds() != 15 || hc.GetRetries() != 3 {
		t.Errorf("sidecar containerHealthCheck = %+v", hc)
	}
	if len(sidecar.GetMountPoints()) != 1 || sidecar.GetMountPoints()[0].GetVolume() != "scratch" {
		t.Errorf("sidecar mount points = %+v", sidecar.GetMountPoints())
	}

	volumes := spec.GetVolumes()
	if len(volumes) != 2 {
		t.Fatalf("volumes = %d, want 2", len(volumes))
	}
	if volumes[0].GetEfs().GetFileSystemId() != "fs-123" || volumes[0].GetEfs().GetRootDirectory() != "/assets" {
		t.Errorf("efs volume = %+v", volumes[0])
	}
	if volumes[1].GetHost().GetSourcePath() != "/mnt/scratch" {
		t.Errorf("host volume = %+v", volumes[1])
	}
}

// TestEncodeMultiContainerRoundTrip: `arenactl get` output for a
// multi-container Fleet must decode back to the same spec.
func TestEncodeMultiContainerRoundTrip(t *testing.T) {
	orig := &arenav1.FleetSpec{
		Template: &arenav1.GameServerTemplate{
			Spec: &arenav1.GameServerSpec{
				GameContainer: "gameserver",
				Containers: []*arenav1.ContainerSpec{
					{
						Name:      "gameserver",
						Image:     "img:v1",
						Resources: &arenav1.Resources{Cpu: "1024", Memory: "2048"},
						MountPoints: []*arenav1.MountPoint{
							{Volume: "assets", ContainerPath: "/data", ReadOnly: true},
						},
					},
					{
						Name:             "forwarder",
						Image:            "forwarder:v1",
						Command:          []string{"/bin/forwarder"},
						Args:             []string{"--verbose"},
						WorkingDirectory: "/app",
						Resources:        &arenav1.Resources{Cpu: "128", Memory: "128"},
						Secrets:          []*arenav1.SecretRef{{Name: "API_KEY", ValueFrom: "arn:secret"}},
						HealthCheck: &arenav1.ContainerHealthCheck{
							Command: []string{"CMD-SHELL", "true"}, IntervalSeconds: 15, Retries: 3,
						},
					},
				},
				Ports: []*arenav1.PortSpec{{Name: "game", ContainerPort: 7777, Protocol: arenav1.Port_PROTOCOL_UDP}},
				Health: &arenav1.HealthSpec{
					InitialDelaySeconds: 30, PeriodSeconds: 10, FailureThreshold: 3,
				},
				Volumes: []*arenav1.VolumeSpec{
					{Name: "assets", Efs: &arenav1.EFSVolume{FileSystemId: "fs-1", RootDirectory: "/"}},
				},
			},
		},
	}
	out, err := Encode(&arenav1.Fleet{Name: "x", Spec: orig})
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(out, "exported.yaml")
	if err != nil {
		t.Fatalf("exported definition does not decode: %v\n%s", err, out)
	}
	if !proto.Equal(back[0].Spec, orig) {
		t.Errorf("spec did not round-trip:\noriginal: %v\nre-decoded: %v\nyaml:\n%s", orig, back[0].Spec, out)
	}
}

// TestDecodeFleetLevelExtensions verifies strategy/allocationOverflow/
// capacity/network/drainGraceSeconds.
func TestDecodeFleetLevelExtensions(t *testing.T) {
	const doc = `
name: fleet-level
drainGraceSeconds: 45
strategy:
  type: rollingUpdate
  rollingUpdate:
    maxSurge: "25%"
    maxUnavailable: "10%"
    drainTimeoutSeconds: 600
allocationOverflow:
  labels: {overflow: "true"}
  annotations: {note: excess}
capacity:
  providers:
    - {name: FARGATE_SPOT, weight: 4}
    - {name: FARGATE, weight: 1, base: 2}
network:
  assignPublicIp: false
  securityGroups: ["sg-1"]
  subnets: ["subnet-1", "subnet-2"]
`
	ms, err := Decode([]byte(doc), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := ms[0].Spec
	if spec.GetDrainGraceSeconds() != 45 {
		t.Errorf("drainGraceSeconds = %d, want 45", spec.GetDrainGraceSeconds())
	}
	strat := spec.GetStrategy()
	if strat.GetType() != arenav1.Strategy_TYPE_ROLLING_UPDATE {
		t.Errorf("strategy type = %v", strat.GetType())
	}
	if strat.GetRollingUpdate().GetMaxSurge() != "25%" || strat.GetRollingUpdate().GetDrainTimeoutSeconds() != 600 {
		t.Errorf("rollingUpdate = %+v", strat.GetRollingUpdate())
	}
	if spec.GetAllocationOverflow().GetLabels()["overflow"] != "true" {
		t.Errorf("allocationOverflow = %+v", spec.GetAllocationOverflow())
	}
	providers := spec.GetCapacity().GetProviders()
	if len(providers) != 2 || providers[0].GetName() != "FARGATE_SPOT" || providers[0].GetWeight() != 4 {
		t.Errorf("capacity providers = %+v", providers)
	}
	if providers[1].GetBase() != 2 {
		t.Errorf("capacity providers[1].base = %d, want 2", providers[1].GetBase())
	}
	net := spec.GetNetwork()
	if net.GetAssignPublicIp() != false {
		t.Errorf("network.assignPublicIp = %v, want false", net.GetAssignPublicIp())
	}
	if len(net.GetSubnets()) != 2 || len(net.GetSecurityGroups()) != 1 {
		t.Errorf("network = %+v", net)
	}
}

// TestEncodeFleetLevelExtensionsRoundTrip mirrors TestDecodeFleetLevelExtensions
// through Encode.
func TestEncodeFleetLevelExtensionsRoundTrip(t *testing.T) {
	falseVal := false
	orig := &arenav1.FleetSpec{
		DrainGraceSeconds: 45,
		Strategy: &arenav1.Strategy{
			Type:          arenav1.Strategy_TYPE_RECREATE,
			RollingUpdate: &arenav1.RollingUpdate{MaxSurge: "25%", MaxUnavailable: "10%", DrainTimeoutSeconds: 600},
		},
		AllocationOverflow: &arenav1.AllocationOverflow{Labels: map[string]string{"overflow": "true"}},
		Capacity: &arenav1.Capacity{Providers: []*arenav1.CapacityProvider{
			{Name: "FARGATE_SPOT", Weight: 4},
			{Name: "FARGATE", Weight: 1, Base: 2},
		}},
		Network: &arenav1.Network{AssignPublicIp: &falseVal, SecurityGroups: []string{"sg-1"}, Subnets: []string{"subnet-1", "subnet-2"}},
	}
	out, err := Encode(&arenav1.Fleet{Name: "x", Spec: orig})
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(out, "exported.yaml")
	if err != nil {
		t.Fatalf("exported definition does not decode: %v\n%s", err, out)
	}
	if !proto.Equal(back[0].Spec, orig) {
		t.Errorf("spec did not round-trip:\noriginal: %v\nre-decoded: %v\nyaml:\n%s", orig, back[0].Spec, out)
	}
}
