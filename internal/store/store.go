// Package store owns DynamoDB access and enforces the GameServer state
// machine with conditional writes. DynamoDB is the single source of
// truth; Redis holds only derived data (internal/pool).
package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Table and index names. Physical names get an environment prefix via
// Options.TablePrefix (e.g. "arena-prod-" + "fleets").
const (
	tableFleets      = "fleets"
	tableGameServers = "gameservers"
	tableAllocations = "allocations"
	tableLeases      = "leases"
	tableEvents      = "events"

	indexNamespaceName = "namespace-name-index"
	indexFleet         = "fleet-index"
	indexSession       = "session-index"
	indexGameServer    = "gameserver-index"
)

// Sentinel errors. Callers map these to API codes.
var (
	// ErrNotFound: the requested item does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrVersionConflict: optimistic-lock version mismatch (→ ABORTED).
	ErrVersionConflict = errors.New("store: version conflict")
	// ErrConditionFailed: a state-machine or existence condition was
	// rejected; the caller moves on and lets reconcile converge.
	ErrConditionFailed = errors.New("store: condition failed")
	// ErrAlreadyExists: a create hit an existing item.
	ErrAlreadyExists = errors.New("store: already exists")
)

// Client is the subset of the DynamoDB API the store uses; *dynamodb.Client
// satisfies it and tests substitute fakes.
type Client interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	Scan(ctx context.Context, in *dynamodb.ScanInput, opts ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// Options configures a Store.
type Options struct {
	// TablePrefix is prepended to every table name.
	TablePrefix string
	// Now overrides the clock (tests). Defaults to time.Now.
	Now func() time.Time
}

// Store provides typed access to the four arena tables.
type Store struct {
	db     Client
	prefix string
	now    func() time.Time
}

// New returns a Store backed by db.
func New(db Client, opts Options) *Store {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, prefix: opts.TablePrefix, now: now}
}

func (s *Store) table(name string) *string {
	t := s.prefix + name
	return &t
}

// isConditionalCheckFailed reports whether err is a DynamoDB conditional
// check failure (single-item writes).
func isConditionalCheckFailed(err error) bool {
	var ccf *types.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

// isTransactionConflict reports whether err is a cancelled transaction due
// to a failed condition.
func isTransactionConditionFailed(err error) bool {
	var tc *types.TransactionCanceledException
	if !errors.As(err, &tc) {
		return false
	}
	for _, r := range tc.CancellationReasons {
		if r.Code != nil && *r.Code == "ConditionalCheckFailed" {
			return true
		}
	}
	return false
}

// pageToken encodes a DynamoDB ExclusiveStartKey as an opaque string.
func encodePageToken(key map[string]types.AttributeValue) (string, error) {
	if len(key) == 0 {
		return "", nil
	}
	var m map[string]any
	if err := attributevalue.UnmarshalMap(key, &m); err != nil {
		return "", fmt.Errorf("encode page token: %w", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode page token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodePageToken(token string) (map[string]types.AttributeValue, error) {
	if token == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("invalid page token")
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("invalid page token")
	}
	key, err := attributevalue.MarshalMap(m)
	if err != nil {
		return nil, fmt.Errorf("invalid page token")
	}
	return key, nil
}
