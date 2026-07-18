package store

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// GetAllocation fetches an Allocation by ID (idempotency-derived).
func (s *Store) GetAllocation(ctx context.Context, allocID string) (*Allocation, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: s.table(tableAllocations),
		Key: map[string]types.AttributeValue{
			"allocation_id": &types.AttributeValueMemberS{Value: allocID},
		},
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, ErrNotFound
	}
	var a Allocation
	if err := attributevalue.UnmarshalMap(out.Item, &a); err != nil {
		return nil, fmt.Errorf("unmarshal allocation: %w", err)
	}
	return &a, nil
}

// ReleaseAllocation stamps released_at on an active allocation. Idempotent:
// releasing an already-released allocation is a no-op.
func (s *Store) ReleaseAllocation(ctx context.Context, allocID string) error {
	_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: s.table(tableAllocations),
		Key: map[string]types.AttributeValue{
			"allocation_id": &types.AttributeValueMemberS{Value: allocID},
		},
		UpdateExpression:    aws.String("SET released_at = :now"),
		ConditionExpression: aws.String("attribute_exists(allocation_id) AND attribute_not_exists(released_at)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberN{Value: fmt.Sprint(s.now().Unix())},
		},
	})
	if isConditionalCheckFailed(err) {
		// Missing or already released: treat both as done. The state of
		// record for reuse is the GameServer transition, not this stamp.
		return nil
	}
	return err
}

// AddAllocation commits an additional Allocation record for a GameServer
// that stays Allocated (high-density reallocation). Unlike
// ClaimGameServer, the GameServer item itself is not mutated, only checked:
// a ConditionCheck that it is still Allocated rides in the same transaction
// as the Allocation Put, so a concurrent Ready()/Release() aborts the claim
// instead of racing it. ErrConditionFailed covers both that case and a
// duplicate allocation_id (idempotent resend — the caller resolves it via
// GetAllocation, same as ClaimGameServer's callers).
func (s *Store) AddAllocation(ctx context.Context, gsID string, alloc Allocation) (*Allocation, error) {
	gs, err := s.GetGameServer(ctx, gsID)
	if err != nil {
		return nil, err
	}
	alloc.GameServerID = gsID
	alloc.FleetID = gs.FleetID
	alloc.AllocatedAt = s.now().Unix()

	allocItem, err := attributevalue.MarshalMap(alloc)
	if err != nil {
		return nil, fmt.Errorf("marshal allocation: %w", err)
	}
	_, err = s.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{
				ConditionCheck: &types.ConditionCheck{
					TableName: s.table(tableGameServers),
					Key: map[string]types.AttributeValue{
						"gameserver_id": &types.AttributeValueMemberS{Value: gsID},
					},
					ConditionExpression: aws.String("#st = :allocated"),
					ExpressionAttributeNames: map[string]string{
						"#st": "state",
					},
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":allocated": &types.AttributeValueMemberS{Value: string(StateAllocated)},
					},
				},
			},
			{
				Put: &types.Put{
					TableName:           s.table(tableAllocations),
					Item:                allocItem,
					ConditionExpression: aws.String("attribute_not_exists(allocation_id)"),
				},
			},
		},
	})
	if isTransactionConditionFailed(err) {
		return nil, fmt.Errorf("%w: gameserver %s no longer allocated (or duplicate allocation_id)", ErrConditionFailed, gsID)
	}
	if err != nil {
		return nil, err
	}
	return &alloc, nil
}

// ReleaseActiveAllocationsForGameServer stamps released_at on every active
// allocation of a GameServer. Used when the SDK returns an Allocated server
// to Ready so allocation records don't dangle. Best-effort
// per record; the authoritative reuse signal is the state transition.
func (s *Store) ReleaseActiveAllocationsForGameServer(ctx context.Context, gsID string) error {
	var startKey map[string]types.AttributeValue
	for {
		out, err := s.db.Query(ctx, &dynamodb.QueryInput{
			TableName:              s.table(tableAllocations),
			IndexName:              aws.String(indexGameServer),
			KeyConditionExpression: aws.String("gameserver_id = :g"),
			FilterExpression:       aws.String("attribute_not_exists(released_at)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":g": &types.AttributeValueMemberS{Value: gsID},
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return err
		}
		for _, item := range out.Items {
			var a Allocation
			if err := attributevalue.UnmarshalMap(item, &a); err != nil {
				return fmt.Errorf("unmarshal allocation: %w", err)
			}
			if err := s.ReleaseAllocation(ctx, a.ID); err != nil {
				return err
			}
		}
		if out.LastEvaluatedKey == nil {
			return nil
		}
		startKey = out.LastEvaluatedKey
	}
}
