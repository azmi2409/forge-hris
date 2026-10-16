package mcp

import (
	"context"

	"github.com/azmi2409/forge-hris/store"
)

// Webhook emission for MCP tool mutations, mirroring the REST API's
// emitWebhook taxonomy (store.WebhookEvent*). The mapping is keyed on tool
// name rather than audit action because the two use different vocabularies
// (e.g. audit "time_entry.clock_in" vs webhook "clock_in.created").
//
// Emission is best-effort: enqueue failures are logged, never returned — the
// tool mutation already succeeded.
var toolWebhookEvents = map[string]string{
	"hris.create_employee":     store.WebhookEventEmployeeCreated,
	"hris.update_employee":     store.WebhookEventEmployeeUpdated,
	"hris.deactivate_employee": store.WebhookEventEmployeeDeactivated,
	"hris.clock_in":            store.WebhookEventClockInCreated,
	"hris.submit_timesheet":    store.WebhookEventTimesheetSubmitted,
	"hris.approve_timesheet":   store.WebhookEventTimesheetApproved,
	"hris.reopen_timesheet":    store.WebhookEventTimesheetReopened,
	"hris.request_leave":       store.WebhookEventLeaveRequested,
	"hris.approve_leave":       store.WebhookEventLeaveApproved,
	"hris.reject_leave":        store.WebhookEventLeaveRejected,
}

// emitWebhookEvent fans a successful tool mutation out to webhook
// subscriptions. Bulk approval emits one leave.approved event per request,
// mirroring the REST bulk-approve behavior.
func (s *Server) emitWebhookEvent(ctx context.Context, ai *AuthInfo, toolName string, result any) {
	if toolName == "hris.approve_leave_bulk" {
		s.emitBulkLeaveApproved(ctx, ai, result)
		return
	}
	eventType, ok := toolWebhookEvents[toolName]
	if !ok {
		return
	}
	if _, err := s.store.EnqueueWebhookEvent(ctx, ai.OrgID, eventType, map[string]any{
		"tool":   toolName,
		"via":    "mcp",
		"result": result,
	}); err != nil {
		s.logger.Error("mcp webhook enqueue failed",
			"tool", toolName, "event", eventType, "error", err)
	}
}

func (s *Server) emitBulkLeaveApproved(ctx context.Context, ai *AuthInfo, result any) {
	items, _ := result.(map[string]any)["approved"].([]store.TimeOffRequest)
	if len(items) == 0 {
		// Fall back to a single event carrying the raw result when the
		// shape is unexpected; never silently drop the notification.
		if _, err := s.store.EnqueueWebhookEvent(ctx, ai.OrgID, store.WebhookEventLeaveApproved, map[string]any{
			"tool": "hris.approve_leave_bulk", "via": "mcp", "result": result,
		}); err != nil {
			s.logger.Error("mcp webhook enqueue failed",
				"tool", "hris.approve_leave_bulk", "error", err)
		}
		return
	}
	for _, req := range items {
		if _, err := s.store.EnqueueWebhookEvent(ctx, ai.OrgID, store.WebhookEventLeaveApproved, map[string]any{
			"tool": "hris.approve_leave_bulk", "via": "mcp", "request": req,
		}); err != nil {
			s.logger.Error("mcp webhook enqueue failed",
				"tool", "hris.approve_leave_bulk", "request_id", req.ID, "error", err)
		}
	}
}
