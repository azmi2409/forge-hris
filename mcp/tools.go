package mcp

import (
	"context"

	"github.com/azmi2409/forge-hris/store"
)

// ToolAnnotations carries the MCP tool annotations. They are HINTS for the
// client; the server enforces tiers in executeTool regardless of them.
type ToolAnnotations struct {
	ReadOnlyHint    bool
	DestructiveHint bool
	IdempotentHint  bool
	OpenWorldHint   bool
	Title           string
}

// auditRecord is what a tool handler returns so executeTool can append one
// audit event with the agent's identity after a successful execution.
type auditRecord struct {
	action       string
	resourceType string
	resourceID   string
	before       any
	after        any
}

// HandlerFunc executes a tool. args are the JSON-decoded arguments with the
// reserved control keys (resume_token, approved, confirmed, on_behalf_of)
// already stripped. It returns the result value (marshaled into the
// content text), the audit record (nil skips auditing), or a *ToolError.
type HandlerFunc func(ctx context.Context, ai *AuthInfo, args map[string]any) (result any, audit *auditRecord, terr *ToolError)

// PreviewFunc builds the human-readable preview for a gated tool without
// mutating anything. Required for confirm/never tiers.
type PreviewFunc func(ctx context.Context, ai *AuthInfo, args map[string]any) (preview any, terr *ToolError)

// ToolDef is the single registry entry for one MCP tool: its contract,
// its gates, and its implementation. REST and MCP share the same store
// calls inside the handlers, so the two surfaces never drift.
type ToolDef struct {
	Name          string
	Description   string
	InputSchema   map[string]any // JSON Schema draft 2020-12
	OutputSchema  map[string]any // JSON Schema draft 2020-12
	RequiredScope string         // "" = no scope gate (role gate may still apply)
	MinRole       store.Role     // "" = no role gate
	Tier          Tier
	Annotations   ToolAnnotations
	Handler       HandlerFunc
	Preview       PreviewFunc
}

// Registry is the single ToolDef registry for the MCP server.
type Registry struct {
	defs   []*ToolDef
	byName map[string]*ToolDef
}

// All returns every registered tool in definition order.
func (r *Registry) All() []*ToolDef { return r.defs }

// Get returns the tool by name.
func (r *Registry) Get(name string) (*ToolDef, bool) {
	d, ok := r.byName[name]
	return d, ok
}

// --- JSON Schema helpers (draft 2020-12) ---

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func optDateProp(desc string) map[string]any {
	return map[string]any{"type": "string", "format": "date", "description": desc + " (YYYY-MM-DD)"}
}

func optTimeProp(desc string) map[string]any {
	return map[string]any{"type": "string", "format": "date-time", "description": desc + " (RFC3339)"}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func numProp(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func arrProp(items map[string]any, desc string) map[string]any {
	return map[string]any{"type": "array", "items": items, "description": desc}
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"properties":           props,
		"additionalProperties": true,
	}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func pageProps() map[string]any {
	return map[string]any{
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "Max items to return (default 100, max 1000)."},
		"cursor": map[string]any{"type": "string", "description": "Opaque cursor from a previous call's next_cursor."},
	}
}

func pageResult(itemDesc string) map[string]any {
	return objSchema(map[string]any{
		"data":        arrProp(map[string]any{"type": "object"}, itemDesc),
		"next_cursor": strProp("Opaque cursor for the next page; empty when done."),
		"has_more":    boolProp("True when another page follows."),
	})
}

func versionProp() map[string]any {
	return map[string]any{"type": "integer", "minimum": 1, "description": "Optimistic concurrency token from the last read; the call fails with VERSION_CONFLICT when stale."}
}

func employeeIDProp(desc string) map[string]any {
	if desc == "" {
		desc = "Employee ID. Members may only act for themselves; managers and above may name any employee."
	}
	return strProp(desc)
}

// readOnly marks the standard annotation set for read tools.
func readOnly(title string) ToolAnnotations {
	return ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: title}
}

// write marks the standard annotation set for mutating tools.
func write(title string) ToolAnnotations {
	return ToolAnnotations{Title: title}
}

// newRegistry builds the 29-tool registry. Handlers are methods on s so
// they share the store; every store call mirrors its REST counterpart.
func newRegistry(s *Server) *Registry {
	defs := []*ToolDef{
		// ---- identity (7) ----
		{
			Name:        "hris.list_employees",
			Description: "List employees in the organization with optional filters. Cursor-paginated.",
			InputSchema: objSchema(map[string]any{
				"group_id": strProp("Filter to one department/group."),
				"status":   strProp("Filter by invitation status: not_invited, pending_invitation, joined, removed."),
				"q":        strProp("Free-text search over name, email, employee number."),
				"limit":    pageProps()["limit"],
				"cursor":   pageProps()["cursor"],
			}),
			OutputSchema:  pageResult("Matching employees."),
			RequiredScope: store.ScopeEmployeesRead,
			Tier:          TierAuto,
			Annotations:   readOnly("List employees"),
			Handler:       s.handleListEmployees,
		},
		{
			Name:        "hris.get_employee",
			Description: "Get one employee by ID. Members may only read their own record.",
			InputSchema: objSchema(map[string]any{
				"employee_id": strProp("Employee ID."),
			}, "employee_id"),
			OutputSchema:  objSchema(map[string]any{"employee": map[string]any{"type": "object"}}),
			RequiredScope: store.ScopeEmployeesRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get employee"),
			Handler:       s.handleGetEmployee,
		},
		{
			Name:        "hris.create_employee",
			Description: "Create a new employee record. Requires human approval before it executes.",
			InputSchema: objSchema(map[string]any{
				"first_name":      strProp("First name."),
				"last_name":       strProp("Last name."),
				"email":           strProp("Work email (unique per organization)."),
				"employee_number": strProp("Employee number (unique per organization); generated when omitted."),
				"position":        strProp("Job title."),
				"employment_type": strProp("One of full_time, part_time, contract, intern."),
				"group_id":        strProp("Department/group ID."),
				"manager_id":      strProp("Manager's employee ID."),
				"time_zone":       strProp("IANA time zone, e.g. Asia/Jakarta."),
				"work_start_date": optDateProp("First working day."),
			}, "first_name", "last_name", "email"),
			OutputSchema:  objSchema(map[string]any{"employee": map[string]any{"type": "object"}}),
			RequiredScope: store.ScopeEmployeesWrite,
			Tier:          TierConfirm,
			Annotations:   write("Create employee"),
			Handler:       s.handleCreateEmployee,
			Preview:       s.previewCreateEmployee,
		},
		{
			Name:        "hris.update_employee",
			Description: "Sparse-update an employee record. Requires the current version (optimistic concurrency).",
			InputSchema: objSchema(map[string]any{
				"employee_id":     strProp("Employee ID."),
				"version":         versionProp(),
				"first_name":      strProp("First name."),
				"last_name":       strProp("Last name."),
				"email":           strProp("Work email (unique per organization)."),
				"position":        strProp("Job title."),
				"employment_type": strProp("One of full_time, part_time, contract, intern."),
				"group_id":        strProp("Department/group ID (empty string clears)."),
				"manager_id":      strProp("Manager's employee ID (empty string clears)."),
				"time_zone":       strProp("IANA time zone."),
				"work_start_date": optDateProp("First working day."),
			}, "employee_id", "version"),
			OutputSchema:  objSchema(map[string]any{"employee": map[string]any{"type": "object"}}),
			RequiredScope: store.ScopeEmployeesWrite,
			Tier:          TierAuto,
			Annotations:   write("Update employee"),
			Handler:       s.handleUpdateEmployee,
		},
		{
			Name:        "hris.deactivate_employee",
			Description: "Soft-deactivate an employee (status -> removed; the record stays for audit). Requires human approval.",
			InputSchema: objSchema(map[string]any{
				"employee_id": strProp("Employee ID."),
				"version":     versionProp(),
			}, "employee_id", "version"),
			OutputSchema:  objSchema(map[string]any{"employee": map[string]any{"type": "object"}}),
			RequiredScope: store.ScopeEmployeesWrite,
			Tier:          TierConfirm,
			Annotations:   ToolAnnotations{DestructiveHint: true, Title: "Deactivate employee"},
			Handler:       s.handleDeactivateEmployee,
			Preview:       s.previewDeactivateEmployee,
		},
		{
			Name:        "hris.list_departments",
			Description: "List departments/teams (groups) in the organization. Cursor-paginated.",
			InputSchema: objSchema(map[string]any{
				"limit":  pageProps()["limit"],
				"cursor": pageProps()["cursor"],
			}),
			OutputSchema:  pageResult("Departments."),
			RequiredScope: store.ScopeEmployeesRead,
			Tier:          TierAuto,
			Annotations:   readOnly("List departments"),
			Handler:       s.handleListDepartments,
		},
		{
			Name:        "hris.get_org_chart",
			Description: "Get the organization chart: departments plus the employee manager graph with direct reports.",
			InputSchema: objSchema(map[string]any{}),
			OutputSchema: objSchema(map[string]any{
				"groups":    arrProp(map[string]any{"type": "object"}, "Departments with member counts."),
				"employees": arrProp(map[string]any{"type": "object"}, "Employees with manager_id and direct_reports."),
				"roots":     arrProp(map[string]any{"type": "string"}, "Employee IDs with no manager."),
			}),
			RequiredScope: store.ScopeEmployeesRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get org chart"),
			Handler:       s.handleGetOrgChart,
		},

		// ---- time (10) ----
		{
			Name:        "hris.clock_in",
			Description: "Clock in: open a time entry for an employee. Fails with ALREADY_CLOCKED_IN when an entry is already open.",
			InputSchema: objSchema(clockProps()),
			OutputSchema: objSchema(map[string]any{
				"time_entry": map[string]any{"type": "object", "description": "The created In entry."},
			}),
			RequiredScope: store.ScopeTimeWrite,
			Tier:          TierAuto,
			Annotations:   write("Clock in"),
			Handler:       s.handleClockIn,
		},
		{
			Name:        "hris.clock_out",
			Description: "Clock out: close the employee's open time entry. Safe to retry with the same idempotency_key.",
			InputSchema: objSchema(clockProps()),
			OutputSchema: objSchema(map[string]any{
				"time_entry": map[string]any{"type": "object", "description": "The created Out entry."},
			}),
			RequiredScope: store.ScopeTimeWrite,
			Tier:          TierAuto,
			Annotations:   ToolAnnotations{IdempotentHint: true, Title: "Clock out"},
			Handler:       s.handleClockOut,
		},
		{
			Name:        "hris.start_break",
			Description: "Start a break inside the employee's open clock entry.",
			InputSchema: objSchema(clockProps()),
			OutputSchema: objSchema(map[string]any{
				"time_entry": map[string]any{"type": "object", "description": "The created StartBreak entry."},
			}),
			RequiredScope: store.ScopeTimeWrite,
			Tier:          TierAuto,
			Annotations:   write("Start break"),
			Handler:       s.handleStartBreak,
		},
		{
			Name:        "hris.end_break",
			Description: "End the employee's open break.",
			InputSchema: objSchema(clockProps()),
			OutputSchema: objSchema(map[string]any{
				"time_entry": map[string]any{"type": "object", "description": "The created EndBreak entry."},
			}),
			RequiredScope: store.ScopeTimeWrite,
			Tier:          TierAuto,
			Annotations:   write("End break"),
			Handler:       s.handleEndBreak,
		},
		{
			Name:        "hris.get_timesheet",
			Description: "Get one timesheet (pay period): the period, the employee's entries inside it, and any open entry.",
			InputSchema: objSchema(map[string]any{
				"period_id":   strProp("Pay period (timesheet) ID."),
				"employee_id": employeeIDProp(""),
			}, "period_id"),
			OutputSchema: objSchema(map[string]any{
				"pay_period":  map[string]any{"type": "object"},
				"entries":     arrProp(map[string]any{"type": "object"}, "Time entries inside the period."),
				"open_entry":  map[string]any{"type": []string{"object", "null"}, "description": "The employee's open entry, if any."},
				"employee_id": strProp("Resolved employee."),
			}),
			RequiredScope: store.ScopeTimeRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get timesheet"),
			Handler:       s.handleGetTimesheet,
		},
		{
			Name:        "hris.list_timesheets",
			Description: "List timesheets (pay periods) with optional filters. Cursor-paginated.",
			InputSchema: objSchema(map[string]any{
				"definition_id": strProp("Filter to one pay-period definition."),
				"status":        strProp("Filter by status: Open, Submitted, Approved, Rejected, Reopened."),
				"limit":         pageProps()["limit"],
				"cursor":        pageProps()["cursor"],
			}),
			OutputSchema:  pageResult("Pay periods."),
			RequiredScope: store.ScopeTimeRead,
			Tier:          TierAuto,
			Annotations:   readOnly("List timesheets"),
			Handler:       s.handleListTimesheets,
		},
		{
			Name:        "hris.submit_timesheet",
			Description: "Submit a timesheet (Open -> Submitted). Entries inside freeze while it awaits a decision.",
			InputSchema: objSchema(map[string]any{
				"period_id": strProp("Pay period (timesheet) ID."),
				"version":   versionProp(),
			}, "period_id", "version"),
			OutputSchema: objSchema(map[string]any{
				"pay_period": map[string]any{"type": "object"},
			}),
			MinRole:     store.RoleManager,
			Tier:        TierAuto,
			Annotations: write("Submit timesheet"),
			Handler:     s.handleSubmitTimesheet,
		},
		{
			Name:        "hris.approve_timesheet",
			Description: "Approve a submitted timesheet. Locks every entry it covers. Requires human approval.",
			InputSchema: objSchema(map[string]any{
				"period_id": strProp("Pay period (timesheet) ID."),
				"version":   versionProp(),
			}, "period_id", "version"),
			OutputSchema: objSchema(map[string]any{
				"pay_period": map[string]any{"type": "object"},
			}),
			MinRole:     store.RoleManager,
			Tier:        TierConfirm,
			Annotations: write("Approve timesheet"),
			Handler:     s.handleApproveTimesheet,
			Preview:     s.previewTimesheetTransition,
		},
		{
			Name:        "hris.reopen_timesheet",
			Description: "Reopen an approved timesheet (Approved -> Reopened). Unlocks the entries it covers. Requires human approval.",
			InputSchema: objSchema(map[string]any{
				"period_id": strProp("Pay period (timesheet) ID."),
				"version":   versionProp(),
			}, "period_id", "version"),
			OutputSchema: objSchema(map[string]any{
				"pay_period": map[string]any{"type": "object"},
			}),
			MinRole:     store.RoleAdmin,
			Tier:        TierConfirm,
			Annotations: write("Reopen timesheet"),
			Handler:     s.handleReopenTimesheet,
			Preview:     s.previewTimesheetTransition,
		},
		{
			Name:        "hris.correct_time_entry",
			Description: "Correct a time entry: the original is frozen (never edited) and a superseding entry is created, linked via prev/next.",
			InputSchema: objSchema(map[string]any{
				"entry_id":    strProp("Time entry ID to correct."),
				"kind":        strProp("Corrected punch kind: in, out, start_break, end_break."),
				"occurred_at": optTimeProp("When the punch happened; defaults to now."),
				"project_id":  strProp("Project tag."),
				"latitude":    numProp("GPS latitude."),
				"longitude":   numProp("GPS longitude."),
				"address":     strProp("Address label."),
				"client_type": strProp("Client type: web, mobile, desktop, kiosk, ai_assistant."),
			}, "entry_id", "kind"),
			OutputSchema: objSchema(map[string]any{
				"time_entry": map[string]any{"type": "object", "description": "The superseding head entry."},
			}),
			RequiredScope: store.ScopeTimeWrite,
			Tier:          TierAuto,
			Annotations:   write("Correct time entry"),
			Handler:       s.handleCorrectTimeEntry,
		},

		// ---- leave (8) ----
		{
			Name:        "hris.list_leave_types",
			Description: "List leave types (time-off policies) in the organization. Cursor-paginated.",
			InputSchema: objSchema(map[string]any{
				"limit":  pageProps()["limit"],
				"cursor": pageProps()["cursor"],
			}),
			OutputSchema:  pageResult("Time-off policies."),
			RequiredScope: store.ScopeLeaveRead,
			Tier:          TierAuto,
			Annotations:   readOnly("List leave types"),
			Handler:       s.handleListLeaveTypes,
		},
		{
			Name:        "hris.get_leave_balance",
			Description: "Get an employee's leave balance for a policy, recalculated from tenure, requests, carry-forward and expiry.",
			InputSchema: objSchema(map[string]any{
				"employee_id":         employeeIDProp(""),
				"policy_id":           strProp("Time-off policy ID."),
				"as_of":               optTimeProp("Recalculate as of this time; defaults to now."),
				"holiday_calendar_id": strProp("Holiday calendar used to count working days."),
			}, "employee_id", "policy_id"),
			OutputSchema: objSchema(map[string]any{
				"balance": map[string]any{"type": "object", "description": "The recalculated leave balance."},
			}),
			RequiredScope: store.ScopeLeaveRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get leave balance"),
			Handler:       s.handleGetLeaveBalance,
		},
		{
			Name:        "hris.request_leave",
			Description: "Request leave for an employee. Days are store-computed from dates and holidays; the balance is checked for paid leave.",
			InputSchema: objSchema(map[string]any{
				"employee_id":         employeeIDProp(""),
				"policy_id":           strProp("Time-off policy ID."),
				"start_date":          optDateProp("First day of leave."),
				"end_date":            optDateProp("Last day of leave, inclusive."),
				"is_unpaid":           boolProp("Unpaid leave skips balance checks (deducted from pay later)."),
				"holiday_calendar_id": strProp("Holiday calendar used to count working days."),
				"approver_ids":        arrProp(map[string]any{"type": "string"}, "Explicit approver chain (managers and above only); defaults to the employee's manager."),
			}, "employee_id", "policy_id", "start_date", "end_date"),
			OutputSchema: objSchema(map[string]any{
				"request": map[string]any{"type": "object", "description": "The created leave request."},
			}),
			RequiredScope: store.ScopeLeaveWrite,
			Tier:          TierAuto,
			Annotations:   write("Request leave"),
			Handler:       s.handleRequestLeave,
		},
		{
			Name:        "hris.cancel_leave_request",
			Description: "Cancel a requested or approved leave request (status -> cancelled; never deleted).",
			InputSchema: objSchema(map[string]any{
				"request_id": strProp("Leave request ID."),
				"version":    versionProp(),
			}, "request_id", "version"),
			OutputSchema: objSchema(map[string]any{
				"request": map[string]any{"type": "object"},
			}),
			RequiredScope: store.ScopeLeaveWrite,
			Tier:          TierAuto,
			Annotations:   ToolAnnotations{DestructiveHint: true, Title: "Cancel leave request"},
			Handler:       s.handleCancelLeaveRequest,
		},
		{
			Name:        "hris.approve_leave",
			Description: "Approve a leave request (records the current approval-chain step's decision). Requires human approval.",
			InputSchema: objSchema(map[string]any{
				"request_id": strProp("Leave request ID."),
				"version":    versionProp(),
			}, "request_id", "version"),
			OutputSchema: objSchema(map[string]any{
				"request":   map[string]any{"type": "object"},
				"approvals": arrProp(map[string]any{"type": "object"}, "Full approval chain after the decision."),
			}),
			RequiredScope: store.ScopeLeaveApprove,
			Tier:          TierConfirm,
			Annotations:   write("Approve leave"),
			Handler:       s.handleApproveLeave,
			Preview:       s.previewLeaveDecision,
		},
		{
			Name:        "hris.approve_leave_bulk",
			Description: "Bulk-approve leave requests, all-or-nothing. NEVER auto-approved: the first call returns a preview with counts; the follow-up must carry the resume token AND confirmed:true.",
			InputSchema: objSchema(map[string]any{
				"request_ids": arrProp(map[string]any{"type": "string"}, "Leave request IDs to approve."),
				"confirmed":   boolProp("Set true on the follow-up call, together with the resume token, to execute."),
			}, "request_ids"),
			OutputSchema: objSchema(map[string]any{
				"approved": arrProp(map[string]any{"type": "object"}, "Approved requests."),
			}),
			RequiredScope: store.ScopeLeaveApprove,
			Tier:          TierNever,
			Annotations:   ToolAnnotations{DestructiveHint: true, Title: "Approve leave (bulk)"},
			Handler:       s.handleBulkApproveLeave,
			Preview:       s.previewBulkApproveLeave,
		},
		{
			Name:        "hris.reject_leave",
			Description: "Reject a leave request (records the current approval-chain step's decision). Requires human approval.",
			InputSchema: objSchema(map[string]any{
				"request_id": strProp("Leave request ID."),
				"version":    versionProp(),
			}, "request_id", "version"),
			OutputSchema: objSchema(map[string]any{
				"request":   map[string]any{"type": "object"},
				"approvals": arrProp(map[string]any{"type": "object"}, "Full approval chain after the decision."),
			}),
			RequiredScope: store.ScopeLeaveApprove,
			Tier:          TierConfirm,
			Annotations:   write("Reject leave"),
			Handler:       s.handleRejectLeave,
			Preview:       s.previewLeaveDecision,
		},
		{
			Name:        "hris.who_is_on_leave_today",
			Description: "List employees with approved leave overlapping a date (defaults to today, UTC).",
			InputSchema: objSchema(map[string]any{
				"date": optDateProp("Date to check; defaults to today."),
			}),
			OutputSchema: objSchema(map[string]any{
				"date":    strProp("The checked date."),
				"entries": arrProp(map[string]any{"type": "object"}, "Employees on leave, with request/policy names."),
			}),
			RequiredScope: store.ScopeLeaveRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Who is on leave today"),
			Handler:       s.handleWhoIsOnLeave,
		},

		// ---- reports (4) ----
		{
			Name:        "hris.get_attendance_report",
			Description: "Attendance report: an employee's time entries over a date range plus any open entry.",
			InputSchema: objSchema(map[string]any{
				"employee_id": employeeIDProp(""),
				"from":        optDateProp("Range start, inclusive."),
				"to":          optDateProp("Range end, exclusive."),
				"limit":       pageProps()["limit"],
				"cursor":      pageProps()["cursor"],
			}, "from", "to"),
			OutputSchema:  pageResult("Time entries in the range."),
			RequiredScope: store.ScopeTimeRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get attendance report"),
			Handler:       s.handleGetAttendanceReport,
		},
		{
			Name:        "hris.get_leave_report",
			Description: "Leave report: leave requests (optionally filtered) plus the balance ledger rows.",
			InputSchema: objSchema(map[string]any{
				"employee_id": employeeIDProp(""),
				"policy_id":   strProp("Filter requests/balances to one policy."),
				"status":      strProp("Filter requests: requested, approved, rejected, cancelled."),
				"period":      strProp("Filter balances to an annual period label, e.g. 2026."),
				"limit":       pageProps()["limit"],
				"cursor":      pageProps()["cursor"],
			}),
			OutputSchema: objSchema(map[string]any{
				"requests": arrProp(map[string]any{"type": "object"}, "Matching leave requests."),
				"balances": arrProp(map[string]any{"type": "object"}, "Matching balance ledger rows."),
			}),
			RequiredScope: store.ScopeLeaveRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get leave report"),
			Handler:       s.handleGetLeaveReport,
		},
		{
			Name:        "hris.get_audit_log",
			Description: "Read the append-only, hash-chained audit log with optional filters. Cursor-paginated.",
			InputSchema: objSchema(map[string]any{
				"actor_type":    strProp("Filter: human, agent, system, scim."),
				"action":        strProp("Filter by action, e.g. leave.approved."),
				"resource_type": strProp("Filter by resource type, e.g. leave_request."),
				"since":         optTimeProp("Only events at or after this time."),
				"limit":         pageProps()["limit"],
				"cursor":        pageProps()["cursor"],
			}),
			OutputSchema:  pageResult("Audit events."),
			RequiredScope: store.ScopeAuditRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get audit log"),
			Handler:       s.handleGetAuditLog,
		},
		{
			Name:        "hris.get_overtime_report",
			Description: "Overtime report: pair an employee's clock entries over a date range into work intervals and bucket the hours (regular / 1.5x / 2x / rest-day / holiday).",
			InputSchema: objSchema(map[string]any{
				"employee_id":            employeeIDProp(""),
				"from":                   optDateProp("Range start, inclusive."),
				"to":                     optDateProp("Range end, exclusive."),
				"daily_threshold_hours":  numProp("Regular hours/day before 1.5x (default 8; <=0 disables)."),
				"double_threshold_hours": numProp("Hours/day before 2x (default 0 = disabled)."),
				"weekly_threshold_hours": numProp("Regular hours/ISO-week before weekly 1.5x (default 0 = disabled)."),
				"holidays":               arrProp(map[string]any{"type": "string", "format": "date"}, "Public holidays (YYYY-MM-DD) bucketed at the holiday multiplier."),
			}, "from", "to"),
			OutputSchema: objSchema(map[string]any{
				"employee_id": strProp("Resolved employee."),
				"from":        strProp("Range start."),
				"to":          strProp("Range end."),
				"entries":     intProp("Clock entries considered."),
				"result":      map[string]any{"type": "object", "description": "Hour buckets: regular_hours, overtime_15, overtime_2x, rest_day_hours, holiday_hours."},
			}),
			RequiredScope: store.ScopeTimeRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get overtime report"),
			Handler:       s.handleGetOvertimeReport,
		},
	}
	byName := make(map[string]*ToolDef, len(defs))
	for _, d := range defs {
		byName[d.Name] = d
	}
	return &Registry{defs: defs, byName: byName}
}

// clockProps returns the shared argument shape of the four clock tools,
// mirroring the REST clockRequest body plus the optional idempotency key.
func clockProps() map[string]any {
	return map[string]any{
		"employee_id":     employeeIDProp(""),
		"occurred_at":     optTimeProp("When the punch happened; defaults to now."),
		"project_id":      strProp("Project tag."),
		"latitude":        numProp("GPS latitude."),
		"longitude":       numProp("GPS longitude."),
		"address":         strProp("Address label."),
		"client_type":     strProp("Client type: web, mobile, desktop, kiosk, ai_assistant."),
		"idempotency_key": strProp("Optional idempotency key: a retry with the same key and identical arguments replays the first result instead of punching twice."),
	}
}
