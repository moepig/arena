package controller

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/store"
)

func autoscaledFleet(fs *fakeCtrlStore, id string, replicas int32, as *arenav1.Autoscaling) *store.Fleet {
	fl := addFleet(fs, id, replicas)
	b, err := protojson.Marshal(as)
	if err != nil {
		panic(err)
	}
	fl.AutoscalingJSON = string(b)
	fl.AutoscalingEnabled = as.GetEnabled()
	return fl
}

func bufferPolicy(size, percent, minR, maxR int32) *arenav1.Autoscaling {
	return &arenav1.Autoscaling{
		Enabled: true,
		Policy: &arenav1.AutoscalingPolicy{
			Type:   arenav1.AutoscalingPolicy_TYPE_BUFFER,
			Buffer: &arenav1.BufferPolicy{BufferSize: size, BufferPercent: percent},
		},
		MinReplicas: minR,
		MaxReplicas: maxR,
	}
}

func TestAutoscaleBufferScalesUp(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	autoscaledFleet(fs, "f1", 2, bufferPolicy(3, 0, 1, 20))
	// 2 allocated → desired = 2 + 3 = 5.
	addGS(fs, "gs-a1", "f1", store.StateAllocated, 1_000)
	addGS(fs, "gs-a2", "f1", store.StateAllocated, 1_000)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.fleets["f1"].Replicas; got != 5 {
		t.Errorf("replicas = %d, want 5", got)
	}
	// active = 2 → launch 3 in the same pass (serialized decision + action).
	if len(fl.launched) != 3 {
		t.Errorf("launched = %d, want 3", len(fl.launched))
	}
}

func TestAutoscaleBufferClampsToMax(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	autoscaledFleet(fs, "f1", 2, bufferPolicy(10, 0, 1, 4))
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.fleets["f1"].Replicas; got != 4 {
		t.Errorf("replicas = %d, want max clamp 4", got)
	}
}

func TestAutoscaleBufferPercent(t *testing.T) {
	if got := bufferDesired(&arenav1.BufferPolicy{BufferPercent: 50}, 10); got != 15 {
		t.Errorf("50%% of 10 allocated → desired = %d, want 15", got)
	}
	if got := bufferDesired(&arenav1.BufferPolicy{BufferPercent: 30}, 0); got != 1 {
		t.Errorf("idle fleet with percent buffer → desired = %d, want 1 (floor)", got)
	}
}

func TestAutoscaleScheduleMostRecentEntryWins(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	autoscaledFleet(fs, "f1", 3, &arenav1.Autoscaling{
		Enabled: true,
		Policy: &arenav1.AutoscalingPolicy{
			Type: arenav1.AutoscalingPolicy_TYPE_SCHEDULE,
			Schedule: []*arenav1.SchedulePolicy{
				{Cron: "0 8 * * *", Replicas: 20}, // 08:00 daily
				{Cron: "0 22 * * *", Replicas: 5}, // 22:00 daily
			},
		},
		MinReplicas: 1,
		MaxReplicas: 50,
	})
	c := newTestController(fs, fl, fp)
	// 12:00 → the 08:00 entry fired most recently.
	c.now = func() time.Time { return time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC) }

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.fleets["f1"].Replicas; got != 20 {
		t.Errorf("replicas at 12:00 = %d, want 20", got)
	}

	// 23:00 → the 22:00 entry took over.
	c.now = func() time.Time { return time.Date(2026, 7, 12, 23, 0, 0, 0, time.UTC) }
	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.fleets["f1"].Replicas; got != 5 {
		t.Errorf("replicas at 23:00 = %d, want 5", got)
	}
}

func TestAutoscaleDisabledKeepsUserReplicas(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	f := autoscaledFleet(fs, "f1", 2, bufferPolicy(5, 0, 1, 20))
	f.AutoscalingEnabled = false
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.fleets["f1"].Replicas; got != 2 {
		t.Errorf("replicas = %d, want untouched 2", got)
	}
}

func TestParseCron(t *testing.T) {
	cases := []struct {
		expr string
		t    time.Time
		want bool
	}{
		{"0 8 * * *", time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC), true},
		{"0 8 * * *", time.Date(2026, 7, 12, 8, 1, 0, 0, time.UTC), false},
		{"*/15 * * * *", time.Date(2026, 7, 12, 3, 45, 0, 0, time.UTC), true},
		{"*/15 * * * *", time.Date(2026, 7, 12, 3, 46, 0, 0, time.UTC), false},
		{"0 9-17 * * 1-5", time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC), true},  // Friday
		{"0 9-17 * * 1-5", time.Date(2026, 7, 12, 13, 0, 0, 0, time.UTC), false}, // Sunday
		{"30 6 1,15 * *", time.Date(2026, 7, 15, 6, 30, 0, 0, time.UTC), true},
		{"30 6 1,15 * *", time.Date(2026, 7, 14, 6, 30, 0, 0, time.UTC), false},
		{"0 0 * * 7", time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC), true}, // 7 ≡ Sunday
	}
	for _, tc := range cases {
		expr, err := parseCron(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		if got := expr.matches(tc.t); got != tc.want {
			t.Errorf("%q matches %s = %v, want %v", tc.expr, tc.t, got, tc.want)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "a * * * *", "*/0 * * * *"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("parse %q: want error", bad)
		}
	}
}

func TestCronLastMatch(t *testing.T) {
	expr, err := parseCron("0 8 * * *")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 12, 12, 34, 56, 0, time.UTC)
	got, ok := expr.lastMatch(now, scheduleLookback)
	if !ok || !got.Equal(time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("lastMatch = %v/%v, want 08:00 today", got, ok)
	}

	never, err := parseCron("0 0 30 2 *") // Feb 30 never fires
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := never.lastMatch(now, scheduleLookback); ok {
		t.Error("impossible schedule reported a match")
	}
}
