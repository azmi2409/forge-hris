// Package api implements the forge-hris component 1 REST/JSON API:
// identity, tenancy, and audit.
//
// The API is a thin translation layer over store.Store: it parses HTTP into
// store calls and maps store errors to HTTP status codes. It is stateless —
// every handler is safe for concurrent use and the server is tuned for high
// connection counts (timeouts on, bounded request bodies).
//
// Authentication is a bearer token (a personal access token bound to an
// employee, or a service account with explicit scopes). Every route requires
// authentication except GET /healthz and the POST /api/v1/organizations
// bootstrap. Errors use a flat envelope:
//
//	{"code":"NOT_FOUND","message":"...","details":{...},"request_id":"..."}
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// maxBodyBytes caps request bodies so one caller can't force large allocs.
const maxBodyBytes = 1 << 20 // 1 MiB

// errorEnvelope is the flat JSON error envelope returned by every error
// response. Codes are stable UPPER_SNAKE strings.
type errorEnvelope struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id"`
}

// Server is the HTTP API over a store.Store.
type Server struct {
	store   store.Store
	logger  *slog.Logger
	mux     *http.ServeMux
	limiter *rateLimiter
	idemMu  *keyedMutex
}

// NewServer builds the API server. Pass nil logger for a discard logger.
func NewServer(st store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		store:   st,
		logger:  logger,
		mux:     http.NewServeMux(),
		limiter: newRateLimiter(100), // 100 req/min per credential
		idemMu:  newKeyedMutex(),
	}
	s.routes()
	return s
}

// Handler returns the middleware-wrapped mux.
func (s *Server) Handler() http.Handler {
	return s.withMiddleware(s.withAuth(s.withRateLimit(s.withIdempotency(s.mux))))
}

// Serve starts the HTTP server with production timeouts. It blocks until ctx
// is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// No WriteTimeout: responses are size-bounded (1 MiB request cap,
		// paginated lists), so a slow client can't hold unbounded memory.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.handleHealth)

	m.HandleFunc("POST /api/v1/organizations", s.handleBootstrapOrganization)
	m.HandleFunc("GET /api/v1/organizations", s.handleListOrganizations)
	m.HandleFunc("GET /api/v1/organizations/{orgID}", s.handleGetOrganization)
	m.HandleFunc("PATCH /api/v1/organizations/{orgID}",
		s.withAudit("organization.updated", "organization", s.patchOrganization))

	m.HandleFunc("POST /api/v1/organizations/{orgID}/groups",
		s.withAudit("group.created", "group", s.createGroup))
	m.HandleFunc("GET /api/v1/organizations/{orgID}/groups", s.handleListGroups)
	m.HandleFunc("GET /api/v1/organizations/{orgID}/groups/{groupID}", s.handleGetGroup)
	m.HandleFunc("PATCH /api/v1/organizations/{orgID}/groups/{groupID}",
		s.withAudit("group.updated", "group", s.patchGroup))
	m.HandleFunc("DELETE /api/v1/organizations/{orgID}/groups/{groupID}",
		s.withAudit("group.deleted", "group", s.deleteGroup))

	m.HandleFunc("POST /api/v1/organizations/{orgID}/employees",
		s.withAudit("employee.created", "employee", s.createEmployee))
	m.HandleFunc("GET /api/v1/organizations/{orgID}/employees", s.handleListEmployees)
	m.HandleFunc("GET /api/v1/organizations/{orgID}/employees/{employeeID}", s.handleGetEmployee)
	m.HandleFunc("PATCH /api/v1/organizations/{orgID}/employees/{employeeID}",
		s.withAudit("employee.updated", "employee", s.patchEmployee))
	m.HandleFunc("POST /api/v1/organizations/{orgID}/employees/{employeeID}/deactivate",
		s.withAudit("employee.deactivated", "employee", s.deactivateEmployee))

	m.HandleFunc("PUT /api/v1/organizations/{orgID}/roles/{employeeID}",
		s.withAudit("role.assigned", "membership", s.setRole))
	m.HandleFunc("GET /api/v1/organizations/{orgID}/roles", s.handleListMemberships)

	m.HandleFunc("POST /api/v1/organizations/{orgID}/credentials",
		s.withAudit("credential.created", "credential", s.issueCredential))
	m.HandleFunc("GET /api/v1/organizations/{orgID}/credentials", s.handleListCredentials)
	m.HandleFunc("DELETE /api/v1/organizations/{orgID}/credentials/{credentialID}",
		s.withAudit("credential.revoked", "credential", s.revokeCredential))

	m.HandleFunc("GET /api/v1/organizations/{orgID}/audit_events", s.handleListAuditEvents)

	s.timeRoutes()
	s.leaveRoutes()
	s.webhookRoutes()
	s.scimRoutes()
}

// --- middleware ---

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := requestID()
		ctx := context.WithValue(r.Context(), requestIDKey{}, rid)
		w.Header().Set("X-Request-ID", rid)
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))
		s.logger.Info("request",
			"request_id", rid,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type requestIDKey struct{}

func requestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// requestIDOf returns the request ID attached by withMiddleware.
func requestIDOf(r *http.Request) string {
	if rid, ok := r.Context().Value(requestIDKey{}).(string); ok && rid != "" {
		return rid
	}
	return "unknown"
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the flat error envelope, carrying the request ID so
// callers can correlate with server logs.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeErrorDetails(w, r, status, code, message, nil)
}

func writeErrorDetails(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	writeJSON(w, status, errorEnvelope{
		Code:      code,
		Message:   message,
		Details:   details,
		RequestID: requestIDOf(r),
	})
}

// mapError converts store sentinel errors to HTTP responses. ok=false means
// the error was written; the handler should return.
//
// It covers the sentinels of all components: component 1 (identity), the
// component 2 time-tracking sentinels (ALREADY_CLOCKED_IN, PERIOD_LOCKED,
// INVALID_TRANSITION), and the component 3 leave sentinels
// (INSUFFICIENT_BALANCE, NOT_CURRENT_APPROVER, INVALID_TRANSITION,
// BULK_APPROVAL_FAILED with per-request results).
func mapError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, store.ErrAlreadyExists):
		writeError(w, r, http.StatusConflict, "ALREADY_EXISTS", err.Error())
	case errors.Is(err, store.ErrVersionConflict):
		writeError(w, r, http.StatusConflict, "VERSION_CONFLICT",
			"resource changed since it was read; GET it again and retry")
	case errors.Is(err, store.ErrNotEmpty):
		writeError(w, r, http.StatusConflict, "GROUP_NOT_EMPTY", err.Error())
	case errors.Is(err, store.ErrCredentialInvalid):
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
			"invalid, expired, or revoked credential")
	case errors.Is(err, store.ErrAlreadyClockedIn):
		writeError(w, r, http.StatusConflict, "ALREADY_CLOCKED_IN",
			"employee already has an open time entry")
	case errors.Is(err, store.ErrPeriodLocked):
		writeError(w, r, http.StatusConflict, "PERIOD_LOCKED",
			"entry falls in a submitted or approved pay period and cannot be changed")
	case errors.Is(err, store.ErrInvalidTransition):
		writeError(w, r, http.StatusConflict, "INVALID_TRANSITION",
			"state transition not allowed: "+err.Error())
	case errors.Is(err, store.ErrInsufficientBalance):
		writeError(w, r, http.StatusConflict, "INSUFFICIENT_BALANCE", err.Error())
	case errors.Is(err, store.ErrNotCurrentApprover):
		writeError(w, r, http.StatusConflict, "NOT_CURRENT_APPROVER", err.Error())
	default:
		var berr *store.BulkApprovalError
		if errors.As(err, &berr) {
			writeErrorDetails(w, r, http.StatusConflict, "BULK_APPROVAL_FAILED",
				"bulk approval failed; nothing was applied",
				map[string]any{"results": berr.Results})
		} else {
			var ve *store.ValidationError
			if errors.As(err, &ve) {
				writeErrorDetails(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
					ve.Message, map[string]any{"field": ve.Field})
			} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				writeError(w, r, http.StatusServiceUnavailable, "CANCELLED", "request cancelled")
			} else {
				writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
			}
		}
	}
	return false
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON: "+err.Error())
		return false
	}
	return true
}
