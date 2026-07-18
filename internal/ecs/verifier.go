package ecs

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
)

// DescribeTasksAPI is the ECS surface the verifier uses.
type DescribeTasksAPI interface {
	DescribeTasks(ctx context.Context, in *awsecs.DescribeTasksInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error)
}

// TaskVerifier authenticates sidecar sessions: the
// sidecar presents its Task ARN (from the task metadata endpoint) and the
// verifier checks the task's startedBy really is "arena:{gameserver_id}".
// A task cannot present another task's ARN and claim its gameserver_id
// without also being started for it, so gameserver_id spoofing is closed
// without giving game server tasks any IAM API permissions.
type TaskVerifier struct {
	api     DescribeTasksAPI
	cluster string

	mu       sync.Mutex
	verified map[string]bool // "taskARN|gsID" → verified (identity is immutable)
}

// NewTaskVerifier returns a verifier for tasks in the given cluster.
func NewTaskVerifier(api DescribeTasksAPI, cluster string) *TaskVerifier {
	return &TaskVerifier{api: api, cluster: cluster, verified: map[string]bool{}}
}

// Verify implements gateway.Verifier.
func (v *TaskVerifier) Verify(ctx context.Context, gameserverID, taskARN string) error {
	if taskARN == "" {
		return fmt.Errorf("session for %s presented no task ARN", gameserverID)
	}
	key := taskARN + "|" + gameserverID
	v.mu.Lock()
	ok := v.verified[key]
	v.mu.Unlock()
	if ok {
		return nil
	}

	out, err := v.api.DescribeTasks(ctx, &awsecs.DescribeTasksInput{
		Cluster: aws.String(v.cluster),
		Tasks:   []string{taskARN},
	})
	if err != nil {
		return fmt.Errorf("describe task %s: %w", taskARN, err)
	}
	if len(out.Tasks) == 0 {
		return fmt.Errorf("task %s not found in cluster %s", taskARN, v.cluster)
	}
	if got := aws.ToString(out.Tasks[0].StartedBy); got != StartedBy(gameserverID) {
		return fmt.Errorf("task %s startedBy %q does not match gameserver %s", taskARN, got, gameserverID)
	}

	// Cache the positive result: the (task, gameserver) binding never
	// changes, and reconnects should not re-hit the ECS API. Negative
	// results are not cached — a task still PROVISIONING may verify later.
	v.mu.Lock()
	if len(v.verified) > 100_000 { // safety valve; entries die with tasks
		v.verified = map[string]bool{}
	}
	v.verified[key] = true
	v.mu.Unlock()
	return nil
}
