package api

// Component 2 API tests: the RBAC matrix over the time routes, the
// double-clock-in 409, the Idempotency-Key replay contract on clock-in, and
// the overtime preview. The store underneath is the MemoryStore.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

func rCtx() context.Context { return context.Background() }

func mustAPITime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// timeFixture holds an org with owner/manager/member tokens plus the member
// and manager employee IDs.
type timeFixture struct {
	srv     *Server
	st      store.Store
	orgID   string
	tokens  map[string]string
	member  string
	manager string
}

func newTimeFixture(t *testing.T) *timeFixture {
	t.Helper()
	srv, st := newTestServer(t)
	b := bootstrap(t, srv, "time-rbac")
	member := joinedEmployee(t, st, b.Organization.ID, "M-001", uniqEmail("member"), store.RoleMember)
	manager := joinedEmployee(t, st, b.Organization.ID, "M-002", uniqEmail("manager"), store.RoleManager)
	tokens := map[string]string{"owner": b.Token}
	mintToken(t, st, b.Organization.ID, "pat", "member", "plaintext-member-time", &member, nil)
	mintToken(t, st, b.Organization.ID, "pat", "manager", "plaintext-manager-time", &manager, nil)
	tokens["member"] = "plaintext-member-time"
	tokens["manager"] = "plaintext-manager-time"
	return &timeFixture{srv: srv, st: st, orgID: b.Organization.ID, tokens: tokens, member: member, manager: manager}
}

type errCode struct {
	Code string `json:"code"`
}

func codeOf(t *testing.T, body []byte) string {
	t.Helper()
	var e errCode
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return e.Code
}

func clockBody() map[string]any {
	return map[string]any{"occurred_at": "2026-10-12T09:00:00Z", "client_type": "ai_assistant"}
}

func TestTimeClockRequiresIdempotencyKey(t *testing.T) {
	f := newTimeFixture(t)
	rr := doReq(t, f.srv, http.MethodPost,
		"/api/v1/organizations/"+f.orgID+"/clock-in",
		f.tokens["member"], clockBody(), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if codeOf(t, rr.Body.Bytes()) != "VALIDATION_ERROR" {
		t.Fatalf("want VALIDATION_ERROR, got %s", rr.Body.String())
	}
}

func TestTimeDoubleClockIn409(t *testing.T) {
	f := newTimeFixture(t)
	path := "/api/v1/organizations/" + f.orgID + "/clock-in"
	rr := doReq(t, f.srv, http.MethodPost, path, f.tokens["member"],
		clockBody(), map[string]string{"Idempotency-Key": "ci-1"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("first clock-in: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPost, path, f.tokens["member"],
		clockBody(), map[string]string{"Idempotency-Key": "ci-2"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("second clock-in: want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := codeOf(t, rr.Body.Bytes()); code != "ALREADY_CLOCKED_IN" {
		t.Fatalf("want ALREADY_CLOCKED_IN, got %s", code)
	}
}

func TestTimeClockIdempotentReplay(t *testing.T) {
	f := newTimeFixture(t)
	base := "/api/v1/organizations/" + f.orgID
	body := clockBody()

	rr := doReq(t, f.srv, http.MethodPost, base+"/clock-in", f.tokens["member"],
		body, map[string]string{"Idempotency-Key": "replay-1"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("clock-in: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var first struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rr = doReq(t, f.srv, http.MethodPost, base+"/clock-out", f.tokens["member"],
		map[string]any{"occurred_at": "2026-10-12T18:00:00Z"},
		map[string]string{"Idempotency-Key": "replay-2"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("clock-out: want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Same key + same body replays the original 201 instead of punching
	// again (or hitting ALREADY_CLOCKED_IN).
	rr = doReq(t, f.srv, http.MethodPost, base+"/clock-in", f.tokens["member"],
		body, map[string]string{"Idempotency-Key": "replay-1"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("replay: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var replayed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &replayed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if replayed.ID != first.ID {
		t.Fatalf("replay returned entry %s, want original %s", replayed.ID, first.ID)
	}
	// And only one In entry exists for the day.
	rr = doReq(t, f.srv, http.MethodGet,
		base+"/time-entries?kind=in&from=2026-10-12T00:00:00Z&to=2026-10-13T00:00:00Z",
		f.tokens["member"], nil, nil)
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("want exactly 1 in-entry after replay, got %d", len(page.Data))
	}
}

func TestTimeMemberCannotClockForOthers(t *testing.T) {
	f := newTimeFixture(t)
	body := clockBody()
	body["employee_id"] = f.manager
	rr := doReq(t, f.srv, http.MethodPost,
		"/api/v1/organizations/"+f.orgID+"/clock-in",
		f.tokens["member"], body, map[string]string{"Idempotency-Key": "other-1"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

// createPeriod creates a Monthly definition + one October 2026 period via
// the API as the manager, returning the period ID and version.
func createPeriod(t *testing.T, f *timeFixture) (string, int64) {
	t.Helper()
	base := "/api/v1/organizations/" + f.orgID
	rr := doReq(t, f.srv, http.MethodPost, base+"/pay-period-definitions",
		f.tokens["manager"], map[string]any{
			"name": "Monthly", "cadence": "Monthly", "anchor_day": 1,
		}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create definition: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var def struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &def); err != nil {
		t.Fatalf("decode def: %v", err)
	}
	rr = doReq(t, f.srv, http.MethodPost, base+"/pay-periods",
		f.tokens["manager"], map[string]any{
			"definition_id": def.ID,
			"period_start":  "2026-10-15T00:00:00Z",
		}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create period: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var period struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &period); err != nil {
		t.Fatalf("decode period: %v", err)
	}
	return period.ID, period.Version
}

func TestTimePayPeriodRBAC(t *testing.T) {
	f := newTimeFixture(t)
	base := "/api/v1/organizations/" + f.orgID
	periodID, version := createPeriod(t, f)

	post := func(token, action string, v int64) (int, string) {
		rr := doReq(t, f.srv, http.MethodPost,
			base+"/pay-periods/"+periodID+"/"+action, token,
			map[string]any{"version": v}, nil)
		return rr.Code, codeOf(t, rr.Body.Bytes())
	}

	// Member cannot submit.
	if code, _ := post(f.tokens["member"], "submit", version); code != http.StatusForbidden {
		t.Fatalf("member submit: want 403, got %d", code)
	}
	// Manager submits.
	rr := doReq(t, f.srv, http.MethodPost, base+"/pay-periods/"+periodID+"/submit",
		f.tokens["manager"], map[string]any{"version": version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("manager submit: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var submitted struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &submitted); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Member cannot approve.
	if code, _ := post(f.tokens["member"], "approve", submitted.Version); code != http.StatusForbidden {
		t.Fatalf("member approve: want 403, got %d", code)
	}
	// Manager approves from Submitted.
	rr = doReq(t, f.srv, http.MethodPost, base+"/pay-periods/"+periodID+"/approve",
		f.tokens["manager"], map[string]any{"version": submitted.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("manager approve: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var approved struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &approved); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Approving an Open period is an invalid transition.
	period2, v2 := createPeriod(t, f)
	rr = doReq(t, f.srv, http.MethodPost, base+"/pay-periods/"+period2+"/approve",
		f.tokens["manager"], map[string]any{"version": v2}, nil)
	if rr.Code != http.StatusConflict || codeOf(t, rr.Body.Bytes()) != "INVALID_TRANSITION" {
		t.Fatalf("approve from Open: want 409 INVALID_TRANSITION, got %d %s",
			rr.Code, codeOf(t, rr.Body.Bytes()))
	}

	// Reopen is admin-only: manager gets 403, owner succeeds.
	if code, _ := post(f.tokens["manager"], "reopen", approved.Version); code != http.StatusForbidden {
		t.Fatalf("manager reopen: want 403, got %d", code)
	}
	rr = doReq(t, f.srv, http.MethodPost, base+"/pay-periods/"+periodID+"/reopen",
		f.tokens["owner"], map[string]any{"version": approved.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner reopen: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestTimePatchLockedEntry409(t *testing.T) {
	f := newTimeFixture(t)
	base := "/api/v1/organizations/" + f.orgID
	ctx := rCtx()

	// Member punches in via the store (fixed timestamp inside October).
	entry, err := f.st.ClockIn(ctx, f.orgID, f.member, store.TimeEntry{
		OccurredAt: mustAPITime("2026-10-12T09:00:00Z"),
	})
	if err != nil {
		t.Fatalf("ClockIn: %v", err)
	}

	// Manager locks the period via submit+approve.
	periodID, version := createPeriod(t, f)
	rr := doReq(t, f.srv, http.MethodPost, base+"/pay-periods/"+periodID+"/submit",
		f.tokens["manager"], map[string]any{"version": version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", rr.Code, rr.Body.String())
	}
	var sub struct {
		Version int64 `json:"version"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &sub)
	rr = doReq(t, f.srv, http.MethodPost, base+"/pay-periods/"+periodID+"/approve",
		f.tokens["manager"], map[string]any{"version": sub.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}

	// PATCH on the now-locked entry -> 409 PERIOD_LOCKED.
	rr = doReq(t, f.srv, http.MethodPatch, base+"/time-entries/"+entry.ID,
		f.tokens["member"], map[string]any{
			"address": "Menara Test",
			"version": entry.Version + 1, // store bumped it on lock
		}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("patch locked: want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := codeOf(t, rr.Body.Bytes()); code != "PERIOD_LOCKED" {
		t.Fatalf("want PERIOD_LOCKED, got %s", code)
	}
}

func TestTimeOvertimePreview(t *testing.T) {
	f := newTimeFixture(t)
	base := "/api/v1/organizations/" + f.orgID
	ctx := rCtx()

	// Two 9h days for the member: Mon + Tue 2026-10-12/13.
	for _, day := range []string{"2026-10-12", "2026-10-13"} {
		if _, err := f.st.ClockIn(ctx, f.orgID, f.member, store.TimeEntry{
			OccurredAt: mustAPITime(day + "T09:00:00Z"),
		}); err != nil {
			t.Fatalf("ClockIn: %v", err)
		}
		if _, err := f.st.ClockOut(ctx, f.orgID, f.member, store.TimeEntry{
			OccurredAt: mustAPITime(day + "T18:00:00Z"),
		}); err != nil {
			t.Fatalf("ClockOut: %v", err)
		}
	}

	rr := doReq(t, f.srv, http.MethodPost, base+"/overtime/preview",
		f.tokens["member"], map[string]any{
			"from": "2026-10-12",
			"to":   "2026-10-14",
			"rules": map[string]any{
				"daily_threshold_hours":  8,
				"double_threshold_hours": 12,
				"weekly_threshold_hours": 40,
				"overtime_mult":          1.5,
				"double_mult":            2,
				"rest_day_mult":          2,
				"holiday_mult":           3,
			},
		}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Entries int `json:"entries"`
		Result  struct {
			RegularHours float64 `json:"regular_hours"`
			Overtime15   float64 `json:"overtime_15"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Entries != 4 {
		t.Fatalf("entries = %d, want 4", out.Entries)
	}
	if out.Result.RegularHours != 16 || out.Result.Overtime15 != 2 {
		t.Fatalf("result = %+v, want {16 2}", out.Result)
	}

	// Member may not preview someone else's hours.
	rr = doReq(t, f.srv, http.MethodPost, base+"/overtime/preview",
		f.tokens["member"], map[string]any{
			"employee_id": f.manager,
			"from":        "2026-10-12",
			"to":          "2026-10-14",
			"rules":       map[string]any{},
		}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-employee preview: want 403, got %d", rr.Code)
	}
}
