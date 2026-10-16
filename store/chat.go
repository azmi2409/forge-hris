// Package store — component 6 (chat integrations + ESS): storage domain.
//
// This file holds the chat-integration domain types (Validate pattern
// copied from component 1) and the ChatStore contract. It is separate from
// store.go on purpose: store.go is frozen and new component contracts live
// in their own files.
//
// Secrets are NEVER stored in plaintext: Slack signing secrets, service
// tokens, and Teams shared secrets are AES-GCM envelopes produced by the
// integrations/secrets package (key from FORGE_HRIS_SECRET_KEY). ESS magic
// links are stored as SHA-256 hex digests — the plaintext token is shown
// once and never persisted.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// SlackWorkspace is one connected Slack workspace. The signing secret
// (X-Slack-Signature verification) and the service token (a forge-hris
// credential with time:write scope, authorizing the integration's clock
// calls) are stored as AES-GCM envelopes — plaintext never touches the DB.
type SlackWorkspace struct {
	TeamID           string    `json:"team_id"`
	OrganizationID   string    `json:"organization_id"`
	TeamName         string    `json:"team_name,omitempty"`
	EncSigningSecret string    `json:"-"`       // AES-GCM envelope; never serialized
	EncServiceToken  string    `json:"-"`       // AES-GCM envelope; never serialized
	Version          int64     `json:"version"` // optimistic concurrency token
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Validate checks the workspace's shape. Secrets must already be encrypted
// by the caller (the API layer); the store only checks presence.
func (w SlackWorkspace) Validate() error {
	if strings.TrimSpace(w.TeamID) == "" {
		return &ValidationError{Field: "team_id", Message: "required"}
	}
	if strings.TrimSpace(w.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(w.EncSigningSecret) == "" {
		return &ValidationError{Field: "signing_secret", Message: "required (encrypted)"}
	}
	if strings.TrimSpace(w.EncServiceToken) == "" {
		return &ValidationError{Field: "service_token", Message: "required (encrypted)"}
	}
	return nil
}

// TeamsConfig is one connected Microsoft Teams tenant. The shared secret
// (the ?key= query parameter on the inbound webhook) is an AES-GCM
// envelope — plaintext never touches the DB.
type TeamsConfig struct {
	OrganizationID  string    `json:"organization_id"`
	TenantRef       string    `json:"tenant_ref,omitempty"` // the customer's Teams tenant id, for ops reference
	EncSharedSecret string    `json:"-"`                    // AES-GCM envelope; never serialized
	Version         int64     `json:"version"`              // optimistic concurrency token
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Validate checks the config's shape.
func (c TeamsConfig) Validate() error {
	if strings.TrimSpace(c.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(c.EncSharedSecret) == "" {
		return &ValidationError{Field: "shared_secret", Message: "required (encrypted)"}
	}
	return nil
}

// ESSToken is one issued ESS magic link. Only the SHA-256 hex digest of the
// token is stored; the plaintext is shown once at issuance and never
// persisted. Tokens are single-use (UsedAt) and short-lived (ExpiresAt).
type ESSToken struct {
	TokenHash      string     `json:"-"` // hex(sha256(token)); never serialized
	OrganizationID string     `json:"organization_id"`
	EmployeeID     string     `json:"employee_id"`
	ExpiresAt      time.Time  `json:"expires_at"`
	UsedAt         *time.Time `json:"used_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Validate checks the token record's shape.
func (t ESSToken) Validate() error {
	if strings.TrimSpace(t.TokenHash) == "" {
		return &ValidationError{Field: "token_hash", Message: "required"}
	}
	if strings.TrimSpace(t.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(t.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	if t.ExpiresAt.IsZero() {
		return &ValidationError{Field: "expires_at", Message: "required"}
	}
	return nil
}

// Expired reports whether the token is past its expiry at now.
func (t ESSToken) Expired(now time.Time) bool {
	return !now.Before(t.ExpiresAt)
}

// Used reports whether the token has already been redeemed.
func (t ESSToken) Used() bool {
	return t.UsedAt != nil
}

// essTokenHash digests a magic-link token the same way IssueCredential
// digests bearer tokens: hex(sha256(plaintext)).
func essTokenHash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ChatStore is the component 6 storage contract: Slack workspace configs,
// Slack/Teams user identity maps, Teams tenant configs, and ESS magic-link
// tokens.
//
// All methods are safe for concurrent use and deep-copy values across the
// boundary, matching the component 1 contract.
type ChatStore interface {
	// Slack workspaces.
	// SaveSlackWorkspace upserts the workspace by team_id. When a row
	// already exists the caller's Version must match (ErrVersionConflict on
	// mismatch); a new row starts at version 1. ErrNotFound for an unknown
	// organization.
	SaveSlackWorkspace(ctx context.Context, w SlackWorkspace) (SlackWorkspace, error)
	// GetSlackWorkspace returns the workspace for team_id or ErrNotFound.
	// Cross-tenant by nature (team_id arrives from Slack before the org is
	// known); the signing secret is the authorization.
	GetSlackWorkspace(ctx context.Context, teamID string) (SlackWorkspace, error)
	// DeleteSlackWorkspace removes the workspace and its user mappings.
	// Idempotent; ErrNotFound when unknown.
	DeleteSlackWorkspace(ctx context.Context, teamID string) error

	// Slack user identity map (Slack user_id -> employee).
	// MapSlackUser links a Slack user to an employee in the workspace's
	// org. ErrNotFound when the workspace or employee is unknown (or the
	// employee belongs to a different organization).
	MapSlackUser(ctx context.Context, teamID, slackUserID, employeeID string) error
	// GetSlackUserEmployee returns the mapped employee ID, or ErrNotFound
	// when the Slack user is not linked.
	GetSlackUserEmployee(ctx context.Context, teamID, slackUserID string) (string, error)
	// UnmapSlackUser removes the link. Idempotent.
	UnmapSlackUser(ctx context.Context, teamID, slackUserID string) error

	// Teams configs.
	// SaveTeamsConfig upserts by organization_id with the same
	// version-checked semantics as SaveSlackWorkspace.
	SaveTeamsConfig(ctx context.Context, c TeamsConfig) (TeamsConfig, error)
	// GetTeamsConfig returns the org's config or ErrNotFound.
	GetTeamsConfig(ctx context.Context, orgID string) (TeamsConfig, error)
	// ListTeamsConfigs returns every org's config (the inbound webhook
	// compares the presented ?key= against each, constant-time). Ordered
	// by (created_at, organization_id), paged.
	ListTeamsConfigs(ctx context.Context, p Page) (configs []TeamsConfig, next string, err error)
	// DeleteTeamsConfig removes the config and its user mappings.
	// Idempotent; ErrNotFound when unknown.
	DeleteTeamsConfig(ctx context.Context, orgID string) error

	// Teams user identity map (Teams user_id -> employee).
	MapTeamsUser(ctx context.Context, orgID, teamsUserID, employeeID string) error
	// GetTeamsUserEmployee returns the mapped employee ID, or ErrNotFound
	// when the Teams user is not linked.
	GetTeamsUserEmployee(ctx context.Context, orgID, teamsUserID string) (string, error)
	// UnmapTeamsUser removes the link. Idempotent.
	UnmapTeamsUser(ctx context.Context, orgID, teamsUserID string) error

	// ESS magic-link tokens.
	// IssueESSToken stores a token record keyed by its digest.
	// ErrAlreadyExists on digest collision (practically impossible;
	// callers regenerate), ErrNotFound for an unknown organization or
	// employee.
	IssueESSToken(ctx context.Context, t ESSToken) (ESSToken, error)
	// GetESSToken returns the token record for a digest, or ErrNotFound.
	// It does NOT reject used or expired tokens — the caller decides.
	GetESSToken(ctx context.Context, tokenHash string) (ESSToken, error)
	// MarkESSTokenUsed records redemption. Idempotent (first write wins);
	// ErrNotFound when unknown.
	MarkESSTokenUsed(ctx context.Context, tokenHash string, at time.Time) error
	// DeleteExpiredESSTokens removes tokens expired before `before`
	// (housekeeping); returns the number removed.
	DeleteExpiredESSTokens(ctx context.Context, before time.Time) (int64, error)

	// FindEmployeeByEmail locates a joined employee by email across
	// organizations (ESS login only has the email). Returns the org ID and
	// the employee. ErrNotFound when no joined employee has the email;
	// an error when the email is ambiguous across orgs.
	FindEmployeeByEmail(ctx context.Context, email string) (orgID string, emp Employee, err error)
}
