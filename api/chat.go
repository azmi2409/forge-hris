// Package api — component 6: chat-integration admin routes + public
// Slack/Teams webhook entry points.
//
// Route contract (fixed by the coordinator; see docs/integrations.md):
//   - POST /api/v1/integrations/slack/events            (public: Slack signature is the auth)
//   - POST /api/v1/organizations/{orgID}/integrations/slack        (admin)
//   - POST /api/v1/organizations/{orgID}/integrations/slack/users  (admin)
//   - POST /api/v1/integrations/teams/webhook            (public: shared secret is the auth)
//   - POST /api/v1/organizations/{orgID}/integrations/teams        (admin)
//   - POST /api/v1/organizations/{orgID}/integrations/teams/users  (admin)
//
// The two webhook routes bypass forge-hris bearer auth (exempted in
// withAuth); their handlers verify the Slack signature / Teams shared
// secret instead. Secrets are encrypted with integrations/secrets before
// they reach the store — plaintext never touches the DB. Errors use the
// flat UPPER_SNAKE envelope via writeError.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/azmi2409/forge-hris/integrations/secrets"
	integrationsslack "github.com/azmi2409/forge-hris/integrations/slack"
	integrationsteams "github.com/azmi2409/forge-hris/integrations/teams"
	"github.com/azmi2409/forge-hris/store"
)

// chatAdapter satisfies the integrations packages' narrow Store interfaces
// by combining the full store.Store with the component 6 ChatStore. The
// type assertion fails only if a Store implementation predates component 6.
type chatAdapter struct {
	store.Store
	store.ChatStore
}

func (s *Server) chatStoreOf(w http.ResponseWriter, r *http.Request) (chatAdapter, bool) {
	cs, ok := s.store.(store.ChatStore)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL",
			"store does not implement the chat integration contract")
		return chatAdapter{}, false
	}
	return chatAdapter{Store: s.store, ChatStore: cs}, true
}

func (s *Server) chatRoutes() {
	m := s.mux
	// Public webhooks (auth = Slack signature / Teams shared secret).
	m.HandleFunc("POST /api/v1/integrations/slack/events", s.handleSlackEvents)
	m.HandleFunc("POST /api/v1/integrations/teams/webhook", s.handleTeamsWebhook)

	// Admin: connect a workspace / tenant.
	m.HandleFunc("POST /api/v1/organizations/{orgID}/integrations/slack",
		s.withAudit("slack.connected", "integration", s.connectSlack))
	m.HandleFunc("POST /api/v1/organizations/{orgID}/integrations/teams",
		s.withAudit("teams.connected", "integration", s.connectTeams))

	// Admin: link chat identities to employees.
	m.HandleFunc("POST /api/v1/organizations/{orgID}/integrations/slack/users",
		s.withAudit("slack.user_linked", "integration", s.linkSlackUser))
	m.HandleFunc("POST /api/v1/organizations/{orgID}/integrations/teams/users",
		s.withAudit("teams.user_linked", "integration", s.linkTeamsUser))
}

// --- public webhooks ---

func (s *Server) handleSlackEvents(w http.ResponseWriter, r *http.Request) {
	ad, ok := s.chatStoreOf(w, r)
	if !ok {
		return
	}
	integrationsslack.NewHandler(ad, integrationsslack.Config{}).HandleEvents(w, r)
}

func (s *Server) handleTeamsWebhook(w http.ResponseWriter, r *http.Request) {
	ad, ok := s.chatStoreOf(w, r)
	if !ok {
		return
	}
	integrationsteams.NewHandler(ad, integrationsteams.Config{}).HandleWebhook(w, r)
}

// --- admin: connect ---

type connectSlackRequest struct {
	TeamID        string `json:"team_id"`
	TeamName      string `json:"team_name"`
	SigningSecret string `json:"signing_secret"`
	ServiceToken  string `json:"service_token"`
}

// connectSlack connects a Slack workspace. The service token must be a
// live forge-hris credential in this org holding the time:write scope —
// its validity is checked now, and it is re-validated on every inbound
// event, so revoking the token disables the bot.
func (s *Server) connectSlack(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return auditData{}, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to connect Slack") {
		return auditData{}, false
	}
	var req connectSlackRequest
	if !decodeJSON(w, r, &req) {
		return auditData{}, false
	}
	if strings.TrimSpace(req.TeamID) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "team_id: required")
		return auditData{}, false
	}
	if strings.TrimSpace(req.SigningSecret) == "" || strings.TrimSpace(req.ServiceToken) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"signing_secret and service_token are required")
		return auditData{}, false
	}
	cred, err := s.store.GetCredentialByTokenHash(r.Context(), hashServiceToken(req.ServiceToken))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"service_token: not a valid forge-hris credential")
		return auditData{}, false
	}
	if cred.OrganizationID != orgID {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"service_token: belongs to a different organization")
		return auditData{}, false
	}
	if !hasScopeString(cred.Scopes, store.ScopeTimeWrite) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"service_token: must hold the time:write scope")
		return auditData{}, false
	}
	encSecret, err := secrets.Encrypt([]byte(req.SigningSecret))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "could not encrypt signing secret")
		return auditData{}, false
	}
	encToken, err := secrets.Encrypt([]byte(req.ServiceToken))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "could not encrypt service token")
		return auditData{}, false
	}
	ad, ok := s.chatStoreOf(w, r)
	if !ok {
		return auditData{}, false
	}
	ws, err := ad.SaveSlackWorkspace(r.Context(), store.SlackWorkspace{
		TeamID:           strings.TrimSpace(req.TeamID),
		OrganizationID:   orgID,
		TeamName:         strings.TrimSpace(req.TeamName),
		EncSigningSecret: encSecret,
		EncServiceToken:  encToken,
	})
	if !mapError(w, r, err) {
		return auditData{}, false
	}
	writeJSON(w, http.StatusCreated, ws)
	return auditData{ResourceID: ws.TeamID, After: map[string]any{"team_id": ws.TeamID}}, true
}

type connectTeamsRequest struct {
	TenantRef    string `json:"tenant_ref"`
	SharedSecret string `json:"shared_secret"`
}

// connectTeams connects a Teams tenant: the shared secret becomes the
// ?key= value the Teams side must present on the inbound webhook.
func (s *Server) connectTeams(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return auditData{}, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to connect Teams") {
		return auditData{}, false
	}
	var req connectTeamsRequest
	if !decodeJSON(w, r, &req) {
		return auditData{}, false
	}
	if strings.TrimSpace(req.SharedSecret) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "shared_secret: required")
		return auditData{}, false
	}
	enc, err := secrets.Encrypt([]byte(req.SharedSecret))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "could not encrypt shared secret")
		return auditData{}, false
	}
	ad, ok := s.chatStoreOf(w, r)
	if !ok {
		return auditData{}, false
	}
	cfg, err := ad.SaveTeamsConfig(r.Context(), store.TeamsConfig{
		OrganizationID:  orgID,
		TenantRef:       strings.TrimSpace(req.TenantRef),
		EncSharedSecret: enc,
	})
	if !mapError(w, r, err) {
		return auditData{}, false
	}
	writeJSON(w, http.StatusCreated, cfg)
	return auditData{ResourceID: orgID, After: map[string]any{"organization_id": orgID}}, true
}

// --- admin: link users ---

type linkSlackUserRequest struct {
	TeamID      string `json:"team_id"`
	SlackUserID string `json:"slack_user_id"`
	EmployeeID  string `json:"employee_id"`
}

// linkSlackUser links a Slack user to an employee. team_id is required:
// an org may connect several workspaces. (Deviation from the first draft
// of the contract, which omitted it — multi-workspace orgs need it.)
func (s *Server) linkSlackUser(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return auditData{}, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to link Slack users") {
		return auditData{}, false
	}
	var req linkSlackUserRequest
	if !decodeJSON(w, r, &req) {
		return auditData{}, false
	}
	if strings.TrimSpace(req.TeamID) == "" || strings.TrimSpace(req.SlackUserID) == "" ||
		strings.TrimSpace(req.EmployeeID) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"team_id, slack_user_id and employee_id are required")
		return auditData{}, false
	}
	ad, ok := s.chatStoreOf(w, r)
	if !ok {
		return auditData{}, false
	}
	// The workspace must belong to the caller's org — cross-tenant guard.
	ws, err := ad.GetSlackWorkspace(r.Context(), strings.TrimSpace(req.TeamID))
	if !mapError(w, r, err) {
		return auditData{}, false
	}
	if ws.OrganizationID != orgID {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "organization mismatch")
		return auditData{}, false
	}
	if err := ad.MapSlackUser(r.Context(), ws.TeamID, strings.TrimSpace(req.SlackUserID), strings.TrimSpace(req.EmployeeID)); !mapError(w, r, err) {
		return auditData{}, false
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"team_id": req.TeamID, "slack_user_id": req.SlackUserID, "employee_id": req.EmployeeID,
	})
	return auditData{
		ResourceID: ws.TeamID + "/" + req.SlackUserID,
		After:      map[string]any{"employee_id": req.EmployeeID},
	}, true
}

type linkTeamsUserRequest struct {
	TeamsUserID string `json:"teams_user_id"`
	EmployeeID  string `json:"employee_id"`
}

// linkTeamsUser links a Teams user to an employee in the caller's org.
func (s *Server) linkTeamsUser(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return auditData{}, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to link Teams users") {
		return auditData{}, false
	}
	var req linkTeamsUserRequest
	if !decodeJSON(w, r, &req) {
		return auditData{}, false
	}
	if strings.TrimSpace(req.TeamsUserID) == "" || strings.TrimSpace(req.EmployeeID) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"teams_user_id and employee_id are required")
		return auditData{}, false
	}
	ad, ok := s.chatStoreOf(w, r)
	if !ok {
		return auditData{}, false
	}
	if err := ad.MapTeamsUser(r.Context(), orgID, strings.TrimSpace(req.TeamsUserID), strings.TrimSpace(req.EmployeeID)); !mapError(w, r, err) {
		return auditData{}, false
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"teams_user_id": req.TeamsUserID, "employee_id": req.EmployeeID,
	})
	return auditData{
		ResourceID: orgID + "/" + req.TeamsUserID,
		After:      map[string]any{"employee_id": req.EmployeeID},
	}, true
}

// --- helpers ---

func hashServiceToken(plaintext string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plaintext)))
	return hex.EncodeToString(sum[:])
}

func hasScopeString(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}
