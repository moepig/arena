package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/ecs"
	"github.com/moepig/arena/internal/store"
)

// TaskStateChange is the subset of an ECS "Task State Change" EventBridge
// event the controller acts on.
type TaskStateChange struct {
	TaskARN       string
	LastStatus    string
	StartedBy     string
	StoppedReason string
	ENIID         string
	PrivateIP     string
}

// queueEvent is one decoded EventBridge message: either an ECS task state
// change or an EC2 Spot interruption warning.
type queueEvent struct {
	task           *TaskStateChange
	spotInstanceID string
}

// parseQueueEvent decodes an EventBridge envelope from an SQS message body.
// Returns (nil, nil) for detail types the controller ignores.
func parseQueueEvent(body string) (*queueEvent, error) {
	var env struct {
		DetailType string `json:"detail-type"`
		Detail     struct {
			// ECS Task State Change fields.
			TaskArn       string `json:"taskArn"`
			LastStatus    string `json:"lastStatus"`
			StartedBy     string `json:"startedBy"`
			StoppedReason string `json:"stoppedReason"`
			Attachments   []struct {
				Type    string `json:"type"`
				Details []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"details"`
			} `json:"attachments"`
			// EC2 Spot Instance Interruption Warning field.
			InstanceID string `json:"instance-id"`
		} `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return nil, fmt.Errorf("parse task event: %w", err)
	}
	switch env.DetailType {
	case "EC2 Spot Instance Interruption Warning":
		if env.Detail.InstanceID == "" {
			return nil, nil
		}
		return &queueEvent{spotInstanceID: env.Detail.InstanceID}, nil
	case "ECS Task State Change":
	default:
		return nil, nil
	}
	ev := &TaskStateChange{
		TaskARN:       env.Detail.TaskArn,
		LastStatus:    env.Detail.LastStatus,
		StartedBy:     env.Detail.StartedBy,
		StoppedReason: env.Detail.StoppedReason,
	}
	for _, att := range env.Detail.Attachments {
		if att.Type != "eni" {
			continue
		}
		for _, d := range att.Details {
			switch d.Name {
			case "networkInterfaceId":
				ev.ENIID = d.Value
			case "privateIPv4Address":
				ev.PrivateIP = d.Value
			}
		}
	}
	return &queueEvent{task: ev}, nil
}

// SQSAPI is the SQS surface the consumer uses.
type SQSAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// EventConsumer long-polls the EventBridge→SQS queue and feeds task state
// changes to the controller (edge trigger). A failed
// handler leaves the message on the queue: SQS redelivery is the retry, the
// DLQ the backstop, and resync the safety net.
type EventConsumer struct {
	sqs         SQSAPI
	queueURL    string
	log         *slog.Logger
	handler     func(ctx context.Context, ev TaskStateChange) error
	spotHandler func(ctx context.Context, instanceID string) error
}

// NewEventConsumer returns a consumer; Controller.New wires the handler.
func NewEventConsumer(api SQSAPI, queueURL string, log *slog.Logger) *EventConsumer {
	if log == nil {
		log = slog.Default()
	}
	return &EventConsumer{sqs: api, queueURL: queueURL, log: log}
}

// Run consumes until ctx is done. It runs only on the leader (started by
// lead()), so events and reconciles share one writer.
func (e *EventConsumer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		out, err := e.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(e.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     20,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.log.Warn("sqs receive failed", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, msg := range out.Messages {
			if err := e.handleMessage(ctx, msg); err != nil {
				e.log.Warn("task event failed, leaving for redelivery", "error", err)
				continue
			}
			if _, err := e.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(e.queueURL),
				ReceiptHandle: msg.ReceiptHandle,
			}); err != nil {
				// Redelivery re-runs an idempotent handler; just log.
				e.log.Warn("sqs delete failed", "error", err)
			}
		}
	}
}

func (e *EventConsumer) handleMessage(ctx context.Context, msg sqstypes.Message) error {
	ev, err := parseQueueEvent(aws.ToString(msg.Body))
	if err != nil {
		// Deterministic parse failure — retrying cannot help, drop it.
		e.log.Warn("unparseable task event dropped", "error", err)
		return nil
	}
	if ev == nil {
		return nil
	}
	if ev.spotInstanceID != "" && e.spotHandler != nil {
		return e.spotHandler(ctx, ev.spotInstanceID)
	}
	if ev.task != nil && e.handler != nil {
		return e.handler(ctx, *ev.task)
	}
	return nil
}

// handleTaskEvent applies an ECS task state change to the GameServer record.
// Idempotent: duplicate events fail their conditional
// writes and are absorbed.
func (c *Controller) handleTaskEvent(ctx context.Context, ev TaskStateChange) error {
	gsID, ok := ecs.ParseStartedBy(ev.StartedBy)
	if !ok {
		return nil // not an arena task
	}
	switch ev.LastStatus {
	case "RUNNING":
		return c.taskRunning(ctx, gsID, ev)
	case "STOPPED":
		return c.taskStopped(ctx, gsID, ev)
	default:
		return nil
	}
}

// taskRunning records the task's network identity and moves Scheduled →
// Starting. The sidecar's Ready() takes it from there.
func (c *Controller) taskRunning(ctx context.Context, gsID string, ev TaskStateChange) error {
	addr, err := c.resolveAddress(ctx, ev)
	if err != nil {
		// ENI association can lag the event; redelivery retries.
		return err
	}
	gs, err := c.store.TransitionState(ctx, gsID, store.StateScheduled, store.StateStarting, func(g *store.GameServer) {
		g.TaskARN = ev.TaskARN
		g.Address = addr
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The task lives but its record is gone: orphan, stop it.
		return c.launcher.Stop(ctx, ev.TaskARN, "orphan task: no gameserver record")
	case errors.Is(err, store.ErrConditionFailed):
		return c.stopIfDefunct(ctx, gsID, ev.TaskARN)
	case err != nil:
		return err
	}
	c.Enqueue(gs.FleetID)
	return nil
}

// stopIfDefunct handles a RUNNING event for a server that is not Scheduled:
// either a duplicate event (server already progressed — ignore) or a task
// that came up after its server was written off (stop it).
func (c *Controller) stopIfDefunct(ctx context.Context, gsID, taskARN string) error {
	gs, err := c.store.GetGameServer(ctx, gsID)
	if errors.Is(err, store.ErrNotFound) {
		return c.launcher.Stop(ctx, taskARN, "orphan task: no gameserver record")
	}
	if err != nil {
		return err
	}
	switch gs.State {
	case store.StateDraining, store.StateUnhealthy, store.StateTerminated:
		return c.launcher.Stop(ctx, taskARN, "gameserver "+string(gs.State))
	}
	return nil
}

// taskStopped confirms Terminated from any state and triggers replenishment.
func (c *Controller) taskStopped(ctx context.Context, gsID string, ev TaskStateChange) error {
	gs, err := c.store.MarkTerminated(ctx, gsID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err // includes transition races; redelivery retries
	}
	// A Ready server that died may still be pooled; remove defensively.
	if err := c.pool.Remove(ctx, gs.FleetID, gsID); err != nil {
		c.log.Warn("pool remove failed", "gameserver_id", gsID, "error", err)
	}
	c.log.Info("gameserver terminated", "gameserver_id", gsID, "fleet_id", gs.FleetID, "reason", ev.StoppedReason)
	c.Enqueue(gs.FleetID)
	return nil
}

// handleSpotInterruption drains every arena GameServer on an interrupted
// EC2 Spot instance and kicks replacements off immediately, spending the
// 2-minute warning on new task startup.
func (c *Controller) handleSpotInterruption(ctx context.Context, instanceID string) error {
	if c.instances == nil {
		c.log.Warn("spot interruption received but no instance resolver configured", "instance_id", instanceID)
		return nil
	}
	gsIDs, err := c.instances.GameServersOnInstance(ctx, instanceID)
	if err != nil {
		return err // SQS redelivery retries within the warning window
	}
	c.log.Info("spot interruption: draining instance", "instance_id", instanceID, "gameservers", len(gsIDs))
	var errs []error
	for _, id := range gsIDs {
		errs = append(errs, c.drainGameServer(ctx, id, "spot interruption"))
	}
	return errors.Join(errs...)
}

// drainGameServer moves a live server to Draining, removes it from the
// pool, pushes the new state to its watch stream (so the game evacuates the
// session within the grace window), and enqueues the fleet for replacement.
func (c *Controller) drainGameServer(ctx context.Context, gsID, reason string) error {
	gs, err := c.store.GetGameServer(ctx, gsID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch gs.State {
	case store.StateReady, store.StateAllocated, store.StateReserved:
	default:
		return nil // already on its way out (or not yet live)
	}
	ngs, err := c.store.TransitionState(ctx, gsID, gs.State, store.StateDraining, nil)
	if errors.Is(err, store.ErrConditionFailed) {
		return nil // whoever won owns the teardown
	}
	if err != nil {
		return err
	}
	if gs.State == store.StateReady {
		if err := c.pool.Remove(ctx, gs.FleetID, gsID); err != nil {
			c.log.Warn("pool remove on drain failed", "gameserver_id", gsID, "error", err)
		}
	}
	// Best-effort watch push; the game sees Draining and starts evacuating.
	_ = c.pool.PublishAllocation(ctx, gsID, convert.EncodeStatePush(ngs))
	c.log.Info("gameserver draining", "gameserver_id", gsID, "fleet_id", gs.FleetID, "reason", reason)
	c.Enqueue(gs.FleetID)
	return nil
}

// resolveAddress picks the address clients connect to (public IP),
// falling back to the event's private IP without a resolver (dev /
// VPC-internal setups).
func (c *Controller) resolveAddress(ctx context.Context, ev TaskStateChange) (string, error) {
	if c.resolver == nil || ev.ENIID == "" {
		return ev.PrivateIP, nil
	}
	ip, err := c.resolver.PublicIP(ctx, ev.ENIID)
	if err != nil {
		return "", err
	}
	if ip == "" {
		return ev.PrivateIP, nil
	}
	return ip, nil
}
