package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// AcquireLease takes or renews a leader lease with a conditional Put: it
// succeeds when the lease is absent, expired, or already held by this
// instance. Returns false when another live holder owns it.
func (s *Store) AcquireLease(ctx context.Context, leaseName, holderID string, ttl time.Duration) (bool, error) {
	now := s.now().Unix()
	item, err := attributevalue.MarshalMap(Lease{
		Name:      leaseName,
		HolderID:  holderID,
		ExpiresAt: s.now().Add(ttl).Unix(),
	})
	if err != nil {
		return false, fmt.Errorf("marshal lease: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           s.table(tableLeases),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(lease_name) OR expires_at < :now OR holder_id = :me"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberN{Value: fmt.Sprint(now)},
			":me":  &types.AttributeValueMemberS{Value: holderID},
		},
	})
	if isConditionalCheckFailed(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetLease reads a lease's current holder without acquiring or mutating it
// (observability / tests — e.g. confirming shard leases actually split
// across controller processes).
func (s *Store) GetLease(ctx context.Context, leaseName string) (*Lease, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: s.table(tableLeases),
		Key: map[string]types.AttributeValue{
			"lease_name": &types.AttributeValueMemberS{Value: leaseName},
		},
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, ErrNotFound
	}
	var l Lease
	if err := attributevalue.UnmarshalMap(out.Item, &l); err != nil {
		return nil, fmt.Errorf("unmarshal lease: %w", err)
	}
	return &l, nil
}

// ReleaseLease deletes the lease if held by this instance, so the standby
// promotes without waiting for TTL expiry.
func (s *Store) ReleaseLease(ctx context.Context, leaseName, holderID string) error {
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: s.table(tableLeases),
		Key: map[string]types.AttributeValue{
			"lease_name": &types.AttributeValueMemberS{Value: leaseName},
		},
		ConditionExpression: aws.String("holder_id = :me"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":me": &types.AttributeValueMemberS{Value: holderID},
		},
	})
	if isConditionalCheckFailed(err) {
		return nil // someone else took it already; nothing to release
	}
	return err
}
