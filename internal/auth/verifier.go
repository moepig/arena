package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// stsHostPattern matches legitimate STS endpoints (global + regional).
var stsHostPattern = regexp.MustCompile(`^sts(\.[a-z0-9-]+)?\.amazonaws\.com(\.cn)?$`)

// maxTokenLifetime caps X-Amz-Expires (STS's own presign ceiling).
const maxTokenLifetime = 15 * time.Minute

// amzDateFormat is the SigV4 timestamp layout.
const amzDateFormat = "20060102T150405Z"

// STSVerifier validates bearer tokens by executing the presigned
// GetCallerIdentity call and caching token → ARN until the token expires,
// so the hot path does not call STS per request.
type STSVerifier struct {
	serverID  string
	client    *http.Client
	allowHost func(string) bool
	now       func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cacheEntry
}

type cacheEntry struct {
	arn     string
	expires time.Time
}

// NewSTSVerifier returns a verifier for tokens bound to serverID.
func NewSTSVerifier(serverID string) *STSVerifier {
	return &STSVerifier{
		serverID:  serverID,
		client:    &http.Client{Timeout: 10 * time.Second},
		allowHost: stsHostPattern.MatchString,
		now:       time.Now,
		cache:     map[[sha256.Size]byte]cacheEntry{},
	}
}

// Verify resolves a bearer token to the caller's IAM principal ARN.
func (v *STSVerifier) Verify(ctx context.Context, token string) (string, error) {
	presignedURL, expires, err := v.validateToken(token)
	if err != nil {
		return "", err
	}

	key := sha256.Sum256([]byte(token))
	v.mu.Lock()
	if e, ok := v.cache[key]; ok && v.now().Before(e.expires) {
		v.mu.Unlock()
		return e.arn, nil
	}
	v.mu.Unlock()

	arn, err := v.callSTS(ctx, presignedURL)
	if err != nil {
		return "", err
	}

	v.mu.Lock()
	if len(v.cache) > 100_000 { // safety valve
		v.cache = map[[sha256.Size]byte]cacheEntry{}
	}
	v.cache[key] = cacheEntry{arn: arn, expires: expires}
	v.mu.Unlock()
	return arn, nil
}

// validateToken checks everything that can be checked without calling STS:
// format, endpoint, action, server binding, and expiry window.
func (v *STSVerifier) validateToken(token string) (string, time.Time, error) {
	raw, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok {
		return "", time.Time{}, errors.New("token missing arena-v1 prefix")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", time.Time{}, errors.New("token is not base64")
	}
	u, err := url.Parse(string(decoded))
	if err != nil {
		return "", time.Time{}, errors.New("token is not a URL")
	}
	if u.Scheme != "https" || !v.allowHost(u.Host) {
		return "", time.Time{}, fmt.Errorf("token host %q is not an STS endpoint", u.Host)
	}

	q := u.Query()
	if q.Get("Action") != "GetCallerIdentity" {
		return "", time.Time{}, fmt.Errorf("token action %q is not GetCallerIdentity", q.Get("Action"))
	}
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || q.Get("X-Amz-Credential") == "" {
		return "", time.Time{}, errors.New("token is not SigV4-presigned")
	}
	// The server binding must be part of the signature, or the token could
	// be replayed against arena from a URL presigned for something else.
	signed := strings.Split(strings.ToLower(q.Get("X-Amz-SignedHeaders")), ";")
	if !contains(signed, ServerIDHeader) {
		return "", time.Time{}, fmt.Errorf("token does not sign the %s header", ServerIDHeader)
	}

	date, err := time.Parse(amzDateFormat, q.Get("X-Amz-Date"))
	if err != nil {
		return "", time.Time{}, errors.New("token has no valid X-Amz-Date")
	}
	lifetime, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || lifetime <= 0 || time.Duration(lifetime)*time.Second > maxTokenLifetime {
		return "", time.Time{}, errors.New("token lifetime is missing or exceeds 15 minutes")
	}
	expires := date.Add(time.Duration(lifetime) * time.Second)
	if now := v.now(); now.After(expires) || date.After(now.Add(5*time.Minute)) {
		return "", time.Time{}, errors.New("token is expired or not yet valid")
	}
	return u.String(), expires, nil
}

// callSTS executes the presigned call. The x-arena-server header must match
// the value the client signed, or STS rejects the signature — that is the
// replay protection doing its job.
func (v *STSVerifier) callSTS(ctx context.Context, presignedURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, presignedURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(ServerIDHeader, v.serverID)
	req.Header.Set("Accept", "application/json")

	res, err := v.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call sts: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read sts response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sts rejected the token (status %d)", res.StatusCode)
	}

	var out struct {
		GetCallerIdentityResponse struct {
			GetCallerIdentityResult struct {
				Arn string `json:"Arn"`
			} `json:"GetCallerIdentityResult"`
		} `json:"GetCallerIdentityResponse"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse sts response: %w", err)
	}
	arn := out.GetCallerIdentityResponse.GetCallerIdentityResult.Arn
	if arn == "" {
		return "", errors.New("sts response carried no ARN")
	}
	return arn, nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
