// Package auth implements the API identity layer: AWS IAM
// is the only identity provider. Clients present a presigned
// sts:GetCallerIdentity URL as a bearer token (the aws-iam-authenticator /
// Vault AWS-auth scheme); the server executes it to learn the caller's IAM
// ARN, then authorizes via a static role mapping (RBAC-lite). There are no
// arena-managed users, passwords, or long-lived API keys.
package auth

import (
	"context"
	"encoding/base64"
	"fmt"

	"connectrpc.com/connect"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const (
	// TokenPrefix versions the token format.
	TokenPrefix = "arena-v1."

	// ServerIDHeader binds the token to one arena deployment: it is signed
	// into the presigned URL, so a token minted for this API cannot be
	// replayed against another service.
	ServerIDHeader = "x-arena-server"

	// tokenExpirySeconds is the presigned URL lifetime (STS caps it at 15m).
	tokenExpirySeconds = "900"
)

// Token mints a bearer token for the caller's AWS credentials: a presigned
// GetCallerIdentity URL bound to serverID, base64-encoded. No secret is
// transmitted — the server learns only the caller's identity.
func Token(ctx context.Context, cfg aws.Config, serverID string) (string, error) {
	if serverID == "" {
		return "", fmt.Errorf("server id is required")
	}
	presign := sts.NewPresignClient(sts.NewFromConfig(cfg))
	out, err := presign.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(o *sts.PresignOptions) {
		o.ClientOptions = append(o.ClientOptions, func(so *sts.Options) {
			so.APIOptions = append(so.APIOptions,
				smithyhttp.SetHeaderValue(ServerIDHeader, serverID),
				smithyhttp.SetHeaderValue("X-Amz-Expires", tokenExpirySeconds),
			)
		})
	})
	if err != nil {
		return "", fmt.Errorf("presign GetCallerIdentity: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(out.URL)), nil
}

// NewClientInterceptor attaches a bearer token to outgoing RPCs. tokenFn is
// called per request; callers that want caching (matchmakers at allocation
// rates) wrap it with CachedToken.
func NewClientInterceptor(tokenFn func(ctx context.Context) (string, error)) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			token, err := tokenFn(ctx)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("mint auth token: %w", err))
			}
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}
}
