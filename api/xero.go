// Package api — Xero integration (component 6, Worker A).
//
// OAuth connect/callback, connection status, account/contact mappings,
// disconnect, and per-employee bill posting from payroll runs. The heavy
// lifting lives in integrations/xero; these handlers only do auth, the
// org guard, and audit wiring. The auto-post hook fired on payroll
// approval lives here too (autoPostXeroBill, called from submitPayrollRun).
package api

import (
	"context"
	"net/http"

	"github.com/azmi2409/forge-hris/integrations/xero"
	"github.com/azmi2409/forge-hris/store"
)

// xeroRoutes registers the component 6 Xero routes. Called from routes().
func (s *Server) xeroRoutes() {
	m := s.mux
	base := "/api/v1/organizations/{orgID}/integrations/xero"

	m.HandleFunc("POST "+base+"/connect",
		s.withAudit("xero.connected", "xero_connection", s.handleXeroConnect))
	// The OAuth callback is unauthenticated by design (Xero redirects the
	// operator's browser here); withAuth exempts the path and the handler
	// validates the state token.
	m.HandleFunc("GET /api/v1/integrations/xero/callback", s.handleXeroCallback)
	m.HandleFunc("GET "+base+"/status", s.handleXeroStatus)
	m.HandleFunc("POST "+base+"/mappings",
		s.withAudit("xero.mappings_updated", "xero_connection", s.handleXeroMappings))
	m.HandleFunc("DELETE "+base,
		s.withAudit("xero.disconnected", "xero_connection", s.handleXeroDisconnect))
	// Idempotency-Key is honored by the existing withIdempotency
	// middleware, which covers every POST.
	m.HandleFunc("POST /api/v1/organizations/{orgID}/payroll-runs/{id}/xero-bill",
		s.withAudit("xero.bill_posted", "xero_bill_posting", s.handleXeroPostBill))
}

// xeroOf resolves the integration handler and the narrow xero store facet.
// A nil handler means the integration is not configured: 501, not 404.
func (s *Server) xeroOf(w http.ResponseWriter, r *http.Request) (*xero.Handler, xero.Store, bool) {
	if s.xero == nil {
		writeError(w, r, http.StatusNotImplemented, "XERO_NOT_CONFIGURED",
			"xero integration is not configured on this server")
		return nil, nil, false
	}
	st, ok := s.store.(xero.Store)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "xero store not available")
		return nil, nil, false
	}
	return s.xero, st, true
}

func (s *Server) handleXeroConnect(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	h, _, ok := s.xeroOf(w, r)
	if !ok {
		return zero, false
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to connect Xero") {
		return zero, false
	}
	h.HandleConnect(w, r, orgID, true)
	return auditData{ResourceID: orgID}, true
}

func (s *Server) handleXeroCallback(w http.ResponseWriter, r *http.Request) {
	h, _, ok := s.xeroOf(w, r)
	if !ok {
		return
	}
	h.HandleCallback(w, r)
}

func (s *Server) handleXeroStatus(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	h, _, ok := s.xeroOf(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	h.HandleStatus(w, r, orgID)
}

func (s *Server) handleXeroMappings(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	h, _, ok := s.xeroOf(w, r)
	if !ok {
		return zero, false
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to configure Xero mappings") {
		return zero, false
	}
	h.HandleMappings(w, r, orgID, true)
	return auditData{ResourceID: orgID}, true
}

func (s *Server) handleXeroDisconnect(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	h, _, ok := s.xeroOf(w, r)
	if !ok {
		return zero, false
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to disconnect Xero") {
		return zero, false
	}
	h.HandleDisconnect(w, r, orgID, true)
	return auditData{ResourceID: orgID}, true
}

type xeroBillRequestBody struct {
	DryRun bool `json:"dry_run"`
}

func (s *Server) handleXeroPostBill(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	h, _, ok := s.xeroOf(w, r)
	if !ok {
		return zero, false
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to post Xero bills") {
		return zero, false
	}
	var body xeroBillRequestBody
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &body) {
			return zero, false
		}
	}
	runID := r.PathValue("id")
	h.HandlePostBill(w, r, orgID, runID, true, body.DryRun)
	return auditData{ResourceID: runID}, true
}

// autoPostXeroBill fires the Xero auto-post hook after a payroll approval.
// It never fails the request: hook errors are logged.
func (s *Server) autoPostXeroBill(ctx context.Context, orgID, runID string) {
	h := s.xero
	if h == nil {
		return
	}
	st, ok := s.store.(xero.Store)
	if !ok {
		s.logger.Error("xero auto-post skipped: store does not implement xero.Store",
			"organization_id", orgID, "run_id", runID)
		return
	}
	if err := xero.MaybeAutoPostBill(ctx, st, h.Client(), orgID, runID, s.logger); err != nil {
		s.logger.Error("xero auto-post failed",
			"organization_id", orgID, "run_id", runID, "error", err)
	}
}
