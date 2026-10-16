package store

// MemoryStore webhook tests: subscription CRUD and validation, enqueue
// fan-out to matching subscriptions only, and the claim/mark delivery
// lifecycle (lease, backoff, dead after MaxWebhookAttempts).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustWebhookOrg(t *testing.T, s *MemoryStore) Organization {
	t.Helper()
	org, err := s.CreateOrganization(context.Background(), Organization{
		Name: "Webhook Co", Slug: "webhook-co-" + newID()[:8], CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return org
}

func mustWebhookSub(t *testing.T, s *MemoryStore, orgID string, events []string, active bool) WebhookSubscription {
	t.Helper()
	sub, err := s.CreateWebhookSubscription(context.Background(), WebhookSubscription{
		OrganizationID: orgID,
		URL:            "https://hooks.example.test/deliver",
		Secret:         "s3cr3t-" + newID()[:8],
		Events:         events,
		Active:         active,
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	return sub
}

func TestWebhookSubscriptionValidation(t *testing.T) {
	s := NewMemoryStore()
	org := mustWebhookOrg(t, s)
	ctx := context.Background()

	bad := []WebhookSubscription{
		{OrganizationID: org.ID, URL: "", Secret: "x", Events: []string{"*"}},                              // no URL
		{OrganizationID: org.ID, URL: "ftp://x.test/h", Secret: "x", Events: []string{"*"}},                // bad scheme
		{OrganizationID: org.ID, URL: "https://x.test/h", Secret: "", Events: []string{"*"}},               // no secret
		{OrganizationID: org.ID, URL: "https://x.test/h", Secret: "x", Events: nil},                        // no events
		{OrganizationID: org.ID, URL: "https://x.test/h", Secret: "x", Events: []string{"employee.bogus"}}, // unknown event
		{OrganizationID: "missing-org", URL: "https://x.test/h", Secret: "x", Events: []string{"*"}},       // unknown org
	}
	for i, sub := range bad {
		if _, err := s.CreateWebhookSubscription(ctx, sub); err == nil {
			t.Fatalf("case %d: want error, got nil", i)
		}
	}
}

func TestWebhookSubscriptionCRUD(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	org := mustWebhookOrg(t, s)

	sub := mustWebhookSub(t, s, org.ID, []string{WebhookEventEmployeeCreated}, true)
	if sub.ID == "" || sub.CreatedAt.IsZero() {
		t.Fatalf("unexpected created subscription: %+v", sub)
	}

	got, err := s.GetWebhookSubscription(ctx, org.ID, sub.ID)
	if err != nil || got.URL != sub.URL {
		t.Fatalf("GetWebhookSubscription: %+v %v", got, err)
	}
	if _, err := s.GetWebhookSubscription(ctx, org.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing sub: want ErrNotFound, got %v", err)
	}
	// Cross-org isolation: the id exists but not under the other org.
	other := mustWebhookOrg(t, s)
	if _, err := s.GetWebhookSubscription(ctx, other.ID, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org get: want ErrNotFound, got %v", err)
	}

	sub.URL = "https://hooks.example.test/other"
	sub.Events = []string{WebhookEventWildcard}
	sub.Active = false
	updated, err := s.UpdateWebhookSubscription(ctx, sub)
	if err != nil || updated.URL != sub.URL || updated.Active {
		t.Fatalf("UpdateWebhookSubscription: %+v %v", updated, err)
	}
	sub.ID = "missing"
	if _, err := s.UpdateWebhookSubscription(ctx, sub); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: want ErrNotFound, got %v", err)
	}
	sub.ID = updated.ID

	// A second subscription for the list walk.
	mustWebhookSub(t, s, org.ID, []string{"*"}, true)
	subs, next, err := s.ListWebhookSubscriptions(ctx, org.ID, Page{Limit: 1})
	if err != nil || len(subs) != 1 || next == "" {
		t.Fatalf("list page 1: %d subs next=%q err=%v", len(subs), next, err)
	}
	subs2, next2, err := s.ListWebhookSubscriptions(ctx, org.ID, Page{Limit: 1, Cursor: next})
	if err != nil || len(subs2) != 1 || next2 != "" || subs2[0].ID == subs[0].ID {
		t.Fatalf("list page 2: %+v next=%q err=%v", subs2, next2, err)
	}
	// Other org sees nothing.
	subs3, _, err := s.ListWebhookSubscriptions(ctx, other.ID, Page{})
	if err != nil || len(subs3) != 0 {
		t.Fatalf("cross-org list: %+v %v", subs3, err)
	}

	if err := s.DeleteWebhookSubscription(ctx, org.ID, sub.ID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}
	if _, err := s.GetWebhookSubscription(ctx, org.ID, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
	if err := s.DeleteWebhookSubscription(ctx, org.ID, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

func TestEnqueueWebhookEventFanOut(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	org := mustWebhookOrg(t, s)

	matching := mustWebhookSub(t, s, org.ID, []string{WebhookEventEmployeeCreated}, true)
	wildcard := mustWebhookSub(t, s, org.ID, []string{"*"}, true)
	mustWebhookSub(t, s, org.ID, []string{"*"}, false) // inactive: never fanned out
	mustWebhookSub(t, s, org.ID, []string{WebhookEventLeaveRequested}, true)

	n, err := s.EnqueueWebhookEvent(ctx, org.ID, WebhookEventEmployeeCreated, map[string]any{"id": "e1"})
	if err != nil || n != 2 {
		t.Fatalf("enqueue employee.created: n=%d err=%v, want 2", n, err)
	}
	n, err = s.EnqueueWebhookEvent(ctx, org.ID, WebhookEventLeaveRequested, map[string]any{"id": "l1"})
	if err != nil || n != 2 { // wildcard + leave.requested
		t.Fatalf("enqueue leave.requested: n=%d err=%v, want 2", n, err)
	}
	n, err = s.EnqueueWebhookEvent(ctx, org.ID, WebhookEventTimesheetSubmitted, map[string]any{})
	if err != nil || n != 1 { // wildcard only
		t.Fatalf("enqueue timesheet.submitted: n=%d err=%v, want 1", n, err)
	}
	if _, err := s.EnqueueWebhookEvent(ctx, org.ID, "bogus.event", nil); err == nil {
		t.Fatalf("enqueue unknown event: want error, got nil")
	}

	// The fanned-out messages carry the right subscription ids.
	msgs, err := s.ClaimWebhookDeliveries(ctx, 100)
	if err != nil {
		t.Fatalf("ClaimWebhookDeliveries: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("claimed %d, want 5", len(msgs))
	}
	seen := map[string]int{}
	for _, m := range msgs {
		seen[m.SubscriptionID]++
		if m.EventType == "" || len(m.Payload) == 0 {
			t.Fatalf("bad message: %+v", m)
		}
	}
	if seen[matching.ID] != 1 || seen[wildcard.ID] != 3 {
		t.Fatalf("fan-out distribution wrong: %+v", seen)
	}
}

// requeueForTest resets a message's next_attempt_at so it is claimable
// again (simulates the backoff elapsing). In-package only.
func requeueForTest(s *MemoryStore, id string) {
	m := s.webhookOutbox[id]
	m.NextAttemptAt = time.Now().UTC().Add(-time.Second)
	s.webhookOutbox[id] = m
}

func TestWebhookClaimMarkFlows(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	org := mustWebhookOrg(t, s)
	sub := mustWebhookSub(t, s, org.ID, []string{"*"}, true)

	n, err := s.EnqueueWebhookEvent(ctx, org.ID, WebhookEventEmployeeCreated, map[string]any{"id": "e1"})
	if err != nil || n != 1 {
		t.Fatalf("enqueue: n=%d err=%v", n, err)
	}

	claimed, err := s.ClaimWebhookDeliveries(ctx, 20)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d %v", len(claimed), err)
	}
	id := claimed[0].ID
	if claimed[0].SubscriptionID != sub.ID || claimed[0].Status != WebhookStatusPending {
		t.Fatalf("bad claim: %+v", claimed[0])
	}
	// Leased: an immediate second claim finds nothing.
	claimed, err = s.ClaimWebhookDeliveries(ctx, 20)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("second claim: %d %v, want 0 (leased)", len(claimed), err)
	}

	// Delivered: gone from the claimable set.
	if err := s.MarkWebhookDelivered(ctx, id); err != nil {
		t.Fatalf("MarkWebhookDelivered: %v", err)
	}
	if m := s.webhookOutbox[id]; m.Status != WebhookStatusSent {
		t.Fatalf("after delivered: status=%q", m.Status)
	}
	if err := s.MarkWebhookDelivered(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deliver missing: want ErrNotFound, got %v", err)
	}

	// Failure path: backoff then dead.
	if _, err := s.EnqueueWebhookEvent(ctx, org.ID, WebhookEventEmployeeCreated, nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, _ = s.ClaimWebhookDeliveries(ctx, 20)
	if len(claimed) != 1 {
		t.Fatalf("claim 2: %d", len(claimed))
	}
	failID := claimed[0].ID
	wantDelays := []time.Duration{30 * time.Second, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	for i, want := range wantDelays {
		if err := s.MarkWebhookFailed(ctx, failID, "boom"); err != nil {
			t.Fatalf("MarkWebhookFailed %d: %v", i+1, err)
		}
		m := s.webhookOutbox[failID]
		if m.Status != WebhookStatusPending || m.Attempts != i+1 || m.LastError != "boom" {
			t.Fatalf("after failure %d: %+v", i+1, m)
		}
		if got := time.Until(m.NextAttemptAt); got < want-time.Minute || got > want+time.Minute {
			t.Fatalf("failure %d: next attempt in %v, want ~%v", i+1, got, want)
		}
		requeueForTest(s, failID)
		claimed, _ = s.ClaimWebhookDeliveries(ctx, 20)
		if len(claimed) != 1 {
			t.Fatalf("re-claim after failure %d: %d", i+1, len(claimed))
		}
	}
	// Fifth failure: dead.
	if err := s.MarkWebhookFailed(ctx, failID, "boom"); err != nil {
		t.Fatalf("MarkWebhookFailed 5: %v", err)
	}
	if m := s.webhookOutbox[failID]; m.Status != WebhookStatusDead || m.Attempts != 5 {
		t.Fatalf("after 5th failure: %+v", m)
	}
	requeueForTest(s, failID)
	claimed, _ = s.ClaimWebhookDeliveries(ctx, 20)
	if len(claimed) != 0 {
		t.Fatalf("dead message claimed: %d", len(claimed))
	}
	if err := s.MarkWebhookFailed(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fail missing: want ErrNotFound, got %v", err)
	}
}

func TestEnqueueForSubscription(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	org := mustWebhookOrg(t, s)
	sub := mustWebhookSub(t, s, org.ID, []string{WebhookEventEmployeeCreated}, true)
	mustWebhookSub(t, s, org.ID, []string{"*"}, true) // must NOT receive the test event

	id, err := s.EnqueueWebhookEventForSubscription(ctx, org.ID, sub.ID,
		WebhookEventWebhookTest, map[string]any{"note": "hi"})
	if err != nil || id == "" {
		t.Fatalf("EnqueueWebhookEventForSubscription: %q %v", id, err)
	}
	claimed, err := s.ClaimWebhookDeliveries(ctx, 20)
	if err != nil || len(claimed) != 1 || claimed[0].ID != id || claimed[0].SubscriptionID != sub.ID {
		t.Fatalf("claimed: %+v %v", claimed, err)
	}
	if claimed[0].EventType != WebhookEventWebhookTest {
		t.Fatalf("event type: %q", claimed[0].EventType)
	}
	if !strings.Contains(string(claimed[0].Payload), "hi") {
		t.Fatalf("payload: %s", claimed[0].Payload)
	}

	if _, err := s.EnqueueWebhookEventForSubscription(ctx, org.ID, "missing",
		WebhookEventWebhookTest, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing sub: want ErrNotFound, got %v", err)
	}
	inactive := mustWebhookSub(t, s, org.ID, []string{"*"}, false)
	if _, err := s.EnqueueWebhookEventForSubscription(ctx, org.ID, inactive.ID,
		WebhookEventWebhookTest, nil); err == nil {
		t.Fatalf("inactive sub: want error, got nil")
	}
}
