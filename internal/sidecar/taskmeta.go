package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// metadataEnv is the ECS-injected container metadata endpoint (v4).
const metadataEnv = "ECS_CONTAINER_METADATA_URI_V4"

// DiscoverTaskARN reads the task ARN from the ECS task metadata endpoint,
// used by the gateway to verify the sidecar's claimed gameserver_id.
// Returns "" off-ECS (local development).
func DiscoverTaskARN(ctx context.Context) (string, error) {
	base := os.Getenv(metadataEnv)
	if base == "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/task", nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("task metadata: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("task metadata: status %d", res.StatusCode)
	}
	var meta struct {
		TaskARN string `json:"TaskARN"`
	}
	if err := json.NewDecoder(res.Body).Decode(&meta); err != nil {
		return "", fmt.Errorf("task metadata: %w", err)
	}
	return meta.TaskARN, nil
}
