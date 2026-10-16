package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func scanWebhookSubscription(r rowScanner) (WebhookSubscription, error) {
	var s WebhookSubscription
	err := r.Scan(&s.ID, &s.OrganizationID, &s.URL, &s.Secret, &s.Events, &s.Active, &s.CreatedAt)
	return s, err
}

const webhookSubscriptionCols = `id::text, organization_id::text, url, secret, events, active, created_at`

func scanWebhookOutbox(r rowScanner) (WebhookOutbox, error) {
	var m WebhookOutbox
	err := r.Scan(
		&m.ID, &m.OrganizationID, &m.SubscriptionID, &m.EventType, &m.Payload,
		&m.Status, &m.Attempts, &m.NextAttemptAt, &m.LastError, &m.CreatedAt,
	)
	return m, err
}

const webhookOutboxCols = `id::text, organization_id::text, subscription_id::text, event_type,
	payload, status, attempts, next_attempt_at, last_error, created_at`

// ---------------------------------------------------------------------------
// Webhook subscriptions
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateWebhookSubscription(ctx context.Context, sub WebhookSubscription) (WebhookSubscription, error) {
	if err := ctx.Err(); err != nil {
		return WebhookSubscription{}, err
	}
	if err := sub.Validate(); err != nil {
		return WebhookSubscription{}, err
	}
	if sub.ID == "" {
		sub.ID = newID()
	}
	var out WebhookSubscription
	err := s.inTenant(ctx, sub.OrganizationID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1::uuid)`,
			sub.OrganizationID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: organization %s", ErrNotFound, sub.OrganizationID)
		}
		var err error
		out, err = scanWebhookSubscription(tx.QueryRow(ctx, `
			INSERT INTO webhook_subscriptions (id, organization_id, url, secret, events, active)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)
			RETURNING `+webhookSubscriptionCols,
			sub.ID, sub.OrganizationID, sub.URL, sub.Secret, sub.Events, sub.Active))
		if err != nil {
			return mapPgError(err, "webhook subscription")
		}
		return nil
	})
	if err != nil {
		return WebhookSubscription{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetWebhookSubscription(ctx context.Context, orgID, id string) (WebhookSubscription, error) {
	if err := ctx.Err(); err != nil {
		return WebhookSubscription{}, err
	}
	var out WebhookSubscription
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanWebhookSubscription(tx.QueryRow(ctx,
			`SELECT `+webhookSubscriptionCols+` FROM webhook_subscriptions WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: webhook subscription %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return WebhookSubscription{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListWebhookSubscriptions(ctx context.Context, orgID string, p Page) ([]WebhookSubscription, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(p.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if p.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []WebhookSubscription
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+webhookSubscriptionCols+` FROM webhook_subscriptions
			WHERE organization_id = $1::uuid
			  AND (NOT $2 OR (created_at, id) > ($3::timestamptz, NULLIF($4,'')::uuid))
			ORDER BY created_at, id
			LIMIT $5`, orgID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (WebhookSubscription, error) { return scanWebhookSubscription(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list webhook subscriptions: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func (s *PostgresStore) UpdateWebhookSubscription(ctx context.Context, sub WebhookSubscription) (WebhookSubscription, error) {
	if err := ctx.Err(); err != nil {
		return WebhookSubscription{}, err
	}
	if err := sub.Validate(); err != nil {
		return WebhookSubscription{}, err
	}
	var out WebhookSubscription
	err := s.inTenant(ctx, sub.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanWebhookSubscription(tx.QueryRow(ctx, `
			UPDATE webhook_subscriptions
			SET url = $3, secret = $4, events = $5, active = $6
			WHERE id = $1::uuid AND organization_id = $2::uuid
			RETURNING `+webhookSubscriptionCols,
			sub.ID, sub.OrganizationID, sub.URL, sub.Secret, sub.Events, sub.Active))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: webhook subscription %s", ErrNotFound, sub.ID)
		}
		return err
	})
	if err != nil {
		return WebhookSubscription{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteWebhookSubscription(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		// The FK drops pending outbox rows (ON DELETE CASCADE); sent/dead
		// history goes with the subscription.
		tag, err := tx.Exec(ctx,
			`DELETE FROM webhook_subscriptions WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: webhook subscription %s", ErrNotFound, id)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Webhook outbox
// ---------------------------------------------------------------------------

func (s *PostgresStore) EnqueueWebhookEvent(ctx context.Context, orgID, eventType string, data any) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !ValidWebhookEventType(eventType) {
		return 0, &ValidationError{Field: "event_type", Message: fmt.Sprintf("unknown event type %q", eventType)}
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("store: marshal webhook payload: %w", err)
	}
	var n int
	err = s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text FROM webhook_subscriptions
			WHERE organization_id = $1::uuid AND active
			  AND (events @> ARRAY[$2::text] OR events @> ARRAY['*'])
			ORDER BY created_at, id`, orgID, eventType)
		if err != nil {
			return err
		}
		var subIDs []string
		for rows.Next() {
			var sid string
			if err := rows.Scan(&sid); err != nil {
				rows.Close()
				return err
			}
			subIDs = append(subIDs, sid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, sid := range subIDs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO webhook_outbox (organization_id, subscription_id, event_type, payload)
				VALUES ($1::uuid, $2::uuid, $3, $4)`, orgID, sid, eventType, payload); err != nil {
				return err
			}
		}
		n = len(subIDs)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *PostgresStore) EnqueueWebhookEventForSubscription(ctx context.Context, orgID, subscriptionID, eventType string, data any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !ValidWebhookEventType(eventType) {
		return "", &ValidationError{Field: "event_type", Message: fmt.Sprintf("unknown event type %q", eventType)}
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("store: marshal webhook payload: %w", err)
	}
	var messageID string
	err = s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var active bool
		err := tx.QueryRow(ctx,
			`SELECT active FROM webhook_subscriptions WHERE id = $1::uuid AND organization_id = $2::uuid`,
			subscriptionID, orgID).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: webhook subscription %s", ErrNotFound, subscriptionID)
		}
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("store: webhook subscription %s is inactive", subscriptionID)
		}
		return tx.QueryRow(ctx, `
			INSERT INTO webhook_outbox (organization_id, subscription_id, event_type, payload)
			VALUES ($1::uuid, $2::uuid, $3, $4)
			RETURNING id::text`, orgID, subscriptionID, eventType, payload).Scan(&messageID)
	})
	if err != nil {
		return "", err
	}
	return messageID, nil
}

// The outbox operations below are cross-tenant: the dispatcher is a system
// component, not an org-scoped caller, so they run on the pool directly
// rather than inTenant. Like GetCredentialByTokenHash they rely on the app
// role owning the tables (plain RLS is bypassed for owners).

func (s *PostgresStore) ClaimWebhookDeliveries(ctx context.Context, limit int) ([]WebhookOutbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	// One atomic statement: lock the due rows (SKIP LOCKED so concurrent
	// dispatchers never double-send) and lease them for webhookClaimLease
	// so a crashed dispatcher releases its claims instead of pinning them.
	// Only rows whose subscription is still active are claimable; paused
	// subscriptions resume delivery on reactivation.
	rows, err := s.pool.Query(ctx, `
		UPDATE webhook_outbox o
		SET next_attempt_at = now() + make_interval(secs => $2)
		WHERE o.id IN (
			SELECT o2.id
			FROM webhook_outbox o2
			JOIN webhook_subscriptions s ON s.id = o2.subscription_id
			WHERE o2.status = 'pending'
			  AND o2.next_attempt_at <= now()
			  AND s.active
			ORDER BY o2.next_attempt_at, o2.id
			LIMIT $1
			FOR UPDATE OF o2 SKIP LOCKED
		)
		RETURNING `+webhookOutboxCols, limit, webhookClaimLease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: claim webhook deliveries: %w", err)
	}
	defer rows.Close()
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (WebhookOutbox, error) { return scanWebhookOutbox(r) })
	if err != nil {
		return nil, fmt.Errorf("store: claim webhook deliveries: %w", err)
	}
	if out == nil {
		out = []WebhookOutbox{}
	}
	return out, nil
}

func (s *PostgresStore) MarkWebhookDelivered(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE webhook_outbox SET status = 'sent' WHERE id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: webhook message %s", ErrNotFound, id)
	}
	return nil
}

func (s *PostgresStore) MarkWebhookFailed(ctx context.Context, id, errMsg string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	var attempts int
	err := s.pool.QueryRow(ctx,
		`SELECT attempts FROM webhook_outbox WHERE id = $1::uuid`, id).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: webhook message %s", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	nextAttempts, dead, nextAt := webhookNextAttempt(attempts, now)
	var status string
	if dead {
		status = WebhookStatusDead
	} else {
		status = WebhookStatusPending
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE webhook_outbox
		SET attempts = $2, status = $3, next_attempt_at = $4, last_error = $5
		WHERE id = $1::uuid`, id, nextAttempts, status, nextAt, truncateErr(errMsg))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: webhook message %s", ErrNotFound, id)
	}
	return nil
}
