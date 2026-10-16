// Package mcp implements the forge-hris component 4 MCP server: a
// JSON-RPC 2.0 agent surface over the same store calls the REST handlers
// use, so REST and MCP never drift.
//
// Transports:
//   - stdio: newline-delimited JSON-RPC on stdin/stdout (default; for
//     Claude Desktop / Claude Code).
//   - HTTP: POST /mcp (JSON-RPC request -> JSON-RPC response) and
//     GET /mcp (Server-Sent Events stream).
//
// Every tool call is authenticated with the same bearer PAT / service
// account model as the REST API: the presented token is digested with
// hex(sha256(token)) and looked up via store.GetCredentialByTokenHash.
// The credential's organization is the tenant; there is no org_id argument
// on any tool. Approval gates (tiers auto/confirm/never) are enforced
// server-side in approvals.go; the annotations in tools/list are hints
// only.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// AuthInfo describes the authenticated MCP caller. It mirrors api.AuthInfo
// (reimplemented here; the mcp package must not import the api package).
type AuthInfo struct {
	OrgID        string
	CredentialID string
	Kind         store.CredentialKind
	EmployeeID   *string // set for PATs; nil for service accounts
	Role         store.Role
	Scopes       []string
}

// hashToken digests a presented token the same way it was digested at
// issuance, so the store only ever sees hashes.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// roleRank orders roles: owner > admin > manager > member. It mirrors
// api/auth.go so role gates behave identically on both surfaces.
var roleRank = map[store.Role]int{
	store.RoleOwner:   4,
	store.RoleAdmin:   3,
	store.RoleManager: 2,
	store.RoleMember:  1,
}

// scopesForRole maps a membership role to its scope set, per the component 1
// spec. Owners and admins hold every scope. Mirrors api/auth.go.
func scopesForRole(role store.Role) []string {
	switch role {
	case store.RoleOwner, store.RoleAdmin:
		return store.AllScopes()
	case store.RoleManager:
		return []string{
			store.ScopeEmployeesRead,
			store.ScopeTimeRead, store.ScopeTimeWrite,
			store.ScopeLeaveRead, store.ScopeLeaveWrite, store.ScopeLeaveApprove,
			store.ScopeAuditRead,
		}
	case store.RoleMember:
		return []string{
			store.ScopeEmployeesRead,
			store.ScopeTimeRead, store.ScopeTimeWrite,
			store.ScopeLeaveRead, store.ScopeLeaveWrite,
		}
	default:
		return nil
	}
}

// effectiveRole resolves the role used for role gates. PATs use their
// membership role. Service accounts have no membership, so they are treated
// as the highest role whose scope set is fully covered by the credential's
// explicit scopes. Mirrors api/auth.go.
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

// roleAtLeast reports whether the caller meets the minimum role.
func roleAtLeast(min store.Role, ai *AuthInfo) bool {
	return roleRank[effectiveRole(ai)] >= roleRank[min]
}

// hasScope reports whether the caller holds the scope.
func hasScope(scope string, ai *AuthInfo) bool {
	for _, s := range ai.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Authenticate resolves a bearer token into an AuthInfo, mirroring the
// REST authenticate path: unknown, expired, and revoked tokens are all
// rejected identically so callers can't distinguish them. PATs additionally
// require an active (joined) employee and membership; service accounts
// carry their explicit scopes. The last-used touch is best-effort.
func (s *Server) Authenticate(ctx context.Context, token string) (*AuthInfo, *rpcError) {
	cred, err := s.store.GetCredentialByTokenHash(ctx, hashToken(token))
	if err != nil {
		return nil, &rpcError{
			Code:    rpcUnauthenticated,
			Message: "unauthenticated: invalid, expired, or revoked credential",
			Data:    map[string]any{"code": "UNAUTHENTICATED"},
		}
	}
	ai := &AuthInfo{
		OrgID:        cred.OrganizationID,
		CredentialID: cred.ID,
		Kind:         cred.Kind,
	}
	switch cred.Kind {
	case store.CredentialPAT:
		if cred.EmployeeID == nil || *cred.EmployeeID == "" {
			return nil, unauthenticated("personal access token is not bound to an employee")
		}
		role, err := s.store.GetRole(ctx, cred.OrganizationID, *cred.EmployeeID)
		if err != nil {
			return nil, unauthenticated("credential has no active role")
		}
		emp, err := s.store.GetEmployee(ctx, cred.OrganizationID, *cred.EmployeeID)
		if err != nil || emp.Status != store.InvitationJoined {
			return nil, unauthenticated("credential has no active role")
		}
		ai.EmployeeID = cred.EmployeeID
		ai.Role = role
		ai.Scopes = scopesForRole(role)
	case store.CredentialServiceAccount:
		ai.Scopes = append([]string(nil), cred.Scopes...)
	default:
		return nil, unauthenticated("unknown credential kind")
	}
	// Best-effort last-used touch; never fail the request over it.
	_ = s.store.TouchCredential(ctx, cred.OrganizationID, cred.ID, time.Now().UTC())
	return ai, nil
}

func unauthenticated(msg string) *rpcError {
	return &rpcError{
		Code:    rpcUnauthenticated,
		Message: "unauthenticated: " + msg,
		Data:    map[string]any{"code": "UNAUTHENTICATED"},
	}
}
