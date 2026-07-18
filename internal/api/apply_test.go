package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/store"
)

// fakeFleetStore is an in-memory FleetStore with DynamoDB-equivalent
// version conditions.
type fakeFleetStore struct {
	fleets map[string]*store.Fleet // by ID
	// conflictOnce makes the next UpdateFleet fail with a version conflict
	// (simulating a concurrent controller write) before succeeding.
	conflictOnce bool
}

func newFakeFleetStore() *fakeFleetStore {
	return &fakeFleetStore{fleets: map[string]*store.Fleet{}}
}

func (f *fakeFleetStore) CreateFleet(_ context.Context, fl store.Fleet) error {
	if _, ok := f.fleets[fl.ID]; ok {
		return store.ErrAlreadyExists
	}
	fl.CreatedAt, fl.UpdatedAt = 111, 111
	f.fleets[fl.ID] = &fl
	return nil
}

func (f *fakeFleetStore) GetFleet(_ context.Context, id string) (*store.Fleet, error) {
	if fl, ok := f.fleets[id]; ok {
		cp := *fl
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeFleetStore) GetFleetByName(_ context.Context, ns, name string) (*store.Fleet, error) {
	for _, fl := range f.fleets {
		if fl.Namespace == ns && fl.Name == name {
			cp := *fl
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeFleetStore) ListFleets(_ context.Context, ns string, _ int32, _ string) ([]store.Fleet, string, error) {
	var out []store.Fleet
	for _, fl := range f.fleets {
		if fl.Namespace == ns {
			out = append(out, *fl)
		}
	}
	return out, "", nil
}

func (f *fakeFleetStore) UpdateFleet(_ context.Context, fl store.Fleet) (*store.Fleet, error) {
	if f.conflictOnce {
		f.conflictOnce = false
		// Bump the stored version like a real concurrent write would.
		f.fleets[fl.ID].Version++
		return nil, store.ErrVersionConflict
	}
	cur, ok := f.fleets[fl.ID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if cur.Version != fl.Version {
		return nil, store.ErrVersionConflict
	}
	fl.Version++
	f.fleets[fl.ID] = &fl
	cp := fl
	return &cp, nil
}

func (f *fakeFleetStore) DeleteFleet(_ context.Context, id string) error {
	if _, ok := f.fleets[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.fleets, id)
	return nil
}

func (f *fakeFleetStore) ListAllGameServersByFleet(context.Context, string, store.State) ([]store.GameServer, error) {
	return nil, nil
}

func applyReq(name string, spec *arenav1.FleetSpec, dryRun bool) *connect.Request[arenav1.ApplyFleetRequest] {
	return connect.NewRequest(&arenav1.ApplyFleetRequest{
		Namespace: "default",
		Name:      name,
		Labels:    map[string]string{"arena.dev/managed-by": "arenactl"},
		Spec:      spec,
		DryRun:    dryRun,
	})
}

func minimalSpec(image string) *arenav1.FleetSpec {
	return &arenav1.FleetSpec{
		Replicas: proto.Int32(3),
		Template: &arenav1.GameServerTemplate{
			Spec: &arenav1.GameServerSpec{
				Container: &arenav1.ContainerSpec{Image: image},
			},
		},
	}
}

func TestApplyCreatesThenUnchangedThenUpdates(t *testing.T) {
	fs := newFakeFleetStore()
	s := &FleetServer{store: fs}
	ctx := context.Background()

	// First apply creates.
	res, err := s.ApplyFleet(ctx, applyReq("shooter", minimalSpec("img:v1"), false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetAction() != arenav1.ApplyFleetResponse_ACTION_CREATED {
		t.Fatalf("action = %v, want CREATED", res.Msg.GetAction())
	}
	if res.Msg.GetFleet().GetSpec().GetReplicas() != 3 {
		t.Errorf("replicas = %d, want 3", res.Msg.GetFleet().GetSpec().GetReplicas())
	}
	if res.Msg.GetFleet().GetGeneration() != 1 {
		t.Errorf("generation = %d, want 1", res.Msg.GetFleet().GetGeneration())
	}

	// Identical apply is a no-op.
	res, err = s.ApplyFleet(ctx, applyReq("shooter", minimalSpec("img:v1"), false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetAction() != arenav1.ApplyFleetResponse_ACTION_UNCHANGED {
		t.Fatalf("action = %v, want UNCHANGED (diff: %s)", res.Msg.GetAction(), res.Msg.GetDiff())
	}

	// New image updates and bumps the generation.
	res, err = s.ApplyFleet(ctx, applyReq("shooter", minimalSpec("img:v2"), false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetAction() != arenav1.ApplyFleetResponse_ACTION_UPDATED {
		t.Fatalf("action = %v, want UPDATED", res.Msg.GetAction())
	}
	if res.Msg.GetFleet().GetGeneration() != 2 {
		t.Errorf("generation = %d, want 2 after template change", res.Msg.GetFleet().GetGeneration())
	}
	if !strings.Contains(res.Msg.GetDiff(), "img:v2") {
		t.Errorf("diff does not mention the new image:\n%s", res.Msg.GetDiff())
	}
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	fs := newFakeFleetStore()
	s := &FleetServer{store: fs}

	res, err := s.ApplyFleet(context.Background(), applyReq("shooter", minimalSpec("img:v1"), true))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetAction() != arenav1.ApplyFleetResponse_ACTION_CREATED {
		t.Fatalf("action = %v, want CREATED", res.Msg.GetAction())
	}
	if len(fs.fleets) != 0 {
		t.Fatal("dry run persisted a fleet")
	}
	if res.Msg.GetNormalizedSpec().GetReplicas() != 3 {
		t.Errorf("normalized replicas = %d", res.Msg.GetNormalizedSpec().GetReplicas())
	}
}

func TestApplyRejectsReplicasOnAutoscaledFleet(t *testing.T) {
	s := &FleetServer{store: newFakeFleetStore()}
	spec := minimalSpec("img:v1") // sets replicas=3
	spec.Autoscaling = &arenav1.Autoscaling{Enabled: true, MinReplicas: 1, MaxReplicas: 10}

	_, err := s.ApplyFleet(context.Background(), applyReq("shooter", spec, false))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v, want FAILED_PRECONDITION", err)
	}
}

func TestApplyAutoscaledFleetKeepsServerReplicas(t *testing.T) {
	fs := newFakeFleetStore()
	s := &FleetServer{store: fs}
	ctx := context.Background()

	spec := minimalSpec("img:v1")
	spec.Replicas = nil
	spec.Autoscaling = &arenav1.Autoscaling{
		Enabled:     true,
		Policy:      &arenav1.AutoscalingPolicy{Type: arenav1.AutoscalingPolicy_TYPE_BUFFER, Buffer: &arenav1.BufferPolicy{BufferSize: 2}},
		MinReplicas: 2,
		MaxReplicas: 10,
	}
	if _, err := s.ApplyFleet(ctx, applyReq("shooter", spec, false)); err != nil {
		t.Fatal(err)
	}
	// Autoscaler moved replicas to 7 in the meantime.
	for _, fl := range fs.fleets {
		fl.Replicas = 7
	}

	// Re-apply must not roll the scale back.
	res, err := s.ApplyFleet(ctx, applyReq("shooter", spec, false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetAction() != arenav1.ApplyFleetResponse_ACTION_UNCHANGED {
		t.Fatalf("action = %v, want UNCHANGED (diff: %s)", res.Msg.GetAction(), res.Msg.GetDiff())
	}
	for _, fl := range fs.fleets {
		if fl.Replicas != 7 {
			t.Errorf("replicas = %d, want autoscaler's 7 preserved", fl.Replicas)
		}
	}
}

func TestApplyRetriesVersionConflict(t *testing.T) {
	fs := newFakeFleetStore()
	s := &FleetServer{store: fs}
	ctx := context.Background()

	if _, err := s.ApplyFleet(ctx, applyReq("shooter", minimalSpec("img:v1"), false)); err != nil {
		t.Fatal(err)
	}
	fs.conflictOnce = true // controller writes status between our read and write

	res, err := s.ApplyFleet(ctx, applyReq("shooter", minimalSpec("img:v2"), false))
	if err != nil {
		t.Fatalf("apply should retry internal version conflicts: %v", err)
	}
	if res.Msg.GetAction() != arenav1.ApplyFleetResponse_ACTION_UPDATED {
		t.Fatalf("action = %v, want UPDATED", res.Msg.GetAction())
	}
}

func TestApplyValidation(t *testing.T) {
	s := &FleetServer{store: newFakeFleetStore()}
	ctx := context.Background()

	if _, err := s.ApplyFleet(ctx, applyReq("Bad_Name", minimalSpec("img:v1"), false)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad name err = %v, want INVALID_ARGUMENT", err)
	}
	noImage := minimalSpec("img:v1")
	noImage.Template.Spec.Container.Image = ""
	if _, err := s.ApplyFleet(ctx, applyReq("shooter", noImage, false)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing image err = %v, want INVALID_ARGUMENT", err)
	}
}
