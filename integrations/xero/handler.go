package xero

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/azmi2409/forge-hris/integrations/secrets"
	"github.com/azmi2409/forge-hris/store"
)

// Store is the narrow storage surface the Xero integration needs. Both
// bundled adapters satisfy it structurally (MemoryStore and PostgresStore
// implement every method); store.go is intentionally untouched.
type Store interface {
	GetXeroConnection(ctx context.Context, orgID string) (store.XeroConnection, error)
	SaveXeroConnection(ctx context.Context, c store.XeroConnection) (store.XeroConnection, error)
	DeleteXeroConnection(ctx context.Context, orgID string) error
	SaveXeroMappings(ctx context.Context, orgID string, accounts []store.XeroAccountMapping, contacts []store.XeroContactMapping) error
	GetXeroMappings(ctx context.Context, orgID string) (accounts []store.XeroAccountMapping, contacts []store.XeroContactMapping, err error)
	RecordBillPosting(ctx context.Context, p store.XeroBillPosting) (store.XeroBillPosting, error)
	ListBillPostings(ctx context.Context, orgID, runID string) ([]store.XeroBillPosting, error)
	GetPayrollRun(ctx context.Context, orgID, runID string) (store.PayrollRun, error)
	ListPayLines(ctx context.Context, orgID, runID string, p store.Page) ([]store.EmployeePayLine, string, error)
	// EnqueueWebhookEvent fans xero.bill_posted / xero.bill_failed out to
	// matching subscriptions (best-effort; never fails the caller).
	EnqueueWebhookEvent(ctx context.Context, orgID, eventType string, data any) (int, error)
}

// Compile-time guard: the bundled adapters satisfy the narrow interface.
var (
	_ Store = (*store.MemoryStore)(nil)
	_ Store = (*store.PostgresStore)(nil)
)

// Config wires the Handler.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// Client overrides the real HTTP client (tests inject the fake). When
	// nil, NewClient(ClientID, ClientSecret) is used.
	Client XeroClient
	// Scopes overrides DefaultScopes.
	Scopes []string
}

// stateTTL bounds how long an OAuth state token stays valid.
const stateTTL = 10 * time.Minute

type stateEntry struct {
	orgID     string
	expiresAt time.Time
}

// stateStore is the in-memory TTL store for OAuth state tokens: it binds
// the callback to the org that started the flow and blocks CSRF replays.
type stateStore struct {
	mu      sync.Mutex
	entries map[string]stateEntry
}

func newStateStore() *stateStore { return &stateStore{entries: make(map[string]stateEntry)} }

// issue mints a state token for orgID.
func (s *stateStore) issue(orgID string) (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("xero: random state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	s.entries[state] = stateEntry{orgID: orgID, expiresAt: time.Now().Add(stateTTL)}
	return state, nil
}

// consume validates state once and removes it (single-use).
func (s *stateStore) consume(state string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	e, ok := s.entries[state]
	if !ok {
		return "", errors.New("unknown or expired state")
	}
	delete(s.entries, state)
	return e.orgID, nil
}

func (s *stateStore) evictLocked() {
	now := time.Now()
	for k, e := range s.entries {
		if !e.expiresAt.After(now) {
			delete(s.entries, k)
		}
	}
}

// Handler serves the Xero integration HTTP surface. It is called from the
// api package's xero routes; it writes the JSON error envelope itself so the
// api layer stays a thin adapter.
type Handler struct {
	st     Store
	cfg    Config
	states *stateStore
	logger *slog.Logger
}

// NewHandler builds the Handler. A nil Client becomes the real HTTP client.
func NewHandler(st Store, cfg Config) *Handler {
	if cfg.Client == nil {
		cfg.Client = NewClient(cfg.ClientID, cfg.ClientSecret)
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = DefaultScopes
	}
	return &Handler{
		st:     st,
		cfg:    cfg,
		states: newStateStore(),
		logger: slog.New(slog.DiscardHandler),
	}
}

// WithLogger sets the logger (nil restores the discard logger).
func (h *Handler) WithLogger(l *slog.Logger) *Handler {
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	h.logger = l
	return h
}

// Client returns the Xero client (used by the auto-post hook).
func (h *Handler) Client() XeroClient { return h.cfg.Client }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"code": code, "message": message})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// authorizationURL builds the Xero OAuth2 authorize URL for one flow.
func (h *Handler) authorizationURL(state string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {h.cfg.ClientID},
		"redirect_uri":  {h.cfg.RedirectURL},
		"scope":         {strings.Join(h.cfg.Scopes, " ")},
		"state":         {state},
	}
	return AuthorizeURL + "?" + q.Encode()
}

// HandleConnect starts the OAuth flow: it returns the Xero authorization
// URL the operator visits. Admin only.
func (h *Handler) HandleConnect(w http.ResponseWriter, r *http.Request, orgID string, isAdmin bool) {
	if !isAdmin {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "requires admin role or higher")
		return
	}
	if h.cfg.ClientID == "" || h.cfg.RedirectURL == "" {
		writeErr(w, http.StatusServiceUnavailable, "XERO_NOT_CONFIGURED",
			"xero integration is not configured (missing client ID or redirect URL)")
		return
	}
	state, err := h.states.issue(orgID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not start OAuth flow")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"authorization_url": h.authorizationURL(state),
		"state":             state,
	})
}

// HandleCallback completes the OAuth flow. It is unauthenticated by design
// (Xero redirects the operator's browser here); the state token binds the
// callback to the org that started the flow.
func (h *Handler) HandleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("error") != "" {
		writeErr(w, http.StatusBadRequest, "XERO_OAUTH_DENIED",
			"xero authorization failed: "+q.Get("error_description"))
		return
	}
	code, state := q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "code and state are required")
		return
	}
	orgID, err := h.states.consume(state)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "XERO_INVALID_STATE", "invalid or expired OAuth state")
		return
	}
	toks, err := h.cfg.Client.ExchangeCode(r.Context(), code, h.cfg.RedirectURL)
	if err != nil {
		h.logger.Error("xero code exchange failed", "organization_id", orgID, "error", err)
		writeErr(w, http.StatusBadGateway, "XERO_TOKEN_EXCHANGE_FAILED", "could not exchange the authorization code")
		return
	}
	if !secrets.KeyConfigured() {
		writeErr(w, http.StatusServiceUnavailable, "XERO_KEY_NOT_CONFIGURED",
			"FORGE_HRIS_SECRET_KEY is not configured; cannot store tokens")
		return
	}
	encAccess, err := secrets.Encrypt([]byte(toks.AccessToken))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not seal access token")
		return
	}
	encRefresh, err := secrets.Encrypt([]byte(toks.RefreshToken))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not seal refresh token")
		return
	}
	conn := store.XeroConnection{
		OrganizationID:  orgID,
		EncAccessToken:  encAccess,
		EncRefreshToken: encRefresh,
		ExpiresAt:       toks.ExpiresAt,
	}
	if tl, ok := h.cfg.Client.(tenantLister); ok {
		tenants, terr := tl.ListTenants(r.Context(), toks.AccessToken)
		if terr != nil {
			h.logger.Error("xero list tenants failed", "organization_id", orgID, "error", terr)
		} else if len(tenants) > 0 {
			conn.TenantID = tenants[0].TenantID
		}
	}
	saved, err := h.st.SaveXeroConnection(r.Context(), conn)
	if err != nil {
		// Reconnects land here: an existing connection reads back for its
		// version and the tokens are replaced.
		existing, gerr := h.st.GetXeroConnection(r.Context(), orgID)
		if gerr != nil {
			h.logger.Error("xero save connection failed", "organization_id", orgID, "error", err)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not save xero connection")
			return
		}
		existing.TenantID = conn.TenantID
		existing.EncAccessToken = encAccess
		existing.EncRefreshToken = encRefresh
		existing.ExpiresAt = toks.ExpiresAt
		saved, err = h.st.SaveXeroConnection(r.Context(), existing)
		if err != nil {
			h.logger.Error("xero reconnect failed", "organization_id", orgID, "error", err)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not save xero connection")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected":  true,
		"tenant_id":  saved.TenantID,
		"auto_post":  saved.AutoPost,
		"expires_at": saved.ExpiresAt,
	})
}

// HandleStatus reports the connection state. Tokens are never returned.
func (h *Handler) HandleStatus(w http.ResponseWriter, r *http.Request, orgID string) {
	conn, err := h.st.GetXeroConnection(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"connected": false})
			return
		}
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not read xero connection")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected":        true,
		"tenant_id":        conn.TenantID,
		"auto_post":        conn.AutoPost,
		"token_expires_at": conn.ExpiresAt,
		"version":          conn.Version,
	})
}

type mappingsBody struct {
	AutoPost        bool `json:"auto_post"`
	AccountMappings []struct {
		ComponentKey string `json:"component_key"`
		AccountCode  string `json:"account_code"`
	} `json:"account_mappings"`
	ContactMappings []struct {
		EmployeeID string `json:"employee_id"`
		ContactID  string `json:"contact_id"`
	} `json:"contact_mappings"`
}

// HandleMappings replaces the org's account/contact mappings and the
// auto_post flag. Admin only.
func (h *Handler) HandleMappings(w http.ResponseWriter, r *http.Request, orgID string, isAdmin bool) {
	if !isAdmin {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "requires admin role or higher")
		return
	}
	var body mappingsBody
	if !decodeBody(w, r, &body) {
		return
	}
	conn, err := h.st.GetXeroConnection(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "XERO_NOT_CONNECTED",
				"connect xero before configuring mappings: POST .../integrations/xero/connect")
			return
		}
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not read xero connection")
		return
	}
	accounts := make([]store.XeroAccountMapping, 0, len(body.AccountMappings))
	for _, m := range body.AccountMappings {
		m.ComponentKey = strings.TrimSpace(m.ComponentKey)
		m.AccountCode = strings.TrimSpace(m.AccountCode)
		if m.ComponentKey == "" || m.AccountCode == "" {
			writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"account_mappings entries need component_key and account_code")
			return
		}
		accounts = append(accounts, store.XeroAccountMapping{
			OrganizationID: orgID, ComponentKey: m.ComponentKey, AccountCode: m.AccountCode,
		})
	}
	contacts := make([]store.XeroContactMapping, 0, len(body.ContactMappings))
	for _, m := range body.ContactMappings {
		m.EmployeeID = strings.TrimSpace(m.EmployeeID)
		m.ContactID = strings.TrimSpace(m.ContactID)
		if m.EmployeeID == "" || m.ContactID == "" {
			writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"contact_mappings entries need employee_id and contact_id")
			return
		}
		contacts = append(contacts, store.XeroContactMapping{
			OrganizationID: orgID, EmployeeID: m.EmployeeID, ContactID: m.ContactID,
		})
	}
	if err := h.st.SaveXeroMappings(r.Context(), orgID, accounts, contacts); err != nil {
		h.logger.Error("xero save mappings failed", "organization_id", orgID, "error", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not save xero mappings")
		return
	}
	conn.AutoPost = body.AutoPost
	updated, err := h.st.SaveXeroConnection(r.Context(), conn)
	if err != nil {
		h.logger.Error("xero save auto_post failed", "organization_id", orgID, "error", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not save xero connection")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auto_post":        updated.AutoPost,
		"account_mappings": accounts,
		"contact_mappings": contacts,
		"version":          updated.Version,
	})
}

// HandleDisconnect removes the connection and its mappings. Idempotent:
// disconnecting twice still returns 204. Admin only.
func (h *Handler) HandleDisconnect(w http.ResponseWriter, r *http.Request, orgID string, isAdmin bool) {
	if !isAdmin {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "requires admin role or higher")
		return
	}
	if err := h.st.DeleteXeroConnection(r.Context(), orgID); err != nil {
		h.logger.Error("xero disconnect failed", "organization_id", orgID, "error", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not disconnect xero")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandlePostBill posts one ACCPAY bill per pay line of a payroll run.
// dry_run returns the would-be bills without touching Xero. Admin only.
func (h *Handler) HandlePostBill(w http.ResponseWriter, r *http.Request, orgID, runID string, isAdmin, dryRun bool) {
	if !isAdmin {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "requires admin role or higher")
		return
	}
	outcome, dryBills, err := h.postBills(r.Context(), orgID, runID, dryRun)
	if err != nil {
		var nf *notConnectedError
		if errors.As(err, &nf) {
			writeErr(w, http.StatusNotFound, "XERO_NOT_CONNECTED",
				"connect xero before posting bills: POST .../integrations/xero/connect")
			return
		}
		var na *runNotApprovedError
		if errors.As(err, &na) {
			writeErr(w, http.StatusConflict, "XERO_RUN_NOT_APPROVED", na.Error())
			return
		}
		var xe *XeroError
		if errors.As(err, &xe) {
			writeErr(w, http.StatusBadGateway, "XERO_API_ERROR", xe.Error())
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		h.logger.Error("xero post bill failed", "organization_id", orgID, "run_id", runID, "error", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not post xero bill")
		return
	}
	if dryRun {
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "bills": dryBills})
		return
	}
	// Total failure is a 502 with the failed ledger rows; a partial
	// failure is a 201 carrying per-bill statuses (the ledger rows are
	// the source of truth in both cases).
	failed := 0
	for _, p := range outcome {
		if p.Status == store.XeroBillPostingFailed {
			failed++
		}
	}
	if len(outcome) > 0 && failed == len(outcome) {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"code": "XERO_API_ERROR", "message": "all xero bill posts failed",
			"postings": outcome,
		})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"postings": outcome})
}

// notConnectedError marks "no Xero connection" distinctly from other store
// errors so callers can map it to XERO_NOT_CONNECTED.
type notConnectedError struct{ err error }

func (e *notConnectedError) Error() string { return e.err.Error() }
func (e *notConnectedError) Unwrap() error { return e.err }

// runNotApprovedError marks a bill post against a run that is not approved.
// Bills only ever post for approved runs: the auto-post hook fires on the
// draft -> approved transition, and the manual endpoint refuses anything
// else so a draft's tentative numbers can never reach Xero.
type runNotApprovedError struct{ status store.PayrollRunStatus }

func (e *runNotApprovedError) Error() string {
	return fmt.Sprintf("payroll run is %s, not approved", e.status)
}

// tokenRefreshSkew refreshes tokens that expire within this window, so a
// bill post never races the token's expiry.
const tokenRefreshSkew = 5 * time.Minute

// accessToken returns a usable access token, refreshing and re-saving when
// the stored one is expired or near expiry.
func (h *Handler) accessToken(ctx context.Context, conn store.XeroConnection) (string, store.XeroConnection, error) {
	access, err := secrets.Decrypt(conn.EncAccessToken)
	if err != nil {
		return "", conn, fmt.Errorf("xero: decrypt access token: %w", err)
	}
	if time.Until(conn.ExpiresAt) > tokenRefreshSkew {
		return string(access), conn, nil
	}
	refresh, err := secrets.Decrypt(conn.EncRefreshToken)
	if err != nil {
		return "", conn, fmt.Errorf("xero: decrypt refresh token: %w", err)
	}
	toks, err := h.cfg.Client.RefreshTokens(ctx, string(refresh))
	if err != nil {
		return "", conn, fmt.Errorf("xero: refresh tokens: %w", err)
	}
	encAccess, err := secrets.Encrypt([]byte(toks.AccessToken))
	if err != nil {
		return "", conn, fmt.Errorf("xero: seal access token: %w", err)
	}
	encRefresh, err := secrets.Encrypt([]byte(toks.RefreshToken))
	if err != nil {
		return "", conn, fmt.Errorf("xero: seal refresh token: %w", err)
	}
	conn.EncAccessToken = encAccess
	conn.EncRefreshToken = encRefresh
	conn.ExpiresAt = toks.ExpiresAt
	updated, err := h.st.SaveXeroConnection(ctx, conn)
	if err != nil {
		return "", conn, fmt.Errorf("xero: save refreshed tokens: %w", err)
	}
	return toks.AccessToken, updated, nil
}

// NetPayAccountKey is the well-known account-mapping component key for the
// per-employee net-pay line.
const NetPayAccountKey = "payroll.net_pay"

// DefaultNetPayAccount is the Xero default chart code used when no mapping
// exists for payroll.net_pay (476 = wages & salaries in Xero's default
// chart of accounts).
const DefaultNetPayAccount = "476"

// buildBills turns a run's pay lines into one ACCPAY bill per employee.
// Contact and account codes come from the org's mappings; unmapped
// employees get a contact named by their employee ID and the default
// account code.
func buildBills(run store.PayrollRun, lines []store.EmployeePayLine,
	accounts []store.XeroAccountMapping, contacts []store.XeroContactMapping,
) []Bill {
	accountByKey := make(map[string]string, len(accounts))
	for _, a := range accounts {
		accountByKey[a.ComponentKey] = a.AccountCode
	}
	contactByEmployee := make(map[string]string, len(contacts))
	for _, c := range contacts {
		contactByEmployee[c.EmployeeID] = c.ContactID
	}
	accountCode := accountByKey[NetPayAccountKey]
	if accountCode == "" {
		accountCode = DefaultNetPayAccount
	}
	divisor := 100.0
	if run.CountryCode == "ID" {
		divisor = 1.0 // rupiah: whole units
	}
	bills := make([]Bill, 0, len(lines))
	for _, l := range lines {
		contact := BillContact{Name: "Employee " + l.EmployeeID}
		if id, ok := contactByEmployee[l.EmployeeID]; ok {
			contact = BillContact{ContactID: id}
		}
		bills = append(bills, Bill{
			Type:      "ACCPAY",
			Contact:   contact,
			Date:      time.Now().UTC().Format("2006-01-02"),
			DueDate:   time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02"),
			Reference: "PAYROLL " + run.Period + " " + l.EmployeeID,
			LineItems: []BillLineItem{{
				Description: "Net pay " + run.Period,
				Quantity:    1,
				UnitAmount:  float64(l.NetMinor) / divisor,
				AccountCode: accountCode,
				TaxType:     "NONE",
			}},
		})
	}
	return bills
}

// postBills is the shared engine behind HandlePostBill and
// MaybeAutoPostBill: it builds the bills, posts each one, and records every
// attempt in the ledger. Already-posted (run, employee) pairs are skipped,
// so the operation is idempotent. A Xero failure on one employee records a
// failed row and continues with the rest.
func (h *Handler) postBills(ctx context.Context, orgID, runID string, dryRun bool) ([]store.XeroBillPosting, []Bill, error) {
	conn, err := h.st.GetXeroConnection(ctx, orgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, &notConnectedError{err}
		}
		return nil, nil, err
	}
	run, err := h.st.GetPayrollRun(ctx, orgID, runID)
	if err != nil {
		return nil, nil, err
	}
	if run.Status != store.PayrollRunApproved {
		return nil, nil, &runNotApprovedError{status: run.Status}
	}
	var lines []store.EmployeePayLine
	cursor := ""
	for {
		page, next, err := h.st.ListPayLines(ctx, orgID, runID, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, nil, err
		}
		lines = append(lines, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	accounts, contacts, err := h.st.GetXeroMappings(ctx, orgID)
	if err != nil {
		return nil, nil, err
	}
	bills := buildBills(run, lines, accounts, contacts)
	if dryRun {
		return nil, bills, nil
	}
	accessToken, _, err := h.accessToken(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	existing, err := h.st.ListBillPostings(ctx, orgID, runID)
	if err != nil {
		return nil, nil, err
	}
	posted := make(map[string]bool, len(existing))
	for _, p := range existing {
		if p.Status == store.XeroBillPostingPosted {
			posted[p.EmployeeID] = true
		}
	}
	out := make([]store.XeroBillPosting, 0, len(lines))
	for i, l := range lines {
		if posted[l.EmployeeID] {
			continue // idempotent: this employee's bill already posted
		}
		res, berr := h.cfg.Client.CreateBill(ctx, accessToken, conn.TenantID, bills[i])
		rec := store.XeroBillPosting{
			OrganizationID: orgID,
			RunID:          runID,
			EmployeeID:     l.EmployeeID,
			IdempotencyKey: "xero-bill:" + runID + ":" + l.EmployeeID,
		}
		if berr != nil {
			rec.Status = store.XeroBillPostingFailed
			rec.Error = berr.Error()
			h.logger.Error("xero bill failed", "organization_id", orgID, "run_id", runID,
				"employee_id", l.EmployeeID, "error", berr)
		} else {
			rec.Status = store.XeroBillPostingPosted
			rec.XeroInvoiceID = res.InvoiceID
			rec.XeroInvoiceNumber = res.InvoiceNumber
		}
		saved, rerr := h.st.RecordBillPosting(ctx, rec)
		if rerr != nil {
			// A concurrent post won the race: re-read and continue.
			if errors.Is(rerr, store.ErrAlreadyExists) {
				continue
			}
			return nil, nil, rerr
		}
		out = append(out, saved)
		h.emitBillEvent(ctx, orgID, runID, l.EmployeeID, saved)
	}
	return out, nil, nil
}

// emitBillEvent fans xero.bill_posted / xero.bill_failed out. Best-effort:
// enqueue failures are logged and never fail the posting.
func (h *Handler) emitBillEvent(ctx context.Context, orgID, runID, employeeID string, p store.XeroBillPosting) {
	event := store.WebhookEventXeroBillPosted
	if p.Status == store.XeroBillPostingFailed {
		event = store.WebhookEventXeroBillFailed
	}
	if _, err := h.st.EnqueueWebhookEvent(ctx, orgID, event, map[string]any{
		"posting_id":          p.ID,
		"run_id":              runID,
		"employee_id":         employeeID,
		"status":              string(p.Status),
		"xero_invoice_id":     p.XeroInvoiceID,
		"xero_invoice_number": p.XeroInvoiceNumber,
		"error":               p.Error,
	}); err != nil {
		h.logger.Error("xero bill event enqueue failed", "organization_id", orgID, "event", event, "error", err)
	}
}

// MaybeAutoPostBill is the payroll-approval hook: when the org connected
// Xero and enabled auto_post, it posts one ACCPAY bill per pay line of the
// approved run and records every attempt. It is a strict no-op without a
// connection or with auto_post disabled.
//
// Xero-side failures are recorded as failed ledger rows, logged, and
// swallowed (the returned error is nil): auto-posting must NEVER fail the
// payroll approval. Only store-layer failures are returned, and the caller
// logs those too.
func MaybeAutoPostBill(ctx context.Context, st Store, client XeroClient, orgID, runID string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	conn, err := st.GetXeroConnection(ctx, orgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // not connected: no-op
		}
		return fmt.Errorf("xero auto-post: get connection: %w", err)
	}
	if !conn.AutoPost {
		return nil // auto-post disabled: no-op
	}
	h := NewHandler(st, Config{Client: client}).WithLogger(logger)
	postings, _, err := h.postBills(ctx, orgID, runID, false)
	if err != nil {
		var xe *XeroError
		if errors.As(err, &xe) {
			// Xero-side failure outside per-bill handling (e.g. token
			// refresh): log it, don't fail the approval.
			logger.Error("xero auto-post failed", "organization_id", orgID, "run_id", runID, "error", xe)
			return nil
		}
		return fmt.Errorf("xero auto-post: %w", err)
	}
	posted, failed := 0, 0
	for _, p := range postings {
		if p.Status == store.XeroBillPostingPosted {
			posted++
		} else {
			failed++
		}
	}
	logger.Info("xero auto-post complete", "organization_id", orgID, "run_id", runID,
		"posted", posted, "failed", failed)
	return nil
}
