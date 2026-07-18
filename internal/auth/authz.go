package auth

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
)

// Role is an arena-internal role (RBAC-lite).
type Role string

// The four built-in roles.
const (
	RoleAdmin       Role = "admin"
	RoleFleetEditor Role = "fleet-editor"
	RoleAllocator   Role = "allocator"
	RoleViewer      Role = "viewer"
)

// viewerProcedures are the read-only RPCs every role may call.
var viewerProcedures = []string{
	arenav1connect.FleetServiceGetFleetProcedure,
	arenav1connect.FleetServiceListFleetsProcedure,
	arenav1connect.GameServerServiceGetGameServerProcedure,
	arenav1connect.GameServerServiceListGameServersProcedure,
	arenav1connect.AllocationServiceGetAllocationProcedure,
}

// rolePermissions maps each role to its allowed procedures (admin is a
// wildcard handled in roleAllows).
var rolePermissions = map[Role]map[string]bool{
	RoleViewer: procSet(viewerProcedures),
	RoleAllocator: procSet(append([]string{
		arenav1connect.AllocationServiceAllocateProcedure,
		arenav1connect.AllocationServiceReleaseProcedure,
	}, viewerProcedures...)),
	RoleFleetEditor: procSet(append([]string{
		arenav1connect.FleetServiceCreateFleetProcedure,
		arenav1connect.FleetServiceUpdateFleetProcedure,
		arenav1connect.FleetServiceDeleteFleetProcedure,
		arenav1connect.FleetServiceScaleFleetProcedure,
		arenav1connect.FleetServiceApplyFleetProcedure,
	}, viewerProcedures...)),
}

func procSet(procs []string) map[string]bool {
	m := make(map[string]bool, len(procs))
	for _, p := range procs {
		m[p] = true
	}
	return m
}

func roleAllows(r Role, procedure string) bool {
	return r == RoleAdmin || rolePermissions[r][procedure]
}

// ReadOnlyProcedure reports whether a procedure never mutates state (used
// to scope audit logging to mutating RPCs).
func ReadOnlyProcedure(procedure string) bool {
	return rolePermissions[RoleViewer][procedure]
}

// Binding grants one IAM principal a role, optionally scoped to namespaces.
type Binding struct {
	// Principal is the normalized IAM ARN (role or user). Assumed-role
	// session ARNs are normalized before matching.
	Principal string `yaml:"principal"`
	Role      Role   `yaml:"role"`
	// Namespaces limits the grant; empty means all. Entries may end in "*"
	// for prefix matching ("shooter-*").
	Namespaces []string `yaml:"namespaces"`
}

// Config is the authorization document (SSM /arena/{env}/authz or a file).
type Config struct {
	Bindings []Binding `yaml:"bindings"`
}

// ParseConfig decodes and validates the YAML authorization document.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse authz config: %w", err)
	}
	for i, b := range cfg.Bindings {
		if b.Principal == "" {
			return nil, fmt.Errorf("authz binding %d: principal is required", i)
		}
		switch b.Role {
		case RoleAdmin, RoleFleetEditor, RoleAllocator, RoleViewer:
		default:
			return nil, fmt.Errorf("authz binding %d: unknown role %q", i, b.Role)
		}
	}
	return &cfg, nil
}

// assumedRolePattern matches STS assumed-role session ARNs.
var assumedRolePattern = regexp.MustCompile(`^arn:aws:sts::(\d+):assumed-role/([^/]+)/.+$`)

// NormalizeARN folds assumed-role session ARNs onto their IAM role ARN so
// bindings are written once per role, not per session.
func NormalizeARN(arn string) string {
	if m := assumedRolePattern.FindStringSubmatch(arn); m != nil {
		return fmt.Sprintf("arn:aws:iam::%s:role/%s", m[1], m[2])
	}
	return arn
}

// Authorizer answers "may this principal call this procedure in this
// namespace". The config is swapped atomically on reload.
type Authorizer struct {
	cfg atomic.Pointer[Config]
}

// NewAuthorizer returns an Authorizer over an initial config.
func NewAuthorizer(cfg *Config) *Authorizer {
	a := &Authorizer{}
	a.cfg.Store(cfg)
	return a
}

// Reload swaps in a new config.
func (a *Authorizer) Reload(cfg *Config) { a.cfg.Store(cfg) }

// Authorize returns nil when some binding grants the (already normalized)
// principal the procedure in the namespace. Requests without a namespace
// concept (e.g. Release by allocation id) pass on the role check alone.
func (a *Authorizer) Authorize(principalARN, procedure, namespace string) error {
	principal := NormalizeARN(principalARN)
	for _, b := range a.cfg.Load().Bindings {
		if b.Principal != principal || !roleAllows(b.Role, procedure) {
			continue
		}
		if namespace == "" || namespaceAllowed(b.Namespaces, namespace) {
			return nil
		}
	}
	return fmt.Errorf("principal %s is not allowed to call %s in namespace %q", principal, procedure, namespace)
}

func namespaceAllowed(patterns []string, ns string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == "*" || p == ns {
			return true
		}
		if prefix, ok := strings.CutSuffix(p, "*"); ok && strings.HasPrefix(ns, prefix) {
			return true
		}
	}
	return false
}

// Source supplies the authz document (file now; SSM in deployment).
type Source interface {
	Fetch(ctx context.Context) ([]byte, error)
}

// FileSource reads the document from a local file.
type FileSource string

// Fetch implements Source.
func (f FileSource) Fetch(context.Context) ([]byte, error) { return os.ReadFile(string(f)) }

// Watch reloads the authorizer from src every interval until ctx is done.
// A failed fetch or parse keeps the previous config (fail-static).
func Watch(ctx context.Context, src Source, interval time.Duration, a *Authorizer, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := src.Fetch(ctx)
			if err != nil {
				log.Warn("authz config fetch failed; keeping previous", "error", err)
				continue
			}
			cfg, err := ParseConfig(data)
			if err != nil {
				log.Warn("authz config invalid; keeping previous", "error", err)
				continue
			}
			a.Reload(cfg)
		}
	}
}
