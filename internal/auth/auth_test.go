package auth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
)

// presignedURL builds a syntactically valid presigned GetCallerIdentity URL.
func presignedURL(host, date string, expires int, signedHeaders string) string {
	q := url.Values{}
	q.Set("Action", "GetCallerIdentity")
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", "AKIAEXAMPLE/20260712/us-east-1/sts/aws4_request")
	q.Set("X-Amz-Date", date)
	q.Set("X-Amz-Expires", fmt.Sprint(expires))
	q.Set("X-Amz-SignedHeaders", signedHeaders)
	q.Set("X-Amz-Signature", "deadbeef")
	return "https://" + host + "/?" + q.Encode()
}

func encodeToken(rawURL string) string {
	return TokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(rawURL))
}

var testNow = time.Date(2026, 7, 12, 0, 5, 0, 0, time.UTC)

func testVerifier(serverID string) *STSVerifier {
	v := NewSTSVerifier(serverID)
	v.now = func() time.Time { return testNow }
	return v
}

func TestValidateTokenRejections(t *testing.T) {
	v := testVerifier("arena.example.com")
	good := presignedURL("sts.amazonaws.com", "20260712T000000Z", 900, "host;x-arena-server")

	cases := map[string]string{
		"missing prefix":     base64.RawURLEncoding.EncodeToString([]byte(good)),
		"not base64":         TokenPrefix + "!!!",
		"non-sts host":       encodeToken(presignedURL("evil.example.com", "20260712T000000Z", 900, "host;x-arena-server")),
		"lookalike host":     encodeToken(presignedURL("sts.amazonaws.com.evil.io", "20260712T000000Z", 900, "host;x-arena-server")),
		"wrong action":       encodeToken("https://sts.amazonaws.com/?Action=AssumeRole&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=x&X-Amz-Date=20260712T000000Z&X-Amz-Expires=900&X-Amz-SignedHeaders=host%3Bx-arena-server"),
		"unbound to server":  encodeToken(presignedURL("sts.amazonaws.com", "20260712T000000Z", 900, "host")),
		"expired":            encodeToken(presignedURL("sts.amazonaws.com", "20260711T000000Z", 900, "host;x-arena-server")),
		"lifetime too long":  encodeToken(presignedURL("sts.amazonaws.com", "20260712T000000Z", 3600, "host;x-arena-server")),
		"plain http/no auth": encodeToken("http://sts.amazonaws.com/?Action=GetCallerIdentity"),
	}
	for label, token := range cases {
		if _, _, err := v.validateToken(token); err == nil {
			t.Errorf("%s: token accepted", label)
		}
	}

	if _, _, err := v.validateToken(encodeToken(good)); err != nil {
		t.Fatalf("well-formed token rejected: %v", err)
	}
}

func TestVerifyCallsSTSAndCaches(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get(ServerIDHeader); got != "arena.example.com" {
			t.Errorf("sts saw %s = %q", ServerIDHeader, got)
		}
		fmt.Fprint(w, `{"GetCallerIdentityResponse":{"GetCallerIdentityResult":{"Arn":"arn:aws:sts::123:assumed-role/matchmaker-prod/session"}}}`)
	}))
	defer srv.Close()

	v := testVerifier("arena.example.com")
	v.client = srv.Client()
	v.allowHost = func(string) bool { return true }

	u, _ := url.Parse(srv.URL)
	token := encodeToken(presignedURL(u.Host, "20260712T000000Z", 900, "host;x-arena-server"))

	arn, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if arn != "arn:aws:sts::123:assumed-role/matchmaker-prod/session" {
		t.Errorf("arn = %q", arn)
	}
	// Second verify is served from cache.
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("STS calls = %d, want 1 (cached)", calls.Load())
	}
}

func TestVerifyRejectsSTSDenial(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "signature does not match", http.StatusForbidden)
	}))
	defer srv.Close()

	v := testVerifier("arena.example.com")
	v.client = srv.Client()
	v.allowHost = func(string) bool { return true }

	u, _ := url.Parse(srv.URL)
	token := encodeToken(presignedURL(u.Host, "20260712T000000Z", 900, "host;x-arena-server"))
	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("denied token verified")
	}
}

func TestNormalizeARN(t *testing.T) {
	got := NormalizeARN("arn:aws:sts::123456789012:assumed-role/matchmaker-prod/i-abc123")
	if got != "arn:aws:iam::123456789012:role/matchmaker-prod" {
		t.Errorf("normalized = %q", got)
	}
	plain := "arn:aws:iam::123456789012:role/arena-admin"
	if NormalizeARN(plain) != plain {
		t.Error("plain role ARN must pass through")
	}
}

const testBindings = `
bindings:
  - principal: "arn:aws:iam::123:role/arena-admin"
    role: admin
  - principal: "arn:aws:iam::123:role/arena-ci-apply"
    role: fleet-editor
    namespaces: ["default", "shooter-*"]
  - principal: "arn:aws:iam::123:role/matchmaker-prod"
    role: allocator
    namespaces: ["shooter-*"]
`

func testAuthorizer(t *testing.T) *Authorizer {
	t.Helper()
	cfg, err := ParseConfig([]byte(testBindings))
	if err != nil {
		t.Fatal(err)
	}
	return NewAuthorizer(cfg)
}

func TestAuthorize(t *testing.T) {
	a := testAuthorizer(t)
	admin := "arn:aws:iam::123:role/arena-admin"
	ci := "arn:aws:sts::123:assumed-role/arena-ci-apply/gha" // session ARN normalizes
	mm := "arn:aws:iam::123:role/matchmaker-prod"

	cases := []struct {
		name      string
		principal string
		procedure string
		namespace string
		allowed   bool
	}{
		{"admin anything", admin, arenav1connect.FleetServiceDeleteFleetProcedure, "any-ns", true},
		{"ci applies in scope", ci, arenav1connect.FleetServiceApplyFleetProcedure, "shooter-jp", true},
		{"ci applies default", ci, arenav1connect.FleetServiceApplyFleetProcedure, "default", true},
		{"ci out of namespace", ci, arenav1connect.FleetServiceApplyFleetProcedure, "casino", false},
		{"ci cannot allocate", ci, arenav1connect.AllocationServiceAllocateProcedure, "shooter-jp", false},
		{"mm allocates in scope", mm, arenav1connect.AllocationServiceAllocateProcedure, "shooter-eu", true},
		{"mm cannot edit fleets", mm, arenav1connect.FleetServiceApplyFleetProcedure, "shooter-eu", false},
		{"mm reads gameservers", mm, arenav1connect.GameServerServiceGetGameServerProcedure, "shooter-eu", true},
		{"mm release (no namespace)", mm, arenav1connect.AllocationServiceReleaseProcedure, "", true},
		{"unknown principal", "arn:aws:iam::999:role/who", arenav1connect.FleetServiceGetFleetProcedure, "default", false},
	}
	for _, tc := range cases {
		err := a.Authorize(tc.principal, tc.procedure, tc.namespace)
		if (err == nil) != tc.allowed {
			t.Errorf("%s: err=%v, want allowed=%v", tc.name, err, tc.allowed)
		}
	}
}

func TestParseConfigRejectsUnknownRole(t *testing.T) {
	if _, err := ParseConfig([]byte("bindings:\n  - principal: x\n    role: superuser")); err == nil {
		t.Fatal("unknown role accepted")
	}
	if _, err := ParseConfig([]byte("bindings:\n  - role: viewer")); err == nil {
		t.Fatal("missing principal accepted")
	}
}

// staticVerifier resolves any token to a fixed ARN.
type staticVerifier string

func (s staticVerifier) Verify(context.Context, string) (string, error) { return string(s), nil }

// fixedFleet is a minimal FleetService for interceptor integration tests.
type fixedFleet struct {
	arenav1connect.UnimplementedFleetServiceHandler
}

func (fixedFleet) GetFleet(context.Context, *connect.Request[arenav1.GetFleetRequest]) (*connect.Response[arenav1.Fleet], error) {
	return connect.NewResponse(&arenav1.Fleet{Name: "ok"}), nil
}

func TestInterceptorEndToEnd(t *testing.T) {
	a := testAuthorizer(t)
	mux := http.NewServeMux()
	mux.Handle(arenav1connect.NewFleetServiceHandler(fixedFleet{},
		connect.WithInterceptors(NewInterceptor(staticVerifier("arn:aws:iam::123:role/matchmaker-prod"), a, nil))))
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	defer srv.Close()

	client := arenav1connect.NewFleetServiceClient(srv.Client(), srv.URL)

	// No token → UNAUTHENTICATED.
	_, err := client.GetFleet(context.Background(), connect.NewRequest(&arenav1.GetFleetRequest{Namespace: "shooter-jp", Name: "x"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no-token err = %v, want UNAUTHENTICATED", err)
	}

	authed := connect.NewRequest(&arenav1.GetFleetRequest{Namespace: "shooter-jp", Name: "x"})
	authed.Header().Set("Authorization", "Bearer whatever")
	if _, err := client.GetFleet(context.Background(), authed); err != nil {
		t.Fatalf("in-scope read: %v", err)
	}

	// Out-of-namespace → PERMISSION_DENIED.
	denied := connect.NewRequest(&arenav1.GetFleetRequest{Namespace: "casino", Name: "x"})
	denied.Header().Set("Authorization", "Bearer whatever")
	_, err = client.GetFleet(context.Background(), denied)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("out-of-scope err = %v, want PERMISSION_DENIED", err)
	}
}
