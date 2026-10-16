// Package ess — the Employee Self-Service portal.
//
// A tiny no-JS web app (Go html/template, minimal inline CSS) where
// employees view their own data and request leave. Authentication is
// magic-link: no passwords, no mail server required.
//
// Mount contract for the integrator (coordinator wires this into the main
// mux once the package lands):
//
//	m.Handle("/ess/", http.StripPrefix("/ess", ess.NewHandler(st)))
//
// Flow:
//  1. GET  /ess/            login page (email form)
//  2. POST /ess/login       issues a single-use magic link (15-min expiry,
//     stored as a SHA-256 digest in ess_tokens). With a
//     Mailer configured the link is emailed; without
//     one (dev) it is DISPLAYED on the page and logged.
//  3. GET  /ess/login?token=… verifies the token, sets the signed
//     ess_session cookie (HMAC, 12h), redirects to /home.
//  4. GET  /ess/home        dashboard: today's clock status, leave balances
//     GET  /ess/timesheets  my recent time entries
//     GET  /ess/leave       balances + policies + my requests + request form
//     POST /ess/leave/requests  submit a leave request
//     GET  /ess/payslips    my pay runs
//     GET  /ess/payslips/{runID}  printable payslip (reuses payroll.RenderPayslipHTML)
//     POST /ess/logout      clears the session
//
// Tenancy: every page requires a session, and every store call is scoped to
// the session's org_id + employee_id. The scoping is structural — handlers
// never accept an employee or org identifier from the request — so member A
// cannot read member B's data by construction.
package ess

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// magicLinkTTL is how long a magic link stays valid.
const magicLinkTTL = 15 * time.Minute

// sessionTTL is the ess_session cookie lifetime.
const sessionTTL = 12 * time.Hour

// sessionCookie is the session cookie name.
const sessionCookie = "ess_session"

// Mailer delivers magic links. It is the pluggable seam for real mail
// delivery (SMTP, SES, …). When no Mailer is configured the portal runs in
// dev mode: the link is displayed on the page and logged instead.
type Mailer interface {
	SendMagicLink(ctx context.Context, toEmail, link string) error
}

// EssStore is the narrow persistence contract the portal needs. The
// concrete *store.MemoryStore / *store.PostgresStore satisfy it.
type EssStore interface {
	// Identity / tenancy.
	GetOrganization(ctx context.Context, id string) (store.Organization, error)
	GetEmployee(ctx context.Context, orgID, id string) (store.Employee, error)
	FindEmployeeByEmail(ctx context.Context, email string) (string, store.Employee, error)
	// Time.
	OpenTimeEntry(ctx context.Context, orgID, employeeID string) (store.TimeEntry, bool, error)
	ListTimeEntries(ctx context.Context, orgID string, f store.TimeEntryFilter, pg store.Page) ([]store.TimeEntry, string, error)
	// Leave.
	ListTimeOffPolicies(ctx context.Context, orgID string, pg store.Page) ([]store.TimeOffPolicy, string, error)
	GetTimeOffPolicy(ctx context.Context, orgID, id string) (store.TimeOffPolicy, error)
	ListLeaveBalances(ctx context.Context, orgID string, f store.LeaveBalanceFilter, pg store.Page) ([]store.LeaveBalance, string, error)
	ListTimeOffRequests(ctx context.Context, orgID string, f store.TimeOffRequestFilter, pg store.Page) ([]store.TimeOffRequest, string, error)
	CreateTimeOffRequest(ctx context.Context, r store.TimeOffRequest, holidays []store.Holiday) (store.TimeOffRequest, error)
	// Payroll.
	ListPayrollRuns(ctx context.Context, orgID string, f store.PayrollRunFilter, pg store.Page) ([]store.PayrollRun, string, error)
	GetPayrollRun(ctx context.Context, orgID, id string) (store.PayrollRun, error)
	GetPayLine(ctx context.Context, orgID, runID, employeeID string) (store.EmployeePayLine, error)
	// ESS tokens.
	IssueESSToken(ctx context.Context, t store.ESSToken) (store.ESSToken, error)
	GetESSToken(ctx context.Context, tokenHash string) (store.ESSToken, error)
	MarkESSTokenUsed(ctx context.Context, tokenHash string, at time.Time) error
}

// Handler serves the ESS portal.
type Handler struct {
	store           EssStore
	mux             *http.ServeMux
	mailer          Mailer
	sessionKey      []byte
	devShowLinks    bool
	devShowLinksSet bool
	now             func() time.Time
	logger          *slog.Logger
	tmpl            *templates
}

// Option configures the handler.
type Option func(*Handler)

// WithMailer sets the magic-link mailer. Without one the portal shows the
// link on the page (dev mode).
func WithMailer(m Mailer) Option {
	return func(h *Handler) { h.mailer = m }
}

// WithSessionKey sets the HMAC key for session cookies. Without one a
// random key is generated per process (sessions die on restart).
func WithSessionKey(key []byte) Option {
	return func(h *Handler) { h.sessionKey = append([]byte(nil), key...) }
}

// WithNow injects the clock (tests).
func WithNow(fn func() time.Time) Option {
	return func(h *Handler) { h.now = fn }
}

// WithDevShowLinks forces the dev link display on/off. Default: on when
// no Mailer is configured, off otherwise.
func WithDevShowLinks(b bool) Option {
	return func(h *Handler) { h.devShowLinks, h.devShowLinksSet = b, true }
}

// NewHandler builds the ESS portal handler. Mount it with
// http.StripPrefix("/ess", …) so internal routes see paths like
// "/login" and "/home".
func NewHandler(st EssStore, opts ...Option) http.Handler {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("ess: random session key: %v", err))
	}
	h := &Handler{
		store:      st,
		mux:        http.NewServeMux(),
		sessionKey: key,
		now:        time.Now,
		logger:     slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(h)
	}
	if !h.devShowLinksSet {
		h.devShowLinks = h.mailer == nil
	}
	h.tmpl = newTemplates()
	h.routes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() {
	m := h.mux
	m.HandleFunc("GET /", h.handleLoginPage)
	m.HandleFunc("POST /login", h.handleLoginPost)
	m.HandleFunc("GET /login", h.handleLoginVerify)
	m.HandleFunc("GET /home", h.requireSession(h.handleHome))
	m.HandleFunc("GET /timesheets", h.requireSession(h.handleTimesheets))
	m.HandleFunc("GET /leave", h.requireSession(h.handleLeave))
	m.HandleFunc("POST /leave/requests", h.requireSession(h.handleLeaveRequestPost))
	m.HandleFunc("GET /payslips", h.requireSession(h.handlePayslips))
	m.HandleFunc("GET /payslips/{runID}", h.requireSession(h.handlePayslip))
	m.HandleFunc("POST /logout", h.requireSession(h.handleLogout))
}

// --- sessions ---

type session struct {
	OrgID      string `json:"org_id"`
	EmployeeID string `json:"employee_id"`
	Exp        int64  `json:"exp"` // unix
}

// signSession builds the cookie value: base64url(payload).base64url(sig).
func (h *Handler) signSession(s session) string {
	payload, _ := json.Marshal(s)
	mac := hmac.New(sha256.New, h.sessionKey)
	mac.Write(payload)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// verifySession parses and authenticates a cookie value.
func (h *Handler) verifySession(value string) (session, bool) {
	var s session
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return s, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return s, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return s, false
	}
	mac := hmac.New(sha256.New, h.sessionKey)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return s, false
	}
	if err := json.Unmarshal(payload, &s); err != nil {
		return s, false
	}
	if s.OrgID == "" || s.EmployeeID == "" {
		return s, false
	}
	if h.now().Unix() > s.Exp {
		return s, false
	}
	return s, true
}

func (h *Handler) setSession(w http.ResponseWriter, r *http.Request, s session) {
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    h.signSession(s),
		Path:     "/ess/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(s.Exp, 0),
	}
	if r.TLS != nil {
		c.Secure = true
	}
	http.SetCookie(w, c)
}

func (h *Handler) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/ess/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

type sessionKey struct{}

func sessionOf(r *http.Request) (session, bool) {
	s, ok := r.Context().Value(sessionKey{}).(session)
	return s, ok
}

// requireSession redirects to the login page when the session cookie is
// missing or invalid.
func (h *Handler) requireSession(next func(w http.ResponseWriter, r *http.Request, s session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/ess/", http.StatusFound)
			return
		}
		s, ok := h.verifySession(c.Value)
		if !ok {
			h.clearSession(w)
			http.Redirect(w, r, "/ess/", http.StatusFound)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)), s)
	}
}

// --- magic-link token helpers ---

func hashMagicToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func newMagicToken() (plaintext string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// magicLinkURL builds the absolute link for the current request.
func magicLinkURL(r *http.Request, token string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// r.URL.Path is "/login" after StripPrefix; re-prefix for the browser.
	return fmt.Sprintf("%s://%s/ess/login?token=%s", scheme, r.Host, token)
}

// --- small helpers ---

func (h *Handler) page(title, name string, authed bool, data map[string]any) pageData {
	if data == nil {
		data = map[string]any{}
	}
	return pageData{Title: title, Authed: authed, Page: name, Data: data}
}

func (h *Handler) render(w http.ResponseWriter, status int, p pageData) {
	h.tmpl.render(w, status, p)
}

func (h *Handler) renderError(w http.ResponseWriter, status int, title, message, back string) {
	h.render(w, status, h.page(title, "error", false, map[string]any{
		"Title": title, "Message": message, "Back": back,
	}))
}

func fullName(e store.Employee) string {
	return strings.TrimSpace(e.FirstName + " " + e.LastName)
}

// employeeOf loads the session's employee, 404ing when it vanished.
func (h *Handler) employeeOf(w http.ResponseWriter, r *http.Request, s session) (store.Employee, bool) {
	emp, err := h.store.GetEmployee(r.Context(), s.OrgID, s.EmployeeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.renderError(w, http.StatusNotFound, "Not found", "Your employee record no longer exists.", "/ess/home")
			return store.Employee{}, false
		}
		h.renderError(w, http.StatusInternalServerError, "Error", "Something went wrong.", "/ess/home")
		return store.Employee{}, false
	}
	return emp, true
}
