package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// AuthInfo describes the authenticated caller. It is attached to the request
// context by the auth middleware; handlers retrieve it with authOf.
type AuthInfo struct {
	OrgID        string
	CredentialID string
	Kind         store.CredentialKind
	EmployeeID   *string // set for PATs; nil for service accounts
	Role         store.Role
	Scopes       []string
}

type authInfoKey struct{}

// authOf returns the AuthInfo attached to the request, or nil when the
// request bypassed authentication (healthz, bootstrap).
func authOf(r *http.Request) *AuthInfo {
	ai, _ := r.Context().Value(authInfoKey{}).(*AuthInfo)
	return ai
}

// authOr401 returns the AuthInfo or writes 401 when the request bypassed
// authentication. Every route except GET /healthz and the bootstrap passes
// through the auth middleware, so this is only a safety net.
func authOr401(w http.ResponseWriter, r *http.Request) (*AuthInfo, bool) {
	if ai := authOf(r); ai != nil {
		return ai, true
	}
	writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
	return nil, false
}

// tokenPrefix marks tokens minted by this API so they are recognizable in
// logs and dashboards. The prefix is part of the hashed material.
const tokenPrefix = "hris_"

// newToken mints a 256-bit random bearer token. Only the SHA-256 hex digest
// is stored (via IssueCredential); the plaintext is shown to the caller once.
func newToken() (plaintext, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	plaintext = tokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return plaintext, hashToken(plaintext), nil
}

// hashToken digests a presented token the same way it was digested at
// issuance, so the store only ever sees hashes.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// --- roles and scopes ---

// roleRank orders roles for requireRole: owner > admin > manager > member.
var roleRank = map[store.Role]int{
	store.RoleOwner:   4,
	store.RoleAdmin:   3,
	store.RoleManager: 2,
	store.RoleMember:  1,
}

// scopesForRole maps a membership role to its scope set, per the component 1
// spec. Owners and admins hold every scope.
func scopesForRole(role store.Role) []string {
	switch role {
	case store.RoleOwner, store.RoleAdmin:
		return store.AllScopes()
	case store.RoleManager:
		return []string{
			store.ScopeEmployeesRead,
			store.ScopeTimeRead, store.ScopeTimeWrite,
			store.ScopeLeaveRead, store.ScopeLeaveWrite, store.ScopeLeaveApprove,
			store.ScopePayrollRead, store.ScopePayrollPreview,
			store.ScopeAuditRead,
		}
	case store.RoleMember:
		return []string{
			store.ScopeEmployeesRead,
			store.ScopeTimeRead, store.ScopeTimeWrite,
			store.ScopeLeaveRead, store.ScopeLeaveWrite,
			store.ScopePayrollRead, // own payslip only; the self-check in the handler enforces it
		}
	default:
		return nil
	}
}

// effectiveRole resolves the role used for requireRole checks. PATs use
// their membership role. Service accounts have no membership, so they are
// treated as the highest role whose scope set is fully covered by the
// credential's explicit scopes (e.g. a service account holding exactly the
// manager scope set is manager-equivalent; one holding a single exotic scope
// like payroll:submit has no role and can only pass scope gates).
func effectiveRole(ai *AuthInfo) store.Role {
	if ai.Kind == store.CredentialPAT {
		return ai.Role
	}
	have := make(map[string]bool, len(ai.Scopes))
	for _, sc := range ai.Scopes {
		have[sc] = true
	}
	covers := func(role store.Role) bool {
		for _, sc := range scopesForRole(role) {
			if !have[sc] {
				return false
			}
		}
		return true
	}
	for _, role := range []store.Role{
		store.RoleOwner, store.RoleAdmin,
		store.RoleManager, store.RoleMember,
	} {
		if covers(role) {
			return role
		}
	}
	return store.Role("")
}

// requireRole reports whether the caller meets the minimum role.
func requireRole(min store.Role, ai *AuthInfo) bool {
	return roleRank[effectiveRole(ai)] >= roleRank[min]
}

// requireScope reports whether the caller holds the scope.
func requireScope(scope string, ai *AuthInfo) bool {
	for _, s := range ai.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// --- bearer authentication ---

// withAuth resolves the bearer token into an AuthInfo and attaches it to the
// request context. GET /healthz and the organization bootstrap are public;
// everything else requires authentication.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" ||
			(r.Method == http.MethodPost && r.URL.Path == "/api/v1/organizations") ||
			// Component 6 public webhooks: authentication is the Slack
			// request signature / Teams shared secret, verified inside the
			// handlers — there is no forge-hris bearer token on these.
			(r.Method == http.MethodPost && (r.URL.Path == "/api/v1/integrations/slack/events" ||
				r.URL.Path == "/api/v1/integrations/teams/webhook")) ||
			// Xero OAuth callback: Xero redirects the operator's browser
			// here; the state token (validated inside the handler) binds
			// the callback to the org that started the flow.
			(r.Method == http.MethodGet && r.URL.Path == "/api/v1/integrations/xero/callback") ||
			// ESS portal: browser-facing employee self-service under /ess/
			// with its own magic-link + session-cookie auth, enforced
			// inside the web/ess handlers — there is no forge-hris bearer
			// token on these requests.
			strings.HasPrefix(r.URL.Path, "/ess/") || r.URL.Path == "/ess" {
			next.ServeHTTP(w, r)
			return
		}
		ai, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authInfoKey{}, ai)))
	})
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*AuthInfo, bool) {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || strings.TrimSpace(parts[1]) == "" {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
			"missing or malformed Authorization header; expected 'Bearer <token>'")
		return nil, false
	}
	cred, err := s.store.GetCredentialByTokenHash(r.Context(), hashToken(strings.TrimSpace(parts[1])))
	if err != nil {
		// The store rejects unknown, expired, and revoked tokens alike so
		// callers can't distinguish them.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
			"invalid, expired, or revoked credential")
		return nil, false
	}

	ai := &AuthInfo{
		OrgID:        cred.OrganizationID,
		CredentialID: cred.ID,
		Kind:         cred.Kind,
	}
	switch cred.Kind {
	case store.CredentialPAT:
		if cred.EmployeeID == nil || *cred.EmployeeID == "" {
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
				"personal access token is not bound to an employee")
			return nil, false
		}
		role, err := s.store.GetRole(r.Context(), cred.OrganizationID, *cred.EmployeeID)
		if err != nil {
			// No membership (e.g. employee deactivated): the token no
			// longer authorizes anything.
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
				"credential has no active role")
			return nil, false
		}
		// A PAT is only valid while its employee is joined. Deactivation
		// (or removal) must immediately stop the token from authenticating,
		// even though the membership row still exists.
		emp, err := s.store.GetEmployee(r.Context(), cred.OrganizationID, *cred.EmployeeID)
		if err != nil || emp.Status != store.InvitationJoined {
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
				"credential has no active role")
			return nil, false
		}
		ai.EmployeeID = cred.EmployeeID
		ai.Role = role
		ai.Scopes = scopesForRole(role)
	case store.CredentialServiceAccount:
		ai.Scopes = append([]string(nil), cred.Scopes...)
	default:
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
			"unknown credential kind")
		return nil, false
	}

	// Best-effort last-used touch; never fail the request over it.
	_ = s.store.TouchCredential(r.Context(), cred.OrganizationID, cred.ID, time.Now().UTC())

	return ai, true
}

// --- audit ---

// auditData is what a mutating handler returns so withAudit can record it.
type auditData struct {
	ResourceID string
	Before     any
	After      any
}

// withAudit wraps a mutating handler: the inner handler performs the
// mutation and reports what changed; on success an audit event is appended
// with the actor derived from the credential (pat -> human,
// service_account -> agent) and the Idempotency-Key when present.
func (s *Server) withAudit(action, resourceType string,
	h func(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ai, ok := authOr401(w, r)
		if !ok {
			return
		}
		ad, ok := h(w, r, ai)
		if !ok {
			return
		}
		s.appendAudit(r.Context(), r, ai, action, resourceType, ad.ResourceID, ad.Before, ad.After)
	}
}

// actorTypeFor maps a credential kind to the audit actor type.
func actorTypeFor(kind store.CredentialKind) store.ActorType {
	if kind == store.CredentialServiceAccount {
		return store.ActorAgent
	}
	return store.ActorHuman
}

// appendAudit records one audit event for an authenticated mutation.
// Failures are logged, never returned: the mutation already succeeded.
func (s *Server) appendAudit(ctx context.Context, r *http.Request, ai *AuthInfo,
	action, resourceType, resourceID string, before, after any,
) {
	s.recordAudit(ctx, store.AuditEvent{
		OrganizationID: ai.OrgID,
		ActorType:      actorTypeFor(ai.Kind),
		ActorID:        ai.CredentialID,
		Action:         action,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		Before:         mustMarshalJSON(before),
		After:          mustMarshalJSON(after),
		IdempotencyKey: idempotencyKeyOf(r),
	})
}

// appendAuditSystem records an audit event for the unauthenticated bootstrap,
// where the actor is the system itself.
func (s *Server) appendAuditSystem(ctx context.Context, orgID, action, resourceType, resourceID string, before, after any) {
	s.recordAudit(ctx, store.AuditEvent{
		OrganizationID: orgID,
		ActorType:      store.ActorSystem,
		ActorID:        "bootstrap",
		Action:         action,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		Before:         mustMarshalJSON(before),
		After:          mustMarshalJSON(after),
	})
}

func (s *Server) recordAudit(ctx context.Context, ev store.AuditEvent) {
	// The store assigns ID/PrevHash/Hash/CreatedAt and maintains the
	// hash chain; a late error here must not fail the mutation.
	if _, err := s.store.AppendAuditEvent(ctx, ev); err != nil {
		s.logger.Error("audit append failed",
			"action", ev.Action,
			"resource_type", ev.ResourceType,
			"resource_id", ev.ResourceID,
			"error", err,
		)
	}
}

func idempotencyKeyOf(r *http.Request) *string {
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		return &key
	}
	return nil
}

func mustMarshalJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		// Store types marshal cleanly; this is unreachable in practice.
		return json.RawMessage(`{"marshal_error":true}`)
	}
	return raw
}
