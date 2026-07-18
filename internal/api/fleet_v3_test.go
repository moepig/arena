package api

// Spec validation and normalization: rolling-update
// strategy parameters, Passthrough port assignment, TCPUDP expansion.

import (
	"testing"

	"connectrpc.com/connect"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/allocation"
)

func specWithStrategy(surge, unavailable string) *arenav1.FleetSpec {
	s := minimalSpec("img")
	s.Strategy = &arenav1.Strategy{
		Type: arenav1.Strategy_TYPE_ROLLING_UPDATE,
		RollingUpdate: &arenav1.RollingUpdate{
			MaxSurge:       surge,
			MaxUnavailable: unavailable,
		},
	}
	return s
}

func TestValidateStrategy(t *testing.T) {
	cases := []struct {
		name               string
		surge, unavailable string
		wantErr            bool
	}{
		{"defaults", "", "", false},
		{"percentages", "25%", "50%", false},
		{"absolute", "2", "1", false},
		{"both zero", "0", "0", true},
		{"garbage", "abc", "", true},
		{"negative", "-1", "", true},
		{"garbage percent", "x%", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpec(specWithStrategy(tc.surge, tc.unavailable))
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("validateSpec err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want INVALID_ARGUMENT", connect.CodeOf(err))
			}
		})
	}
}

func TestValidatePassthroughPort(t *testing.T) {
	s := minimalSpec("img")
	s.Template.Spec.Ports = []*arenav1.PortSpec{
		{Name: "game", Policy: arenav1.PortSpec_POLICY_PASSTHROUGH},
	}
	if err := validateSpec(s); err != nil {
		t.Errorf("passthrough without container_port must validate, got %v", err)
	}

	s.Template.Spec.Ports[0].ContainerPort = 7777
	if err := validateSpec(s); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("passthrough with explicit port err = %v, want INVALID_ARGUMENT", err)
	}
}

func TestNormalizeSpecAssignsPassthroughPorts(t *testing.T) {
	s := minimalSpec("img")
	s.Template.Spec.Ports = []*arenav1.PortSpec{
		{Name: "static", ContainerPort: 7000, Protocol: arenav1.Port_PROTOCOL_UDP},
		{Name: "auto-a", Policy: arenav1.PortSpec_POLICY_PASSTHROUGH},
		{Name: "auto-b", Policy: arenav1.PortSpec_POLICY_PASSTHROUGH},
	}
	normalizeSpec(s)

	ports := s.Template.Spec.Ports
	// 7000 is taken by the static port; assignments skip it.
	if ports[1].GetContainerPort() != 7001 || ports[2].GetContainerPort() != 7002 {
		t.Errorf("assigned ports = %d, %d, want 7001, 7002",
			ports[1].GetContainerPort(), ports[2].GetContainerPort())
	}

	// Deterministic: normalizing an identical spec assigns the same ports.
	s2 := minimalSpec("img")
	s2.Template.Spec.Ports = []*arenav1.PortSpec{
		{Name: "static", ContainerPort: 7000, Protocol: arenav1.Port_PROTOCOL_UDP},
		{Name: "auto-a", Policy: arenav1.PortSpec_POLICY_PASSTHROUGH},
		{Name: "auto-b", Policy: arenav1.PortSpec_POLICY_PASSTHROUGH},
	}
	normalizeSpec(s2)
	for i := range ports {
		if ports[i].GetContainerPort() != s2.Template.Spec.Ports[i].GetContainerPort() {
			t.Errorf("port %d differs across normalizations", i)
		}
	}
}

func TestNormalizeSpecExpandsTCPUDP(t *testing.T) {
	s := minimalSpec("img")
	s.Template.Spec.Ports = []*arenav1.PortSpec{
		{Name: "game", ContainerPort: 7777, Protocol: arenav1.Port_PROTOCOL_TCPUDP},
	}
	normalizeSpec(s)

	ports := s.Template.Spec.Ports
	if len(ports) != 2 {
		t.Fatalf("ports = %d, want TCPUDP expanded into 2", len(ports))
	}
	if ports[0].GetName() != "game-tcp" || ports[0].GetProtocol() != arenav1.Port_PROTOCOL_TCP {
		t.Errorf("ports[0] = %s/%s, want game-tcp/TCP", ports[0].GetName(), ports[0].GetProtocol())
	}
	if ports[1].GetName() != "game-udp" || ports[1].GetProtocol() != arenav1.Port_PROTOCOL_UDP {
		t.Errorf("ports[1] = %s/%s, want game-udp/UDP", ports[1].GetName(), ports[1].GetProtocol())
	}
	if ports[0].GetContainerPort() != 7777 || ports[1].GetContainerPort() != 7777 {
		t.Errorf("expanded ports keep the container port, got %d/%d",
			ports[0].GetContainerPort(), ports[1].GetContainerPort())
	}
}

func TestSelectorsFromProtoValidation(t *testing.T) {
	if _, err := selectorsFromProto([]*arenav1.Selectors{
		{MatchFields: map[string]string{"bogus": "x"}},
	}); err == nil {
		t.Error("unknown match_fields key must be rejected")
	}
	if _, err := selectorsFromProto([]*arenav1.Selectors{
		{Required: []*arenav1.Requirement{{Key: "k"}}},
	}); err == nil {
		t.Error("missing operator must be rejected")
	}
	if _, err := selectorsFromProto([]*arenav1.Selectors{
		{Required: []*arenav1.Requirement{{Key: "k", Operator: arenav1.Requirement_OPERATOR_IN}}},
	}); err == nil {
		t.Error("IN without values must be rejected")
	}
	sels, err := selectorsFromProto([]*arenav1.Selectors{
		{MatchLabels: map[string]string{"a": "b"}},
		{Preferred: []*arenav1.Requirement{{Key: "k", Operator: arenav1.Requirement_OPERATOR_EXISTS}}},
	})
	if err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}
	if len(sels) != 2 {
		t.Errorf("selectors = %d, want the chain preserved", len(sels))
	}
}

func TestCounterFiltersFromProtoValidation(t *testing.T) {
	if _, err := counterFiltersFromProto([]*arenav1.CounterFilter{{}}); err == nil {
		t.Error("missing name must be rejected")
	}
	if _, err := counterFiltersFromProto([]*arenav1.CounterFilter{
		{Name: "rooms", MinAvailable: -1},
	}); err == nil {
		t.Error("negative min_available must be rejected")
	}
	if _, err := counterFiltersFromProto([]*arenav1.CounterFilter{
		{Name: "rooms", MinAvailable: 5, MaxAvailable: 1},
	}); err == nil {
		t.Error("max_available below min_available must be rejected")
	}
	got, err := counterFiltersFromProto([]*arenav1.CounterFilter{
		{Name: "rooms", MinAvailable: 1, MaxAvailable: 10},
	})
	if err != nil {
		t.Fatalf("valid filter rejected: %v", err)
	}
	if len(got) != 1 || got[0].Name != "rooms" || got[0].MinAvailable != 1 || got[0].MaxAvailable != 10 {
		t.Errorf("got %+v, want the filter converted through unchanged", got)
	}
}

func TestPrioritiesFromProtoValidation(t *testing.T) {
	if _, err := prioritiesFromProto([]*arenav1.Priority{{}}); err == nil {
		t.Error("missing counter must be rejected")
	}
	got, err := prioritiesFromProto([]*arenav1.Priority{
		{Counter: "rooms", Order: arenav1.Priority_ORDER_DESCENDING},
		{Counter: "players"}, // ORDER_UNSPECIFIED defaults to ascending
	})
	if err != nil {
		t.Fatalf("valid priorities rejected: %v", err)
	}
	if len(got) != 2 || got[0].Order != allocation.PriorityDescending || got[1].Order != allocation.PriorityAscending {
		t.Errorf("got %+v, want [rooms:descending players:ascending]", got)
	}
}
