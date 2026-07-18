package controller

// Webhook autoscaler policy: POST the
// fleet's observed status, receive a desired replica count. Calls are
// protected by a timeout + per-URL circuit breaker (failsafe-go); any
// failure means "no opinion" and the current count is kept.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/timeout"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/store"
)

const webhookTimeout = 3 * time.Second

// webhookRequest is the POST body.
type webhookRequest struct {
	Fleet  webhookFleet  `json:"fleet"`
	Status webhookStatus `json:"status"`
}

type webhookFleet struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type webhookStatus struct {
	Replicas  int32                             `json:"replicas"`
	Total     int32                             `json:"total"`
	Ready     int32                             `json:"ready"`
	Allocated int32                             `json:"allocated"`
	Reserved  int32                             `json:"reserved"`
	Starting  int32                             `json:"starting"`
	Counters  map[string]store.CounterAggregate `json:"counters,omitempty"`
}

// webhookResponse is the expected reply. A null replicas = no opinion.
type webhookResponse struct {
	Replicas *int32 `json:"replicas"`
}

// webhookCaller keeps one circuit breaker per endpoint URL.
type webhookCaller struct {
	client *http.Client

	mu       sync.Mutex
	breakers map[string]circuitbreaker.CircuitBreaker[[]byte]
}

func newWebhookCaller() *webhookCaller {
	return &webhookCaller{
		client:   &http.Client{Timeout: webhookTimeout + time.Second},
		breakers: map[string]circuitbreaker.CircuitBreaker[[]byte]{},
	}
}

func (w *webhookCaller) breaker(url string) circuitbreaker.CircuitBreaker[[]byte] {
	w.mu.Lock()
	defer w.mu.Unlock()
	cb, ok := w.breakers[url]
	if !ok {
		cb = circuitbreaker.NewBuilder[[]byte]().
			WithFailureThreshold(3).
			WithDelay(30 * time.Second).
			Build()
		w.breakers[url] = cb
	}
	return cb
}

// desired asks the webhook for a replica count. ok=false = no opinion
// (failure, open breaker, or an explicit null reply).
func (w *webhookCaller) desired(ctx context.Context, policy *arenav1.WebhookPolicy, fleet *store.Fleet, st store.FleetStatus) (int32, bool, error) {
	body, err := json.Marshal(webhookRequest{
		Fleet: webhookFleet{ID: fleet.ID, Namespace: fleet.Namespace, Name: fleet.Name},
		Status: webhookStatus{
			Replicas: fleet.Replicas, Total: st.Total, Ready: st.Ready,
			Allocated: st.Allocated, Reserved: st.Reserved, Starting: st.Starting,
			Counters: st.Counters,
		},
	})
	if err != nil {
		return 0, false, err
	}

	respBody, err := failsafe.With(w.breaker(policy.GetUrl()), timeout.New[[]byte](webhookTimeout)).
		WithContext(ctx).
		Get(func() ([]byte, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, policy.GetUrl(), bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			for k, v := range policy.GetHeaders() {
				req.Header.Set(k, v)
			}
			res, err := w.client.Do(req)
			if err != nil {
				return nil, err
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("webhook status %d", res.StatusCode)
			}
			return io.ReadAll(io.LimitReader(res.Body, 1<<16))
		})
	if err != nil {
		// Failure keeps the current count; the caller logs.
		return 0, false, err
	}
	var out webhookResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return 0, false, fmt.Errorf("webhook reply: %w", err)
	}
	if out.Replicas == nil {
		return 0, false, nil
	}
	return *out.Replicas, true, nil
}
