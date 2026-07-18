package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
)

// namespaced is implemented by request messages carrying a namespace.
type namespaced interface{ GetNamespace() string }

// TokenVerifier resolves a bearer token to an IAM principal ARN
// (*STSVerifier in production; tests substitute fakes).
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (string, error)
}

// NewInterceptor returns the authn → authz interceptor chain for the
// control-plane services. The SDK Gateway is not behind
// it — sidecar identity is the Task-ARN check, a separate trust domain.
// Mutating RPCs are audit-logged with the authenticated principal.
func NewInterceptor(verifier TokenVerifier, authz *Authorizer, log *slog.Logger) connect.UnaryInterceptorFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if req.Spec().IsClient {
				return next(ctx, req)
			}

			token, ok := bearerToken(req.Header().Get("Authorization"))
			if !ok {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					errors.New("missing bearer token (Authorization header)"))
			}
			arn, err := verifier.Verify(ctx, token)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}

			procedure := req.Spec().Procedure
			ns := ""
			if n, ok := req.Any().(namespaced); ok {
				if ns = n.GetNamespace(); ns == "" {
					ns = "default"
				}
			}
			if err := authz.Authorize(arn, procedure, ns); err != nil {
				return nil, connect.NewError(connect.CodePermissionDenied, err)
			}

			if !ReadOnlyProcedure(procedure) {
				// Audit trail for every mutating RPC.
				log.Info("audit", "principal", NormalizeARN(arn), "procedure", procedure, "namespace", ns)
			}
			return next(ctx, req)
		}
	}
}

func bearerToken(header string) (string, bool) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	return token, ok && token != ""
}
