package mcp

// Registry tests: the 37-tool contract — names, schemas, tiers, scopes,
// annotations, and the confirm/never preview invariant.

import (
	"strings"
	"testing"

	"github.com/azmi2409/forge-hris/store"
)

var expectedTools = []string{
	// identity (7)
	"hris.list_employees", "hris.get_employee", "hris.create_employee",
	"hris.update_employee", "hris.deactivate_employee", "hris.list_departments",
	"hris.get_org_chart",
	// time (10)
	"hris.clock_in", "hris.clock_out", "hris.start_break", "hris.end_break",
	"hris.get_timesheet", "hris.list_timesheets", "hris.submit_timesheet",
	"hris.approve_timesheet", "hris.reopen_timesheet", "hris.correct_time_entry",
	// leave (8)
	"hris.list_leave_types", "hris.get_leave_balance", "hris.request_leave",
	"hris.cancel_leave_request", "hris.approve_leave", "hris.approve_leave_bulk",
	"hris.reject_leave", "hris.who_is_on_leave_today",
	// reports (4)
	"hris.get_attendance_report", "hris.get_leave_report", "hris.get_audit_log",
	"hris.get_overtime_report",
	// payroll (8)
	"hris.list_payroll_runs", "hris.get_payroll_run", "hris.draft_payroll_run",
	"hris.preview_payroll", "hris.submit_payroll", "hris.cancel_payroll_run",
	"hris.get_payslip", "hris.list_pay_schedules",
}

var expectedTiers = map[string]Tier{
	"hris.create_employee":     TierConfirm,
	"hris.deactivate_employee": TierConfirm,
	"hris.approve_timesheet":   TierConfirm,
	"hris.reopen_timesheet":    TierConfirm,
	"hris.approve_leave":       TierConfirm,
	"hris.reject_leave":        TierConfirm,
	"hris.approve_leave_bulk":  TierNever,
	"hris.submit_payroll":      TierNever,
	"hris.cancel_payroll_run":  TierConfirm,
}

func TestRegistry(t *testing.T) {
	srv := NewServer(store.NewMemoryStore(), nil)
	reg := srv.Registry()

	t.Run("count is 37", func(t *testing.T) {
		if got := len(reg.All()); got != 37 {
			t.Fatalf("registry has %d tools, want 37", got)
		}
	})

	t.Run("exact tool set", func(t *testing.T) {
		seen := map[string]bool{}
		for _, d := range reg.All() {
			if seen[d.Name] {
				t.Errorf("duplicate tool %q", d.Name)
			}
			seen[d.Name] = true
		}
		for _, name := range expectedTools {
			if !seen[name] {
				t.Errorf("missing tool %q", name)
			}
		}
		for name := range seen {
			found := false
			for _, want := range expectedTools {
				if name == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("unexpected tool %q (not in the REA tool map)", name)
			}
		}
	})

	t.Run("per-tool invariants", func(t *testing.T) {
		for _, d := range reg.All() {
			t.Run(d.Name, func(t *testing.T) {
				if !strings.HasPrefix(d.Name, "hris.") {
					t.Errorf("name %q must start with hris.", d.Name)
				}
				if strings.TrimSpace(d.Description) == "" {
					t.Error("description is empty")
				}
				if d.InputSchema["type"] != "object" {
					t.Errorf("inputSchema type = %v, want object", d.InputSchema["type"])
				}
				if d.OutputSchema["type"] != "object" {
					t.Errorf("outputSchema type = %v, want object", d.OutputSchema["type"])
				}
				if !ValidTier(d.Tier) {
					t.Errorf("tier %q is not valid", d.Tier)
				}
				if d.RequiredScope != "" && !store.ValidScope(d.RequiredScope) {
					t.Errorf("requiredScope %q is not a known scope", d.RequiredScope)
				}
				if d.MinRole != "" && !store.ValidRole(d.MinRole) {
					t.Errorf("minRole %q is not a known role", d.MinRole)
				}
				if strings.TrimSpace(d.Annotations.Title) == "" {
					t.Error("annotation title is empty")
				}
				if d.Handler == nil {
					t.Error("handler is nil")
				}
				if (d.Tier == TierConfirm || d.Tier == TierNever) && d.Preview == nil {
					t.Errorf("tier %q requires a preview function", d.Tier)
				}
				wantTier, gated := expectedTiers[d.Name]
				if gated {
					if d.Tier != wantTier {
						t.Errorf("tier = %q, want %q", d.Tier, wantTier)
					}
				} else if d.Tier != TierAuto {
					t.Errorf("tier = %q, want auto", d.Tier)
				}
			})
		}
	})

	t.Run("tier distribution", func(t *testing.T) {
		counts := map[Tier]int{}
		for _, d := range reg.All() {
			counts[d.Tier]++
		}
		if counts[TierConfirm] != 7 || counts[TierNever] != 2 || counts[TierAuto] != 28 {
			t.Errorf("tier distribution = %v, want auto:28 confirm:7 never:2", counts)
		}
	})

	t.Run("tools/list advertises tiers and annotations", func(t *testing.T) {
		listedAny, ok := srv.handleToolsList(nil).(map[string]any)
		if !ok {
			t.Fatal("tools/list did not return an object")
		}
		listed := listedAny["tools"].([]toolJSON)
		if len(listed) != 37 {
			t.Fatalf("tools/list returned %d tools, want 37", len(listed))
		}
		for _, item := range listed {
			def, ok := reg.Get(item.Name)
			if !ok {
				t.Fatalf("tools/list returned unknown tool %q", item.Name)
			}
			forge, ok := item.Meta["forge"].(map[string]any)
			if !ok {
				t.Fatalf("%s: missing _meta.forge", item.Name)
			}
			if forge["tier"] != string(def.Tier) {
				t.Errorf("%s: advertised tier %v != registry %v", item.Name, forge["tier"], def.Tier)
			}
			if forge["requiredScope"] != def.RequiredScope {
				t.Errorf("%s: advertised scope %v != registry %v", item.Name, forge["requiredScope"], def.RequiredScope)
			}
			ann := item.Annotations
			for _, key := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint", "title"} {
				if _, ok := ann[key]; !ok {
					t.Errorf("%s: annotation %q missing", item.Name, key)
				}
			}
		}
	})
}
