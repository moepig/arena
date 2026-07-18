package ecs

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

type fakeDescribeTasks struct {
	startedBy map[string]string // taskARN → startedBy
	calls     int
	err       error
}

func (f *fakeDescribeTasks) DescribeTasks(_ context.Context, in *awsecs.DescribeTasksInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	sb, ok := f.startedBy[in.Tasks[0]]
	if !ok {
		return &awsecs.DescribeTasksOutput{}, nil
	}
	return &awsecs.DescribeTasksOutput{
		Tasks: []ecstypes.Task{{TaskArn: aws.String(in.Tasks[0]), StartedBy: aws.String(sb)}},
	}, nil
}

func TestVerifyMatchesStartedBy(t *testing.T) {
	f := &fakeDescribeTasks{startedBy: map[string]string{"arn:task/1": "arena:gs-1"}}
	v := NewTaskVerifier(f, "arena")

	if err := v.Verify(context.Background(), "gs-1", "arn:task/1"); err != nil {
		t.Fatal(err)
	}
	// Cached: a reconnect does not re-hit the ECS API.
	if err := v.Verify(context.Background(), "gs-1", "arn:task/1"); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Errorf("DescribeTasks calls = %d, want 1 (cached)", f.calls)
	}
}

func TestVerifyRejectsSpoofedID(t *testing.T) {
	f := &fakeDescribeTasks{startedBy: map[string]string{"arn:task/1": "arena:gs-1"}}
	v := NewTaskVerifier(f, "arena")

	if err := v.Verify(context.Background(), "gs-other", "arn:task/1"); err == nil {
		t.Fatal("spoofed gameserver_id verified")
	}
	if err := v.Verify(context.Background(), "gs-1", ""); err == nil {
		t.Fatal("missing task ARN verified")
	}
	if err := v.Verify(context.Background(), "gs-1", "arn:task/unknown"); err == nil {
		t.Fatal("unknown task verified")
	}
}

func TestVerifyDoesNotCacheFailures(t *testing.T) {
	f := &fakeDescribeTasks{err: errors.New("throttled")}
	v := NewTaskVerifier(f, "arena")

	if err := v.Verify(context.Background(), "gs-1", "arn:task/1"); err == nil {
		t.Fatal("API error verified")
	}
	f.err = nil
	f.startedBy = map[string]string{"arn:task/1": "arena:gs-1"}
	if err := v.Verify(context.Background(), "gs-1", "arn:task/1"); err != nil {
		t.Fatalf("retry after transient failure: %v", err)
	}
}
