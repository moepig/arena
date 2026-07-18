package store

// Object events — the `kubectl describe` Events
// equivalent. One item per event; PK "resource" = "{type}#{id}", SK "ts"
// (unix nanos, so bursts within one second keep distinct keys), TTL 7 days.
// GameServer state transitions are recorded from the single Transitioner
// hook; fleet-level events (scaling, rollout) are written by the
// controller.

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// eventTTL is how long events stay queryable.
const eventTTL = 7 * 24 * time.Hour

// Event resource types.
const (
	EventResourceFleet      = "fleet"
	EventResourceGameServer = "gameserver"
)

// Event types.
const (
	EventNormal  = "Normal"
	EventWarning = "Warning"
)

// Event is an item in the `events` table.
type Event struct {
	Resource     string `dynamodbav:"resource"` // "{type}#{id}"
	TS           int64  `dynamodbav:"ts"`       // unix nanos (sort key)
	ResourceType string `dynamodbav:"resource_type"`
	ResourceID   string `dynamodbav:"resource_id"`
	Type         string `dynamodbav:"type"` // Normal | Warning
	Reason       string `dynamodbav:"reason"`
	Message      string `dynamodbav:"message,omitempty"`
	TTL          int64  `dynamodbav:"ttl"`
}

// PutEvent records one event. Best-effort by contract: callers log failures
// instead of failing the operation that produced the event.
func (s *Store) PutEvent(ctx context.Context, resourceType, resourceID, eventType, reason, message string) error {
	now := s.now()
	ev := Event{
		Resource:     resourceType + "#" + resourceID,
		TS:           now.UnixNano(),
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Type:         eventType,
		Reason:       reason,
		Message:      message,
		TTL:          now.Add(eventTTL).Unix(),
	}
	item, err := attributevalue.MarshalMap(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: s.table(tableEvents),
		Item:      item,
	})
	return err
}

// ListEvents returns a resource's events, newest first.
func (s *Store) ListEvents(ctx context.Context, resourceType, resourceID string, limit int32) ([]Event, error) {
	if limit <= 0 {
		limit = 50
	}
	out, err := s.db.Query(ctx, &dynamodb.QueryInput{
		TableName:              s.table(tableEvents),
		KeyConditionExpression: aws.String("#r = :r"),
		ExpressionAttributeNames: map[string]string{
			"#r": "resource",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":r": &types.AttributeValueMemberS{Value: resourceType + "#" + resourceID},
		},
		ScanIndexForward: aws.Bool(false), // newest first
		Limit:            aws.Int32(limit),
	})
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(out.Items))
	for _, item := range out.Items {
		var ev Event
		if err := attributevalue.UnmarshalMap(item, &ev); err != nil {
			return nil, fmt.Errorf("unmarshal event: %w", err)
		}
		events = append(events, ev)
	}
	return events, nil
}
