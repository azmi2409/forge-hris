package ess

// ESS portal tests: magic-link issue/verify, session auth, and —
// critically — own-data-only enforcement (A can never read B's
// timesheets, leave, or payslips).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// Compile-time check: the concrete stores satisfy the narrow interface.
var (
	_ EssStore = (*store.MemoryStore)(nil)
	_ EssStore = (*store.PostgresStore)(nil)
)

type fixture struct {
	st     *store.MemoryStore
	orgID  string
	empAID string
	empBID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemoryStore()
	org, err := st.CreateOrganization(ctx, store.Organization{
		Name: "ESS Co", Slug: "ess-co", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	mkEmp := func(num, email string) string {
		e, err := st.CreateEmployee(ctx, store.Employee{
			OrganizationID: org.ID, EmployeeNumber: num,
			FirstName: "Emp" + num, LastName: "Test", Email: email,
			Status: store.InvitationJoined,
		})
		if err != nil {
			t.Fatalf("CreateEmployee %s: %v", num, err)
		}
		return e.ID
	}
	return &fixture{
		st:     st,
		orgID:  org.ID,
		empAID: mkEmp("A-1", "a@ess.test"),
		empBID: mkEmp("B-1", "b@ess.test"),
	}
}

// serve wraps the handler exactly like the production mount contract.
func serve(st *store.MemoryStore, opts ...Option) http.Handler {
	return http.StripPrefix("/ess", NewHandler(st, opts...))
}

func doReq(h http.Handler, method, path string, form url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

var tokenRe = regexp.MustCompile(`/ess/login\?token=([A-Za-z0-9_-]+)`)

// loginAs runs the full magic-link flow for email and returns the session
// cookies.
func loginAs(t *testing.T, h http.Handler, email string) []*http.Cookie {
	t.Helper()
	rr := doReq(h, http.MethodPost, "/ess/login", url.Values{"email": {email}}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /login: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	m := tokenRe.FindStringSubmatch(rr.Body.String())
	if len(m) != 2 {
		t.Fatalf("no magic link on the page (dev mode): %s", rr.Body.String())
	}
	rr = doReq(h, http.MethodGet, "/ess/login?token="+m[1], nil, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("GET /login?token=: want 302, got %d: %s", rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie set")
	}
	// Follow the redirect: home must greet the employee.
	rr = doReq(h, http.MethodGet, "/ess/home", nil, cookies)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /home: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	return cookies
}

func TestLoginFlow(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	cookies := loginAs(t, h, "a@ess.test")
	rr := doReq(h, http.MethodGet, "/ess/home", nil, cookies)
	if !strings.Contains(rr.Body.String(), "EmpA-1") {
		t.Fatalf("home should greet employee A: %s", rr.Body.String())
	}
}

func TestLoginUnknownEmailNoEnumeration(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	rr := doReq(h, http.MethodPost, "/ess/login", url.Values{"email": {"nobody@ess.test"}}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Check your email") {
		t.Fatalf("same page for unknown email: %s", rr.Body.String())
	}
	if tokenRe.MatchString(rr.Body.String()) {
		t.Fatal("must not issue a link for an unknown email")
	}
}

func TestTokenSingleUse(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	rr := doReq(h, http.MethodPost, "/ess/login", url.Values{"email": {"a@ess.test"}}, nil)
	m := tokenRe.FindStringSubmatch(rr.Body.String())
	if len(m) != 2 {
		t.Fatalf("no link: %s", rr.Body.String())
	}
	if rr := doReq(h, http.MethodGet, "/ess/login?token="+m[1], nil, nil); rr.Code != http.StatusFound {
		t.Fatalf("first use: want 302, got %d", rr.Code)
	}
	rr = doReq(h, http.MethodGet, "/ess/login?token="+m[1], nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("second use: want 401, got %d", rr.Code)
	}
}

func TestTokenExpired(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	ctx := context.Background()
	// Issue an already-expired token directly in the store.
	rec, err := f.st.IssueESSToken(ctx, store.ESSToken{
		TokenHash:      hashMagicToken("expired-plaintext"),
		OrganizationID: f.orgID, EmployeeID: f.empAID,
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("IssueESSToken: %v", err)
	}
	_ = rec
	rr := doReq(h, http.MethodGet, "/ess/login?token=expired-plaintext", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: want 401, got %d", rr.Code)
	}
}

func TestSessionRequired(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	for _, path := range []string{"/ess/home", "/ess/timesheets", "/ess/leave", "/ess/payslips"} {
		rr := doReq(h, http.MethodGet, path, nil, nil)
		if rr.Code != http.StatusFound {
			t.Fatalf("GET %s without session: want 302, got %d", path, rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/ess/" {
			t.Fatalf("GET %s: want redirect to /ess/, got %q", path, loc)
		}
	}
}

func TestSessionTampered(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	cookies := loginAs(t, h, "a@ess.test")
	cookies[0].Value += "tampered"
	rr := doReq(h, http.MethodGet, "/ess/home", nil, cookies)
	if rr.Code != http.StatusFound {
		t.Fatalf("tampered session: want 302, got %d", rr.Code)
	}
}

func TestLogout(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	cookies := loginAs(t, h, "a@ess.test")
	rr := doReq(h, http.MethodPost, "/ess/logout", nil, cookies)
	if rr.Code != http.StatusFound {
		t.Fatalf("logout: want 302, got %d", rr.Code)
	}
	setCookie := rr.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "ess_session=") || !strings.Contains(setCookie, "Max-Age=0") && !strings.Contains(setCookie, "max-age=0") {
		t.Fatalf("logout must clear the cookie: %q", setCookie)
	}
}

// --- own-data-only enforcement ---

func TestOwnDataTimesheets(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	ctx := context.Background()
	// A clocks in at 08:00, B at 09:30 — distinguishable timestamps.
	atA := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	atB := time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)
	if _, err := f.st.ClockIn(ctx, f.orgID, f.empAID, store.TimeEntry{OccurredAt: atA}); err != nil {
		t.Fatalf("ClockIn A: %v", err)
	}
	if _, err := f.st.ClockIn(ctx, f.orgID, f.empBID, store.TimeEntry{OccurredAt: atB}); err != nil {
		t.Fatalf("ClockIn B: %v", err)
	}
	cookies := loginAs(t, h, "a@ess.test")
	rr := doReq(h, http.MethodGet, "/ess/timesheets", nil, cookies)
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if !strings.Contains(body, "2026-10-15 08:00") {
		t.Fatalf("A's own entry must be visible: %s", body)
	}
	if strings.Contains(body, "2026-10-15 09:30") {
		t.Fatalf("B's entry must NOT be visible to A: %s", body)
	}
}

func seedPayslip(t *testing.T, f *fixture, empID, period string, netMinor int64) string {
	t.Helper()
	ctx := context.Background()
	run, err := f.st.CreatePayrollRun(ctx, store.PayrollRun{
		OrganizationID: f.orgID, Period: period, CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreatePayrollRun: %v", err)
	}
	if _, err := f.st.UpsertPayLine(ctx, store.EmployeePayLine{
		OrganizationID: f.orgID, RunID: run.ID, EmployeeID: empID,
		GrossMinor: netMinor, NetMinor: netMinor,
	}); err != nil {
		t.Fatalf("UpsertPayLine: %v", err)
	}
	run, err = f.st.TransitionPayrollRun(ctx, f.orgID, run.ID, run.Version, store.PayrollRunApproved, "test")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return run.ID
}

func TestOwnDataPayslips(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	// B has a payslip; A has none.
	runB := seedPayslip(t, f, f.empBID, "2026-09", 500_000)

	cookies := loginAs(t, h, "a@ess.test")

	// The list must not show B's run (A has no line on it).
	rr := doReq(h, http.MethodGet, "/ess/payslips", nil, cookies)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "2026-09") {
		t.Fatalf("B's run must not appear in A's list: %s", rr.Body.String())
	}

	// Direct fetch of B's run as A: 404.
	rr = doReq(h, http.MethodGet, "/ess/payslips/"+runB, nil, cookies)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("A fetching B's payslip: want 404, got %d", rr.Code)
	}

	// A's own payslip renders.
	runA := seedPayslip(t, f, f.empAID, "2026-08", 450_000)
	rr = doReq(h, http.MethodGet, "/ess/payslips/"+runA, nil, cookies)
	if rr.Code != http.StatusOK {
		t.Fatalf("A's own payslip: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "EmpA-1") || !strings.Contains(rr.Body.String(), "Net pay") {
		t.Fatalf("payslip should render employee + net pay: %s", rr.Body.String()[:500])
	}
}

func TestOwnDataCrossOrg(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	ctx := context.Background()
	// A second org with its own employee and payslip.
	org2, err := f.st.CreateOrganization(ctx, store.Organization{
		Name: "Other Co", Slug: "other-co", CountryCode: "ID",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	empC, err := f.st.CreateEmployee(ctx, store.Employee{
		OrganizationID: org2.ID, EmployeeNumber: "C-1",
		FirstName: "EmpC", Email: "c@other.test", Status: store.InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	run, err := f.st.CreatePayrollRun(ctx, store.PayrollRun{
		OrganizationID: org2.ID, Period: "2026-09", CountryCode: "ID",
	})
	if err != nil {
		t.Fatalf("CreatePayrollRun: %v", err)
	}
	if _, err := f.st.UpsertPayLine(ctx, store.EmployeePayLine{
		OrganizationID: org2.ID, RunID: run.ID, EmployeeID: empC.ID, NetMinor: 1_000_000,
	}); err != nil {
		t.Fatalf("UpsertPayLine: %v", err)
	}

	cookies := loginAs(t, h, "a@ess.test")
	rr := doReq(h, http.MethodGet, "/ess/payslips/"+run.ID, nil, cookies)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org payslip: want 404, got %d", rr.Code)
	}
}

func TestLeaveRequestFlow(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	ctx := context.Background()
	pol, err := f.st.CreateTimeOffPolicy(ctx, store.TimeOffPolicy{
		OrganizationID: f.orgID, Name: "Annual",
		AccrualMode: store.AccrualMonthly, AccrualAmount: 2,
	})
	if err != nil {
		t.Fatalf("CreateTimeOffPolicy: %v", err)
	}
	// Seed a balance so the paid request passes the balance check: give
	// employee A a year of tenure so monthly accrual produces days.
	emp, err := f.st.GetEmployee(ctx, f.orgID, f.empAID)
	if err != nil {
		t.Fatalf("GetEmployee: %v", err)
	}
	wsd := time.Now().AddDate(-1, 0, 0)
	emp.WorkStartDate = &wsd
	if _, err := f.st.UpdateEmployee(ctx, emp); err != nil {
		t.Fatalf("UpdateEmployee: %v", err)
	}
	if _, err := f.st.RecalculateLeaveBalance(ctx, f.orgID, f.empAID, pol.ID, time.Now(), nil); err != nil {
		t.Fatalf("RecalculateLeaveBalance: %v", err)
	}
	cookies := loginAs(t, h, "a@ess.test")

	rr := doReq(h, http.MethodPost, "/ess/leave/requests", url.Values{
		"policy_id":  {pol.ID},
		"start_date": {"2026-11-02"},
		"end_date":   {"2026-11-03"},
	}, cookies)
	if rr.Code != http.StatusFound {
		t.Fatalf("leave request: want 302, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(h, http.MethodGet, "/ess/leave", nil, cookies)
	if !strings.Contains(rr.Body.String(), "2026-11-02") {
		t.Fatalf("request should appear in my requests: %s", rr.Body.String())
	}
}

func TestLeaveRequestUnknownPolicy(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	cookies := loginAs(t, h, "a@ess.test")
	rr := doReq(h, http.MethodPost, "/ess/leave/requests", url.Values{
		"policy_id":  {"nope"},
		"start_date": {"2026-11-02"},
		"end_date":   {"2026-11-03"},
	}, cookies)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown policy: want 400, got %d", rr.Code)
	}
}

func TestConcurrentSessions(t *testing.T) {
	f := newFixture(t)
	h := serve(f.st)
	cookiesA := loginAs(t, h, "a@ess.test")
	cookiesB := loginAs(t, h, "b@ess.test")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := cookiesA
			if i%2 == 1 {
				c = cookiesB
			}
			rr := doReq(h, http.MethodGet, "/ess/home", nil, c)
			if rr.Code != http.StatusOK {
				t.Errorf("concurrent home: want 200, got %d", rr.Code)
			}
		}(i)
	}
	wg.Wait()
}
