package store

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// CreateFleet inserts a new Fleet. The caller assigns ID, SpecHash and
// initial Version (1). Fails with ErrAlreadyExists when the fleet_id is
// taken; (namespace, name) uniqueness is enforced by GetFleetByName before
// the write plus the id being a fresh UUID — a racing duplicate is caught
// by reconcile listing, not by this call.
func (s *Store) CreateFleet(ctx context.Context, f Fleet) error {
	now := s.now().Unix()
	f.CreatedAt, f.UpdatedAt = now, now
	item, err := attributevalue.MarshalMap(f)
	if err != nil {
		return fmt.Errorf("marshal fleet: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           s.table(tableFleets),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(fleet_id)"),
	})
	if isConditionalCheckFailed(err) {
		return ErrAlreadyExists
	}
	return err
}

// GetFleet fetches a Fleet by internal ID.
func (s *Store) GetFleet(ctx context.Context, fleetID string) (*Fleet, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: s.table(tableFleets),
		Key: map[string]types.AttributeValue{
			"fleet_id": &types.AttributeValueMemberS{Value: fleetID},
		},
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, ErrNotFound
	}
	var f Fleet
	if err := attributevalue.UnmarshalMap(out.Item, &f); err != nil {
		return nil, fmt.Errorf("unmarshal fleet: %w", err)
	}
	return &f, nil
}

// GetFleetByName resolves a Fleet by its user-facing identity
// (namespace, name) via the namespace-name-index GSI.
func (s *Store) GetFleetByName(ctx context.Context, namespace, name string) (*Fleet, error) {
	out, err := s.db.Query(ctx, &dynamodb.QueryInput{
		TableName:              s.table(tableFleets),
		IndexName:              aws.String(indexNamespaceName),
		KeyConditionExpression: aws.String("#ns = :ns AND #n = :n"),
		ExpressionAttributeNames: map[string]string{
			"#ns": "namespace",
			"#n":  "name",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ns": &types.AttributeValueMemberS{Value: namespace},
			":n":  &types.AttributeValueMemberS{Value: name},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, ErrNotFound
	}
	var f Fleet
	if err := attributevalue.UnmarshalMap(out.Items[0], &f); err != nil {
		return nil, fmt.Errorf("unmarshal fleet: %w", err)
	}
	return &f, nil
}

// ListFleets pages through the fleets of a namespace.
func (s *Store) ListFleets(ctx context.Context, namespace string, pageSize int32, pageToken string) ([]Fleet, string, error) {
	startKey, err := decodePageToken(pageToken)
	if err != nil {
		return nil, "", err
	}
	out, err := s.db.Query(ctx, &dynamodb.QueryInput{
		TableName:              s.table(tableFleets),
		IndexName:              aws.String(indexNamespaceName),
		KeyConditionExpression: aws.String("#ns = :ns"),
		ExpressionAttributeNames: map[string]string{
			"#ns": "namespace",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ns": &types.AttributeValueMemberS{Value: namespace},
		},
		Limit:             aws.Int32(pageSize),
		ExclusiveStartKey: startKey,
	})
	if err != nil {
		return nil, "", err
	}
	fleets := make([]Fleet, 0, len(out.Items))
	for _, item := range out.Items {
		var f Fleet
		if err := attributevalue.UnmarshalMap(item, &f); err != nil {
			return nil, "", fmt.Errorf("unmarshal fleet: %w", err)
		}
		fleets = append(fleets, f)
	}
	next, err := encodePageToken(out.LastEvaluatedKey)
	if err != nil {
		return nil, "", err
	}
	return fleets, next, nil
}

// ListAllFleetsByNamespace drains every fleet in a namespace off the
// namespace GSI (all pages) — the fleet_selector resolution source;
// callers cache the result since it isn't cheap to call per-Allocate.
func (s *Store) ListAllFleetsByNamespace(ctx context.Context, namespace string) ([]Fleet, error) {
	var all []Fleet
	var startKey map[string]types.AttributeValue
	for {
		out, err := s.db.Query(ctx, &dynamodb.QueryInput{
			TableName:              s.table(tableFleets),
			IndexName:              aws.String(indexNamespaceName),
			KeyConditionExpression: aws.String("#ns = :ns"),
			ExpressionAttributeNames: map[string]string{
				"#ns": "namespace",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":ns": &types.AttributeValueMemberS{Value: namespace},
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range out.Items {
			var f Fleet
			if err := attributevalue.UnmarshalMap(item, &f); err != nil {
				return nil, fmt.Errorf("unmarshal fleet: %w", err)
			}
			all = append(all, f)
		}
		if out.LastEvaluatedKey == nil {
			return all, nil
		}
		startKey = out.LastEvaluatedKey
	}
}

// ListAllFleets scans every fleet across namespaces. Controller-only
// (resync enumeration, ≤1,000 fleets); user-facing listing goes through
// the namespace GSI.
func (s *Store) ListAllFleets(ctx context.Context) ([]Fleet, error) {
	var all []Fleet
	var startKey map[string]types.AttributeValue
	for {
		out, err := s.db.Scan(ctx, &dynamodb.ScanInput{
			TableName:         s.table(tableFleets),
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range out.Items {
			var f Fleet
			if err := attributevalue.UnmarshalMap(item, &f); err != nil {
				return nil, fmt.Errorf("unmarshal fleet: %w", err)
			}
			all = append(all, f)
		}
		if out.LastEvaluatedKey == nil {
			return all, nil
		}
		startKey = out.LastEvaluatedKey
	}
}

// UpdateFleet replaces the fleet item, conditioned on the version the
// caller read (optimistic lock). f.Version must be the *current* stored
// version; the write bumps it by one. Returns ErrVersionConflict when the
// item changed underneath (→ ABORTED).
func (s *Store) UpdateFleet(ctx context.Context, f Fleet) (*Fleet, error) {
	expected := f.Version
	f.Version = expected + 1
	f.UpdatedAt = s.now().Unix()
	item, err := attributevalue.MarshalMap(f)
	if err != nil {
		return nil, fmt.Errorf("marshal fleet: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           s.table(tableFleets),
		Item:                item,
		ConditionExpression: aws.String("attribute_exists(fleet_id) AND version = :v"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":v": &types.AttributeValueMemberN{Value: fmt.Sprint(expected)},
		},
	})
	if isConditionalCheckFailed(err) {
		return nil, ErrVersionConflict
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// UpdateFleetStatus writes the controller-observed status counts without
// touching spec or version ownership rules. It still bumps version so
// concurrent spec updates are detected, conditioned on the version read.
func (s *Store) UpdateFleetStatus(ctx context.Context, fleetID string, version int64, st FleetStatus) error {
	statusAV, err := attributevalue.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	_, err = s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: s.table(tableFleets),
		Key: map[string]types.AttributeValue{
			"fleet_id": &types.AttributeValueMemberS{Value: fleetID},
		},
		UpdateExpression:    aws.String("SET #st = :st, version = :nv, updated_at = :now"),
		ConditionExpression: aws.String("attribute_exists(fleet_id) AND version = :v"),
		ExpressionAttributeNames: map[string]string{
			"#st": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":st":  statusAV,
			":v":   &types.AttributeValueMemberN{Value: fmt.Sprint(version)},
			":nv":  &types.AttributeValueMemberN{Value: fmt.Sprint(version + 1)},
			":now": &types.AttributeValueMemberN{Value: fmt.Sprint(s.now().Unix())},
		},
	})
	if isConditionalCheckFailed(err) {
		return ErrVersionConflict
	}
	return err
}

// DeleteFleet removes the fleet item. Deleting a fleet with live
// GameServers is prevented at the API layer (scale to zero first).
func (s *Store) DeleteFleet(ctx context.Context, fleetID string) error {
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: s.table(tableFleets),
		Key: map[string]types.AttributeValue{
			"fleet_id": &types.AttributeValueMemberS{Value: fleetID},
		},
		ConditionExpression: aws.String("attribute_exists(fleet_id)"),
	})
	if isConditionalCheckFailed(err) {
		return ErrNotFound
	}
	return err
}
