package ess

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/azmi2409/forge-hris/payroll"
	"github.com/azmi2409/forge-hris/store"
)

// --- login flow ---

// handleLoginPage renders the email form, or bounces to /home when a
// valid session already exists.
func (h *Handler) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, ok := h.verifySession(c.Value); ok {
			http.Redirect(w, r, "/ess/home", http.StatusFound)
			return
		}
	}
	h.render(w, http.StatusOK, h.page("Sign in", "login", false, nil))
}

// handleLoginPost issues a single-use magic link for the email's employee.
func (h *Handler) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderError(w, http.StatusBadRequest, "Bad request", "Couldn't read the form.", "/ess/")
		return
	}
	email := strings.TrimSpace(r.Form.Get("email"))
	if email == "" || !strings.Contains(email, "@") {
		h.render(w, http.StatusBadRequest, h.page("Sign in", "login", false, nil))
		return
	}
	ctx := r.Context()
	orgID, emp, err := h.store.FindEmployeeByEmail(ctx, email)
	if err != nil {
		// Do not reveal whether the email exists: same page either way.
		h.render(w, http.StatusOK, h.page("Check your email", "login_sent", false, map[string]any{
			"Email": email,
		}))
		return
	}
	token, err := newMagicToken()
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Error", "Couldn't create a sign-in link.", "/ess/")
		return
	}
	if _, err := h.store.IssueESSToken(ctx, store.ESSToken{
		TokenHash:      hashMagicToken(token),
		OrganizationID: orgID,
		EmployeeID:     emp.ID,
		ExpiresAt:      h.now().Add(magicLinkTTL),
	}); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Error", "Couldn't create a sign-in link.", "/ess/")
		return
	}
	link := magicLinkURL(r, token)
	if h.mailer != nil {
		if err := h.mailer.SendMagicLink(ctx, emp.Email, link); err != nil {
			h.logger.Error("ess mailer failed", "email", emp.Email, "error", err)
			// Fall through to the dev display so the user isn't stranded
			// when the mailer is broken; the link is still single-use.
		} else {
			h.render(w, http.StatusOK, h.page("Check your email", "login_sent", false, map[string]any{
				"Email": email,
			}))
			return
		}
	}
	if h.devShowLinks {
		h.logger.Info("ess magic link (dev)", "email", emp.Email, "link", link)
	}
	h.render(w, http.StatusOK, h.page("Check your email", "login_sent", false, map[string]any{
		"Email":   email,
		"DevLink": devLinkOrEmpty(h.devShowLinks, link),
	}))
}

func devLinkOrEmpty(show bool, link string) string {
	if show {
		return link
	}
	return ""
}

// handleLoginVerify redeems ?token=: single-use, 15-min expiry. On success
// it sets the session cookie and redirects to /home.
func (h *Handler) handleLoginVerify(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Redirect(w, r, "/ess/", http.StatusFound)
		return
	}
	ctx := r.Context()
	now := h.now()
	rec, err := h.store.GetESSToken(ctx, hashMagicToken(token))
	if err != nil || rec.Used() || rec.Expired(now) {
		h.renderError(w, http.StatusUnauthorized, "Link invalid",
			"This sign-in link is invalid, expired, or already used. Request a new one.", "/ess/")
		return
	}
	if err := h.store.MarkESSTokenUsed(ctx, rec.TokenHash, now); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Error", "Couldn't redeem the link.", "/ess/")
		return
	}
	// Confirm the employee still exists and is joined before minting the
	// session.
	emp, err := h.store.GetEmployee(ctx, rec.OrganizationID, rec.EmployeeID)
	if err != nil || emp.Status != store.InvitationJoined {
		h.renderError(w, http.StatusUnauthorized, "Link invalid",
			"This sign-in link is no longer valid for your account.", "/ess/")
		return
	}
	h.setSession(w, r, session{
		OrgID:      rec.OrganizationID,
		EmployeeID: rec.EmployeeID,
		Exp:        now.Add(sessionTTL).Unix(),
	})
	http.Redirect(w, r, "/ess/home", http.StatusFound)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request, _ session) {
	h.clearSession(w)
	http.Redirect(w, r, "/ess/", http.StatusFound)
}

// --- home ---

type balanceView struct {
	PolicyName string
	PolicyID   string
	Period     string
	Accrued    float64
	Taken      float64
	Balance    float64
}

func (h *Handler) balancesOf(r *http.Request, s session) []balanceView {
	ctx := r.Context()
	bals, _, err := h.store.ListLeaveBalances(ctx, s.OrgID,
		store.LeaveBalanceFilter{EmployeeID: s.EmployeeID}, store.Page{})
	if err != nil {
		return nil
	}
	pols, _, _ := h.store.ListTimeOffPolicies(ctx, s.OrgID, store.Page{})
	names := map[string]string{}
	for _, p := range pols {
		names[p.ID] = p.Name
	}
	var out []balanceView
	for _, b := range bals {
		name := names[b.PolicyID]
		if name == "" {
			name = b.PolicyID
		}
		out = append(out, balanceView{
			PolicyName: name, PolicyID: b.PolicyID, Period: b.Period,
			Accrued: b.Accrued, Taken: b.Taken, Balance: b.Balance,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PolicyName != out[j].PolicyName {
			return out[i].PolicyName < out[j].PolicyName
		}
		return out[i].Period > out[j].Period
	})
	return out
}

func (h *Handler) handleHome(w http.ResponseWriter, r *http.Request, s session) {
	ctx := r.Context()
	emp, ok := h.employeeOf(w, r, s)
	if !ok {
		return
	}
	org, err := h.store.GetOrganization(ctx, s.OrgID)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Error", "Something went wrong.", "/ess/home")
		return
	}
	data := map[string]any{
		"EmployeeName":   fullName(emp),
		"EmployeeNumber": emp.EmployeeNumber,
		"OrgName":        org.Name,
		"ClockedIn":      false,
		"Balances":       h.balancesOf(r, s),
	}
	if open, isOpen, err := h.store.OpenTimeEntry(ctx, s.OrgID, s.EmployeeID); err == nil && isOpen {
		data["ClockedIn"] = true
		data["ClockSince"] = open.OccurredAt.Format("15:04")
		data["OnBreak"] = open.Kind == store.TimeEntryStartBreak
	}
	h.render(w, http.StatusOK, h.page("Home", "home", true, data))
}

// --- timesheets ---

func kindLabel(k store.TimeEntryKind) string {
	switch k {
	case store.TimeEntryIn:
		return "Clock in"
	case store.TimeEntryOut:
		return "Clock out"
	case store.TimeEntryStartBreak:
		return "Break start"
	case store.TimeEntryEndBreak:
		return "Break end"
	default:
		return string(k)
	}
}

func (h *Handler) handleTimesheets(w http.ResponseWriter, r *http.Request, s session) {
	ctx := r.Context()
	from := h.now().AddDate(0, 0, -30)
	entries, _, err := h.store.ListTimeEntries(ctx, s.OrgID, store.TimeEntryFilter{
		EmployeeID: s.EmployeeID,
		From:       &from,
	}, store.Page{Limit: 200})
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Error", "Couldn't load timesheets.", "/ess/home")
		return
	}
	// Newest first for display.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].OccurredAt.After(entries[j].OccurredAt)
	})
	rows := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, map[string]string{
			"When": e.OccurredAt.Format("2006-01-02 15:04"),
			"Kind": kindLabel(e.Kind),
		})
	}
	h.render(w, http.StatusOK, h.page("Timesheets", "timesheets", true, map[string]any{
		"Entries": rows,
	}))
}

// --- leave ---

func (h *Handler) handleLeave(w http.ResponseWriter, r *http.Request, s session) {
	ctx := r.Context()
	pols, _, err := h.store.ListTimeOffPolicies(ctx, s.OrgID, store.Page{})
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Error", "Couldn't load leave data.", "/ess/home")
		return
	}
	reqs, _, err := h.store.ListTimeOffRequests(ctx, s.OrgID,
		store.TimeOffRequestFilter{EmployeeID: s.EmployeeID}, store.Page{Limit: 100})
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Error", "Couldn't load leave data.", "/ess/home")
		return
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].CreatedAt.After(reqs[j].CreatedAt) })
	reqRows := make([]map[string]any, 0, len(reqs))
	for _, q := range reqs {
		reqRows = append(reqRows, map[string]any{
			"Start": q.StartDate, "End": q.EndDate,
			"Days": fmt.Sprintf("%.1f", q.Days), "Status": string(q.Status),
		})
	}
	polRows := make([]map[string]string, 0, len(pols))
	for _, p := range pols {
		polRows = append(polRows, map[string]string{"ID": p.ID, "Name": p.Name})
	}
	h.render(w, http.StatusOK, h.page("Leave", "leave", true, map[string]any{
		"Balances": h.balancesOf(r, s),
		"Policies": polRows,
		"Requests": reqRows,
	}))
}

// handleLeaveRequestPost submits a leave request as the session's employee.
func (h *Handler) handleLeaveRequestPost(w http.ResponseWriter, r *http.Request, s session) {
	if err := r.ParseForm(); err != nil {
		h.renderError(w, http.StatusBadRequest, "Bad request", "Couldn't read the form.", "/ess/leave")
		return
	}
	policyID := strings.TrimSpace(r.Form.Get("policy_id"))
	start := strings.TrimSpace(r.Form.Get("start_date"))
	end := strings.TrimSpace(r.Form.Get("end_date"))
	if policyID == "" || start == "" || end == "" {
		h.renderError(w, http.StatusBadRequest, "Missing fields",
			"Policy, start date and end date are required.", "/ess/leave")
		return
	}
	// The policy must belong to the session's org (no cross-tenant policy).
	pol, err := h.store.GetTimeOffPolicy(r.Context(), s.OrgID, policyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.renderError(w, http.StatusBadRequest, "Unknown policy",
				"That leave policy doesn't exist in your organization.", "/ess/leave")
			return
		}
		h.renderError(w, http.StatusInternalServerError, "Error", "Something went wrong.", "/ess/leave")
		return
	}
	req, err := h.store.CreateTimeOffRequest(r.Context(), store.TimeOffRequest{
		OrganizationID: s.OrgID,
		EmployeeID:     s.EmployeeID,
		PolicyID:       pol.ID,
		StartDate:      start,
		EndDate:        end,
	}, nil)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, store.ErrInsufficientBalance) {
			msg = "You don't have enough leave balance for that request."
		}
		h.renderError(w, http.StatusBadRequest, "Couldn't submit", msg, "/ess/leave")
		return
	}
	h.logger.Info("ess leave request", "org", s.OrgID, "employee", s.EmployeeID, "request", req.ID)
	http.Redirect(w, r, "/ess/leave", http.StatusFound)
}

// --- payslips ---

func (h *Handler) handlePayslips(w http.ResponseWriter, r *http.Request, s session) {
	ctx := r.Context()
	runs, _, err := h.store.ListPayrollRuns(ctx, s.OrgID, store.PayrollRunFilter{}, store.Page{Limit: 100})
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Error", "Couldn't load payslips.", "/ess/home")
		return
	}
	// Only runs that actually have this employee's pay line, newest first.
	sort.Slice(runs, func(i, j int) bool { return runs[i].Period > runs[j].Period })
	rows := make([]map[string]string, 0, len(runs))
	for _, run := range runs {
		if run.Status != store.PayrollRunApproved && run.Status != store.PayrollRunLocked {
			continue
		}
		line, err := h.store.GetPayLine(ctx, s.OrgID, run.ID, s.EmployeeID)
		if err != nil {
			continue // no pay line for this employee on this run
		}
		rows = append(rows, map[string]string{
			"ID": run.ID, "Period": run.Period, "Status": string(run.Status),
			"Net": formatMoney(line.NetMinor, run.CountryCode),
		})
	}
	h.render(w, http.StatusOK, h.page("Payslips", "payslips", true, map[string]any{
		"Runs": rows,
	}))
}

// handlePayslip renders one printable payslip. Own-data-only is structural:
// the run is fetched in the session's org, the line in the session's org for
// the session's employee, and both are cross-checked before rendering.
func (h *Handler) handlePayslip(w http.ResponseWriter, r *http.Request, s session) {
	ctx := r.Context()
	runID := r.PathValue("runID")
	run, err := h.store.GetPayrollRun(ctx, s.OrgID, runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.renderError(w, http.StatusNotFound, "Not found", "That payslip doesn't exist.", "/ess/payslips")
			return
		}
		h.renderError(w, http.StatusInternalServerError, "Error", "Something went wrong.", "/ess/payslips")
		return
	}
	if run.OrganizationID != s.OrgID {
		h.renderError(w, http.StatusNotFound, "Not found", "That payslip doesn't exist.", "/ess/payslips")
		return
	}
	line, err := h.store.GetPayLine(ctx, s.OrgID, run.ID, s.EmployeeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.renderError(w, http.StatusNotFound, "Not found", "That payslip doesn't exist.", "/ess/payslips")
			return
		}
		h.renderError(w, http.StatusInternalServerError, "Error", "Something went wrong.", "/ess/payslips")
		return
	}
	if line.OrganizationID != s.OrgID || line.EmployeeID != s.EmployeeID || line.RunID != run.ID {
		h.renderError(w, http.StatusNotFound, "Not found", "That payslip doesn't exist.", "/ess/payslips")
		return
	}
	emp, ok := h.employeeOf(w, r, s)
	if !ok {
		return
	}
	org, err := h.store.GetOrganization(ctx, s.OrgID)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Error", "Something went wrong.", "/ess/payslips")
		return
	}
	currency := "MYR"
	if strings.ToUpper(run.CountryCode) == "ID" {
		currency = "IDR"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(payroll.RenderPayslipHTML(payroll.PayslipData{
		CompanyName:     org.Name,
		EmployeeName:    fullName(emp),
		EmployeeNumber:  emp.EmployeeNumber,
		Period:          run.Period,
		Currency:        currency,
		Items:           line.Items,
		GrossMinor:      line.GrossMinor,
		DeductionsMinor: line.DeductionsMinor,
		NetMinor:        line.NetMinor,
		Splits:          line.Statutory,
	})))
}

// formatMoney renders minor units for the payslip list.
func formatMoney(minor int64, countryCode string) string {
	if strings.ToUpper(countryCode) == "ID" {
		return fmt.Sprintf("Rp %d", minor)
	}
	neg := minor < 0
	if neg {
		minor = -minor
	}
	s := fmt.Sprintf("RM %d.%02d", minor/100, minor%100)
	if neg {
		s = "-" + s
	}
	return s
}
