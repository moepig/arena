package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// SchemaClient is the DynamoDB surface EnsureTables needs (table admin, not
// part of the runtime Client interface — production tables come from
// Terraform; this exists for DynamoDB Local and tests).
type SchemaClient interface {
	CreateTable(ctx context.Context, in *dynamodb.CreateTableInput, opts ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(ctx context.Context, in *dynamodb.DescribeTableInput, opts ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	UpdateTimeToLive(ctx context.Context, in *dynamodb.UpdateTimeToLiveInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error)
}

// EnsureTables creates the four arena tables with the given name prefix,
// waiting until they are ACTIVE. Existing tables are left as-is.
func EnsureTables(ctx context.Context, db SchemaClient, prefix string) error {
	attr := func(name string, t types.ScalarAttributeType) types.AttributeDefinition {
		return types.AttributeDefinition{AttributeName: aws.String(name), AttributeType: t}
	}
	hashKey := func(name string) []types.KeySchemaElement {
		return []types.KeySchemaElement{{AttributeName: aws.String(name), KeyType: types.KeyTypeHash}}
	}
	hashRangeKey := func(hash, rng string) []types.KeySchemaElement {
		return []types.KeySchemaElement{
			{AttributeName: aws.String(hash), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(rng), KeyType: types.KeyTypeRange},
		}
	}
	projectAll := &types.Projection{ProjectionType: types.ProjectionTypeAll}

	tables := []*dynamodb.CreateTableInput{
		{
			TableName: aws.String(prefix + tableFleets),
			AttributeDefinitions: []types.AttributeDefinition{
				attr("fleet_id", types.ScalarAttributeTypeS),
				attr("namespace", types.ScalarAttributeTypeS),
				attr("name", types.ScalarAttributeTypeS),
			},
			KeySchema:   hashKey("fleet_id"),
			BillingMode: types.BillingModePayPerRequest,
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String(indexNamespaceName),
				KeySchema:  hashRangeKey("namespace", "name"),
				Projection: projectAll,
			}},
		},
		{
			TableName: aws.String(prefix + tableGameServers),
			AttributeDefinitions: []types.AttributeDefinition{
				attr("gameserver_id", types.ScalarAttributeTypeS),
				attr("fleet_id", types.ScalarAttributeTypeS),
				attr("state_created", types.ScalarAttributeTypeS),
			},
			KeySchema:   hashKey("gameserver_id"),
			BillingMode: types.BillingModePayPerRequest,
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String(indexFleet),
				KeySchema:  hashRangeKey("fleet_id", "state_created"),
				Projection: projectAll,
			}},
		},
		{
			TableName: aws.String(prefix + tableAllocations),
			AttributeDefinitions: []types.AttributeDefinition{
				attr("allocation_id", types.ScalarAttributeTypeS),
				attr("session_id", types.ScalarAttributeTypeS),
				attr("gameserver_id", types.ScalarAttributeTypeS),
				attr("allocated_at", types.ScalarAttributeTypeN),
			},
			KeySchema:   hashKey("allocation_id"),
			BillingMode: types.BillingModePayPerRequest,
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
				{
					IndexName:  aws.String(indexSession),
					KeySchema:  hashKey("session_id"),
					Projection: projectAll,
				},
				{
					IndexName:  aws.String(indexGameServer),
					KeySchema:  hashRangeKey("gameserver_id", "allocated_at"),
					Projection: projectAll,
				},
			},
		},
		{
			TableName: aws.String(prefix + tableLeases),
			AttributeDefinitions: []types.AttributeDefinition{
				attr("lease_name", types.ScalarAttributeTypeS),
			},
			KeySchema:   hashKey("lease_name"),
			BillingMode: types.BillingModePayPerRequest,
		},
		{
			TableName: aws.String(prefix + tableEvents),
			AttributeDefinitions: []types.AttributeDefinition{
				attr("resource", types.ScalarAttributeTypeS),
				attr("ts", types.ScalarAttributeTypeN),
			},
			KeySchema:   hashRangeKey("resource", "ts"),
			BillingMode: types.BillingModePayPerRequest,
		},
	}

	// TTL attributes (terminated gameservers and old allocations expire
	// automatically; events after 7 days).
	ttlTables := map[string]bool{tableGameServers: true, tableAllocations: true, tableEvents: true}

	for _, in := range tables {
		if _, err := db.CreateTable(ctx, in); err != nil {
			var exists *types.ResourceInUseException
			if errors.As(err, &exists) {
				continue
			}
			return fmt.Errorf("create table %s: %w", aws.ToString(in.TableName), err)
		}
		if err := waitTableActive(ctx, db, aws.ToString(in.TableName)); err != nil {
			return err
		}
		base := aws.ToString(in.TableName)[len(prefix):]
		if !ttlTables[base] {
			continue
		}
		if _, err := db.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName: in.TableName,
			TimeToLiveSpecification: &types.TimeToLiveSpecification{
				AttributeName: aws.String("ttl"),
				Enabled:       aws.Bool(true),
			},
		}); err != nil {
			return fmt.Errorf("enable ttl on %s: %w", aws.ToString(in.TableName), err)
		}
	}
	return nil
}

func waitTableActive(ctx context.Context, db SchemaClient, name string) error {
	for i := 0; i < 60; i++ {
		out, err := db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
		if err == nil && out.Table.TableStatus == types.TableStatusActive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("table %s never became active", name)
}
