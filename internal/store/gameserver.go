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

// terminatedTTL is how long Terminated records stay before DynamoDB TTL
// deletes them (audit window; long-term history is DynamoDB Streams).
const terminatedTTL = 24 * time.Hour

// SortKey builds the fleet-index composite sort key ("State#created_at").
// The zero-padded fixed width keeps lexicographic order == numeric order.
func SortKey(state State, createdAt int64) string {
	return fmt.Sprintf("%s#%013d", state, createdAt)
}

// StatePrefix is the begins_with prefix selecting one state on fleet-index.
func StatePrefix(state State) string {
	return string(state) + "#"
}

// PutGameServer inserts a new GameServer in state Scheduled (Version 1).
func (s *Store) PutGameServer(ctx context.Context, gs GameServer) error {
	now := s.now().Unix()
	gs.CreatedAt, gs.UpdatedAt = now, now
	gs.Version = 1
	gs.StateCreated = SortKey(gs.State, gs.CreatedAt)
	item, err := attributevalue.MarshalMap(gs)
	if err != nil {
		return fmt.Errorf("marshal gameserver: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           s.table(tableGameServers),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(gameserver_id)"),
	})
	if isConditionalCheckFailed(err) {
		return ErrAlreadyExists
	}
	return err
}

// GetGameServer fetches a GameServer by ID.
func (s *Store) GetGameServer(ctx context.Context, gsID string) (*GameServer, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: s.table(tableGameServers),
		Key: map[string]types.AttributeValue{
			"gameserver_id": &types.AttributeValueMemberS{Value: gsID},
		},
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, ErrNotFound
	}
	var gs GameServer
	if err := attributevalue.UnmarshalMap(out.Item, &gs); err != nil {
		return nil, fmt.Errorf("unmarshal gameserver: %w", err)
	}
	return &gs, nil
}

// ListGameServersByFleet pages a fleet's GameServers from the fleet-index
// GSI, optionally restricted to one state via begins_with on the composite
// sort key.
func (s *Store) ListGameServersByFleet(ctx context.Context, fleetID string, state State, pageSize int32, pageToken string) ([]GameServer, string, error) {
	startKey, err := decodePageToken(pageToken)
	if err != nil {
		return nil, "", err
	}
	in := &dynamodb.QueryInput{
		TableName:              s.table(tableGameServers),
		IndexName:              aws.String(indexFleet),
		KeyConditionExpression: aws.String("fleet_id = :f"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":f": &types.AttributeValueMemberS{Value: fleetID},
		},
		ExclusiveStartKey: startKey,
	}
	if pageSize > 0 {
		in.Limit = aws.Int32(pageSize)
	}
	if state != "" {
		in.KeyConditionExpression = aws.String("fleet_id = :f AND begins_with(state_created, :p)")
		in.ExpressionAttributeValues[":p"] = &types.AttributeValueMemberS{Value: StatePrefix(state)}
	}
	out, err := s.db.Query(ctx, in)
	if err != nil {
		return nil, "", err
	}
	gss := make([]GameServer, 0, len(out.Items))
	for _, item := range out.Items {
		var gs GameServer
		if err := attributevalue.UnmarshalMap(item, &gs); err != nil {
			return nil, "", fmt.Errorf("unmarshal gameserver: %w", err)
		}
		gss = append(gss, gs)
	}
	next, err := encodePageToken(out.LastEvaluatedKey)
	if err != nil {
		return nil, "", err
	}
	return gss, next, nil
}

// ListAllGameServersByFleet drains all pages; used by the reconciler, which
// needs the full fleet picture.
func (s *Store) ListAllGameServersByFleet(ctx context.Context, fleetID string, state State) ([]GameServer, error) {
	var all []GameServer
	token := ""
	for {
		page, next, err := s.ListGameServersByFleet(ctx, fleetID, state, 0, token)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			return all, nil
		}
		token = next
	}
}

// TransitionState moves a GameServer from → to, enforcing the state machine
// with a conditional write. mutate (optional) adjusts additional fields
// (address, ports, ...) between read and write. Returns ErrConditionFailed
// when the server is no longer in the "from" state or was updated
// concurrently — callers treat that as "someone else got there first" and
// let reconcile converge.
func (s *Store) TransitionState(ctx context.Context, gsID string, from, to State, mutate func(*GameServer)) (*GameServer, error) {
	if !CanTransition(from, to) {
		return nil, fmt.Errorf("%w: illegal transition %s -> %s", ErrConditionFailed, from, to)
	}
	gs, err := s.GetGameServer(ctx, gsID)
	if err != nil {
		return nil, err
	}
	if gs.State != from {
		return nil, fmt.Errorf("%w: state is %s, want %s", ErrConditionFailed, gs.State, from)
	}

	expected := gs.Version
	now := s.now()
	gs.State = to
	gs.StateCreated = SortKey(to, gs.CreatedAt)
	gs.Version = expected + 1
	gs.UpdatedAt = now.Unix()
	switch to {
	case StateReady:
		gs.ReadyAt = now.Unix()
	case StateAllocated:
		gs.AllocatedAt = now.Unix()
	case StateTerminated:
		gs.TTL = now.Add(terminatedTTL).Unix()
	}
	// ReservedUntil only has meaning while Reserved; a reservation (or an
	// extension) sets it via mutate.
	if to != StateReserved {
		gs.ReservedUntil = 0
	}
	if mutate != nil {
		mutate(gs)
	}

	item, err := attributevalue.MarshalMap(gs)
	if err != nil {
		return nil, fmt.Errorf("marshal gameserver: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           s.table(tableGameServers),
		Item:                item,
		ConditionExpression: aws.String("#st = :from AND version = :v"),
		ExpressionAttributeNames: map[string]string{
			"#st": "state",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":from": &types.AttributeValueMemberS{Value: string(from)},
			":v":    &types.AttributeValueMemberN{Value: fmt.Sprint(expected)},
		},
	})
	if isConditionalCheckFailed(err) {
		return nil, fmt.Errorf("%w: concurrent update on %s", ErrConditionFailed, gsID)
	}
	if err != nil {
		return nil, err
	}
	s.recordTransition(ctx, gs, from)
	return gs, nil
}

// recordTransition emits the state-transition event asynchronously — events
// are observability, never on the critical path. This is the single
// Transitioner hook covering every gameserver state change.
func (s *Store) recordTransition(ctx context.Context, gs *GameServer, from State) {
	typ := EventNormal
	if gs.State == StateUnhealthy {
		typ = EventWarning
	}
	bg := context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 5*time.Second)
		defer cancel()
		_ = s.PutEvent(ctx, EventResourceGameServer, gs.ID, typ, string(gs.State),
			fmt.Sprintf("%s -> %s", from, gs.State))
	}()
}

// MarkTerminated forces a GameServer to Terminated from whatever state it
// is in (task STOPPED confirmation — legal from every state). Idempotent:
// already-Terminated is a no-op.
func (s *Store) MarkTerminated(ctx context.Context, gsID string) (*GameServer, error) {
	gs, err := s.GetGameServer(ctx, gsID)
	if err != nil {
		return nil, err
	}
	if gs.State == StateTerminated {
		return gs, nil
	}
	return s.TransitionState(ctx, gsID, gs.State, StateTerminated, nil)
}

// UpdateGameServerMetadata replaces labels/annotations (SDK SetLabel /
// SetAnnotation), version-conditioned without changing state.
func (s *Store) UpdateGameServerMetadata(ctx context.Context, gsID string, mutate func(*GameServer)) (*GameServer, error) {
	gs, err := s.GetGameServer(ctx, gsID)
	if err != nil {
		return nil, err
	}
	expected := gs.Version
	gs.Version = expected + 1
	gs.UpdatedAt = s.now().Unix()
	mutate(gs)

	item, err := attributevalue.MarshalMap(gs)
	if err != nil {
		return nil, fmt.Errorf("marshal gameserver: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           s.table(tableGameServers),
		Item:                item,
		ConditionExpression: aws.String("version = :v"),
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
	return gs, nil
}

// ClaimGameServer atomically commits an allocation: the Ready → Allocated
// transition and the Allocation record are one TransactWriteItems, so a
// half-committed "ghost Allocated" cannot exist.
// mutate (optional) patches the GameServer in the same transaction — the
// game_server_metadata channel. ErrConditionFailed means the server left
// Ready (or the allocation_id was concurrently written); the allocator
// moves to the next candidate.
func (s *Store) ClaimGameServer(ctx context.Context, gsID string, alloc Allocation, mutate func(*GameServer)) (*GameServer, error) {
	return s.claimGameServer(ctx, gsID, []State{StateReady}, alloc, mutate)
}

// SelfAllocateGameServer is the SDK Allocate path: the server claims
// itself from Ready or Reserved. The synthesized Allocation record is
// committed in the same transaction as the state change.
func (s *Store) SelfAllocateGameServer(ctx context.Context, gsID string, alloc Allocation) (*GameServer, error) {
	return s.claimGameServer(ctx, gsID, []State{StateReady, StateReserved}, alloc, nil)
}

func (s *Store) claimGameServer(ctx context.Context, gsID string, from []State, alloc Allocation, mutate func(*GameServer)) (*GameServer, error) {
	gs, err := s.GetGameServer(ctx, gsID)
	if err != nil {
		return nil, err
	}
	claimable := false
	for _, st := range from {
		claimable = claimable || gs.State == st
	}
	if !claimable {
		return nil, fmt.Errorf("%w: state is %s, want one of %v", ErrConditionFailed, gs.State, from)
	}

	expected := gs.Version
	observed := gs.State
	now := s.now()
	gs.State = StateAllocated
	gs.StateCreated = SortKey(StateAllocated, gs.CreatedAt)
	gs.Version = expected + 1
	gs.AllocatedAt = now.Unix()
	gs.UpdatedAt = now.Unix()
	gs.ReservedUntil = 0
	if mutate != nil {
		mutate(gs)
	}

	alloc.GameServerID = gs.ID
	alloc.FleetID = gs.FleetID
	alloc.AllocatedAt = now.Unix()

	gsItem, err := attributevalue.MarshalMap(gs)
	if err != nil {
		return nil, fmt.Errorf("marshal gameserver: %w", err)
	}
	allocItem, err := attributevalue.MarshalMap(alloc)
	if err != nil {
		return nil, fmt.Errorf("marshal allocation: %w", err)
	}

	_, err = s.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{
				Put: &types.Put{
					TableName:           s.table(tableGameServers),
					Item:                gsItem,
					ConditionExpression: aws.String("#st = :from AND version = :v"),
					ExpressionAttributeNames: map[string]string{
						"#st": "state",
					},
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":from": &types.AttributeValueMemberS{Value: string(observed)},
						":v":    &types.AttributeValueMemberN{Value: fmt.Sprint(expected)},
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
		return nil, fmt.Errorf("%w: claim rejected for %s", ErrConditionFailed, gsID)
	}
	if err != nil {
		return nil, err
	}
	s.recordTransition(ctx, gs, observed)
	return gs, nil
}
