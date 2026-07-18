package store

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestSortKeyOrdering(t *testing.T) {
	// Lexicographic order of the composite SK must equal numeric order of
	// created_at within one state (zero padding).
	older := SortKey(StateReady, 1700000000)
	newer := SortKey(StateReady, 1700000001)
	if !(older < newer) {
		t.Errorf("SortKey ordering broken: %q >= %q", older, newer)
	}
	if got, want := SortKey(StateReady, 1700000000), "Ready#0001700000000"; got != want {
		t.Errorf("SortKey = %q, want %q", got, want)
	}
}

func TestPageTokenRoundTrip(t *testing.T) {
	key := map[string]types.AttributeValue{
		"fleet_id":      &types.AttributeValueMemberS{Value: "f-1"},
		"state_created": &types.AttributeValueMemberS{Value: "Ready#0001700000000"},
		"gameserver_id": &types.AttributeValueMemberS{Value: "gs-1"},
	}
	token, err := encodePageToken(key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodePageToken(token)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range key {
		gv, ok := got[k].(*types.AttributeValueMemberS)
		if !ok || gv.Value != v.(*types.AttributeValueMemberS).Value {
			t.Errorf("round trip lost %s", k)
		}
	}

	if tok, err := encodePageToken(nil); err != nil || tok != "" {
		t.Errorf("empty key should encode to empty token, got %q, %v", tok, err)
	}
	if key, err := decodePageToken(""); err != nil || key != nil {
		t.Errorf("empty token should decode to nil key, got %v, %v", key, err)
	}
	if _, err := decodePageToken("!!!not-base64!!!"); err == nil {
		t.Error("garbage token should fail")
	}
}
