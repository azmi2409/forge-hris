package store

// Postgres adapter webhook tests: the same contract as the MemoryStore
// (subscription CRUD, enqueue fan-out, claim/mark lifecycle), run against a
// real database. They skip cleanly without TEST_DATABASE_URL, using the
// testPostgresHRIS helper from postgres_test.go (same package).

import (
	"errors"
	"testing"
	"time"
)

func mustPgWebhookOrg(t *testing.T, s *PostgresStore) string {
	t.Helper()
	org, err := s.CreateOrganization(t.Context(), Organization{
		Name: "PG Webhook Co", Slug: "pg-webhook-" + newID()[:8], CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return org.ID
}

func mustPgWebhookSub(t *testing.T, s *PostgresStore, orgID string, events []string, active bool) WebhookSubscription {
	t.Helper()
	sub, err := s.CreateWebhookSubscription(t.Context(), WebhookSubscription{
		OrganizationID: orgID,
		URL:            "https://hooks.example.test/deliver",
		Secret:         "pg-s3cr3t-" + newID()[:8],
		Events:         events,
		Active:         active,
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	return sub
}

// pgRequeueForTest forces a message's next_attempt_at into the past so it
// is claimable again (simulates the backoff elapsing).
func pgRequeueForTest(t *testing.T, s *PostgresStore, id string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE webhook_outbox SET next_attempt_at = now() - interval '1 second' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("requeue: %v", err)
	}
}

func TestPostgresWebhookSubscriptionCRUD(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	orgID := mustPgWebhookOrg(t, s)

	sub := mustPgWebhookSub(t, s, orgID, []string{WebhookEventEmployeeCreated}, true)
	got, err := s.GetWebhookSubscription(ctx, orgID, sub.ID)
	if err != nil || got.URL != sub.URL || got.Secret != sub.Secret {
		t.Fatalf("GetWebhookSubscription: %+v %v", got, err)
	}
	if _, err := s.GetWebhookSubscription(ctx, orgID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: want ErrNotFound, got %v", err)
	}

	sub.Events = []string{WebhookEventWildcard}
	sub.Active = false
	updated, err := s.UpdateWebhookSubscription(ctx, sub)
	if err != nil || updated.Active || len(updated.Events) != 1 {
		t.Fatalf("UpdateWebhookSubscription: %+v %v", updated, err)
	}

	if err := s.DeleteWebhookSubscription(ctx, orgID, sub.ID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}
	if _, err := s.GetWebhookSubscription(ctx, orgID, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
}

func TestPostgresWebhookEnqueueAndClaim(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	orgID := mustPgWebhookOrg(t, s)

	matching := mustPgWebhookSub(t, s, orgID, []string{WebhookEventEmployeeCreated}, true)
	wildcard := mustPgWebhookSub(t, s, orgID, []string{"*"}, true)
	mustPgWebhookSub(t, s, orgID, []string{"*"}, false) // inactive

	n, err := s.EnqueueWebhookEvent(ctx, orgID, WebhookEventEmployeeCreated, map[string]any{"id": "e1"})
	if err != nil || n != 2 {
		t.Fatalf("enqueue: n=%d err=%v, want 2", n, err)
	}
	// Duplicate subscription IDs are impossible; unknown event is rejected.
	if _, err := s.EnqueueWebhookEvent(ctx, orgID, "bogus.event", nil); err == nil {
		t.Fatalf("unknown event: want error, got nil")
	}

	claimed, err := s.ClaimWebhookDeliveries(ctx, 20)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim: %d %v", len(claimed), err)
	}
	seen := map[string]bool{}
	for _, m := range claimed {
		seen[m.SubscriptionID] = true
		if m.EventType != WebhookEventEmployeeCreated || m.Status != WebhookStatusPending {
			t.Fatalf("bad message: %+v", m)
		}
	}
	if !seen[matching.ID] || !seen[wildcard.ID] {
		t.Fatalf("fan-out wrong: %+v", seen)
	}
	// Leased: nothing left to claim.
	if again, err := s.ClaimWebhookDeliveries(ctx, 20); err != nil || len(again) != 0 {
		t.Fatalf("second claim: %d %v, want 0", len(again), err)
	}

	// One delivered, one failed → backoff → dead.
	if err := s.MarkWebhookDelivered(ctx, claimed[0].ID); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	failID := claimed[1].ID
	for i := 0; i < 4; i++ {
		if err := s.MarkWebhookFailed(ctx, failID, "pg boom"); err != nil {
			t.Fatalf("fail %d: %v", i+1, err)
		}
		var status string
		var attempts int
		var nextAt time.Time
		if err := s.pool.QueryRow(ctx,
			`SELECT status, attempts, next_attempt_at FROM webhook_outbox WHERE id = $1::uuid`,
			failID).Scan(&status, &attempts, &nextAt); err != nil {
			t.Fatalf("read: %v", err)
		}
		if status != WebhookStatusPending || attempts != i+1 {
			t.Fatalf("after fail %d: status=%q attempts=%d", i+1, status, attempts)
		}
		pgRequeueForTest(t, s, failID)
		if again, err := s.ClaimWebhookDeliveries(ctx, 20); err != nil || len(again) != 1 {
			t.Fatalf("re-claim after fail %d: %d %v", i+1, len(again), err)
		}
	}
	if err := s.MarkWebhookFailed(ctx, failID, "pg boom"); err != nil {
		t.Fatalf("fail 5: %v", err)
	}
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT status FROM webhook_outbox WHERE id = $1::uuid`, failID).Scan(&status); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != WebhookStatusDead {
		t.Fatalf("after 5th failure: status=%q, want dead", status)
	}
	if err := s.MarkWebhookFailed(ctx, "00000000-0000-0000-0000-000000000000", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fail missing: want ErrNotFound, got %v", err)
	}
}

func TestPostgresWebhookTestEndpoint(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	orgID := mustPgWebhookOrg(t, s)
	sub := mustPgWebhookSub(t, s, orgID, []string{WebhookEventEmployeeCreated}, true)
	mustPgWebhookSub(t, s, orgID, []string{"*"}, true) // must NOT receive it

	id, err := s.EnqueueWebhookEventForSubscription(ctx, orgID, sub.ID,
		WebhookEventWebhookTest, map[string]any{"note": "pg"})
	if err != nil || id == "" {
		t.Fatalf("enqueue for sub: %q %v", id, err)
	}
	claimed, err := s.ClaimWebhookDeliveries(ctx, 20)
	if err != nil || len(claimed) != 1 || claimed[0].ID != id || claimed[0].SubscriptionID != sub.ID {
		t.Fatalf("claimed: %+v %v", claimed, err)
	}
}
