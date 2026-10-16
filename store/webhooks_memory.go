package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// webhookClaimLease is how long a claimed delivery stays invisible to other
// dispatchers. It only matters when a dispatcher dies mid-delivery: the
// message becomes claimable again after the lease instead of being pinned.
// Dispatchers are in-process and single per instance, so this is a safety
// net, not the primary exclusion mechanism.
const webhookClaimLease = 60 * time.Second

func copyWebhookSubscription(s WebhookSubscription) WebhookSubscription {
	s.Events = copyStrings(s.Events)
	return s
}

func copyWebhookOutbox(m WebhookOutbox) WebhookOutbox {
	m.Payload = copyBytes(m.Payload)
	return m
}

// ---------------------------------------------------------------------------
// Webhook subscriptions
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateWebhookSubscription(ctx context.Context, sub WebhookSubscription) (WebhookSubscription, error) {
	if err := ctx.Err(); err != nil {
		return WebhookSubscription{}, err
	}
	if err := sub.Validate(); err != nil {
		return WebhookSubscription{}, err
	}
	if sub.ID == "" {
		sub.ID = newID()
	}
	sub.CreatedAt = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[sub.OrganizationID]; !ok {
		return WebhookSubscription{}, fmt.Errorf("%w: organization %s", ErrNotFound, sub.OrganizationID)
	}
	if _, ok := s.webhookSubs[gkey(sub.OrganizationID, sub.ID)]; ok {
		return WebhookSubscription{}, fmt.Errorf("%w: webhook subscription %s", ErrAlreadyExists, sub.ID)
	}
	s.webhookSubs[gkey(sub.OrganizationID, sub.ID)] = sub
	return copyWebhookSubscription(sub), nil
}

func (s *MemoryStore) GetWebhookSubscription(ctx context.Context, orgID, id string) (WebhookSubscription, error) {
	if err := ctx.Err(); err != nil {
		return WebhookSubscription{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.webhookSubs[gkey(orgID, id)]
	if !ok {
		return WebhookSubscription{}, fmt.Errorf("%w: webhook subscription %s", ErrNotFound, id)
	}
	return copyWebhookSubscription(sub), nil
}

func (s *MemoryStore) ListWebhookSubscriptions(ctx context.Context, orgID string, p Page) ([]WebhookSubscription, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []WebhookSubscription
	for _, sub := range s.webhookSubs {
		if sub.OrganizationID == orgID {
			out = append(out, copyWebhookSubscription(sub))
		}
	}
	sortByCursor(out, func(s WebhookSubscription) cursorKey { return cursorKey{s.CreatedAt, s.ID} })
	return paginate(out, func(s WebhookSubscription) cursorKey { return cursorKey{s.CreatedAt, s.ID} }, p)
}

func (s *MemoryStore) UpdateWebhookSubscription(ctx context.Context, sub WebhookSubscription) (WebhookSubscription, error) {
	if err := ctx.Err(); err != nil {
		return WebhookSubscription{}, err
	}
	if err := sub.Validate(); err != nil {
		return WebhookSubscription{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(sub.OrganizationID, sub.ID)
	cur, ok := s.webhookSubs[k]
	if !ok {
		return WebhookSubscription{}, fmt.Errorf("%w: webhook subscription %s", ErrNotFound, sub.ID)
	}
	cur.URL = sub.URL
	cur.Secret = sub.Secret
	cur.Events = copyStrings(sub.Events)
	cur.Active = sub.Active
	s.webhookSubs[k] = cur
	return copyWebhookSubscription(cur), nil
}

func (s *MemoryStore) DeleteWebhookSubscription(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(orgID, id)
	if _, ok := s.webhookSubs[k]; !ok {
		return fmt.Errorf("%w: webhook subscription %s", ErrNotFound, id)
	}
	delete(s.webhookSubs, k)
	// Pending outbox rows die with their subscription (mirrors the Postgres
	// ON DELETE CASCADE); sent/dead history is kept.
	for mid, m := range s.webhookOutbox {
		if m.SubscriptionID == id && m.Status == WebhookStatusPending {
			delete(s.webhookOutbox, mid)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Webhook outbox
// ---------------------------------------------------------------------------

// enqueueWebhookEventLocked fans one event out to matching subscriptions and
// returns the inserted messages in fan-out order. The caller must hold s.mu.
func (s *MemoryStore) enqueueWebhookEventLocked(orgID, eventType string, payload json.RawMessage, onlySubscriptionID string) ([]WebhookOutbox, error) {
	if !ValidWebhookEventType(eventType) {
		return nil, &ValidationError{Field: "event_type", Message: fmt.Sprintf("unknown event type %q", eventType)}
	}
	var subs []WebhookSubscription
	for _, sub := range s.webhookSubs {
		if sub.OrganizationID != orgID || !sub.Active {
			continue
		}
		if onlySubscriptionID != "" {
			if sub.ID != onlySubscriptionID {
				continue
			}
		} else if !webhookMatches(sub.Events, eventType) {
			continue
		}
		subs = append(subs, sub)
	}
	// Deterministic fan-out order (created_at, id) so retries and tests see
	// a stable sequence.
	sort.Slice(subs, func(i, j int) bool {
		if !subs[i].CreatedAt.Equal(subs[j].CreatedAt) {
			return subs[i].CreatedAt.Before(subs[j].CreatedAt)
		}
		return subs[i].ID < subs[j].ID
	})
	now := time.Now().UTC()
	out := make([]WebhookOutbox, 0, len(subs))
	for _, sub := range subs {
		m := WebhookOutbox{
			ID:             newID(),
			OrganizationID: orgID,
			SubscriptionID: sub.ID,
			EventType:      eventType,
			Payload:        copyBytes(payload),
			Status:         WebhookStatusPending,
			NextAttemptAt:  now,
			CreatedAt:      now,
		}
		s.webhookOutbox[m.ID] = m
		out = append(out, copyWebhookOutbox(m))
	}
	return out, nil
}

func (s *MemoryStore) EnqueueWebhookEvent(ctx context.Context, orgID, eventType string, data any) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("store: marshal webhook payload: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs, err := s.enqueueWebhookEventLocked(orgID, eventType, payload, "")
	if err != nil {
		return 0, err
	}
	return len(msgs), nil
}

func (s *MemoryStore) EnqueueWebhookEventForSubscription(ctx context.Context, orgID, subscriptionID, eventType string, data any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("store: marshal webhook payload: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.webhookSubs[gkey(orgID, subscriptionID)]
	if !ok {
		return "", fmt.Errorf("%w: webhook subscription %s", ErrNotFound, subscriptionID)
	}
	if !sub.Active {
		return "", fmt.Errorf("store: webhook subscription %s is inactive", subscriptionID)
	}
	msgs, err := s.enqueueWebhookEventLocked(orgID, eventType, payload, subscriptionID)
	if err != nil {
		return "", err
	}
	if len(msgs) != 1 {
		return "", fmt.Errorf("store: enqueue for subscription %s wrote %d rows", subscriptionID, len(msgs))
	}
	return msgs[0].ID, nil
}

func (s *MemoryStore) ClaimWebhookDeliveries(ctx context.Context, limit int) ([]WebhookOutbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	var due []WebhookOutbox
	for _, m := range s.webhookOutbox {
		if m.Status != WebhookStatusPending || m.NextAttemptAt.After(now) {
			continue
		}
		sub, ok := s.webhookSubs[gkey(m.OrganizationID, m.SubscriptionID)]
		if !ok || !sub.Active {
			continue // orphaned or paused: leave for delete/reactivation
		}
		due = append(due, m)
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].NextAttemptAt.Equal(due[j].NextAttemptAt) {
			return due[i].NextAttemptAt.Before(due[j].NextAttemptAt)
		}
		return due[i].ID < due[j].ID
	})
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]WebhookOutbox, 0, len(due))
	for _, m := range due {
		// Lease the row so a crashed dispatcher doesn't pin it and a
		// concurrent dispatcher doesn't re-send it.
		m.NextAttemptAt = now.Add(webhookClaimLease)
		s.webhookOutbox[m.ID] = m
		out = append(out, copyWebhookOutbox(m))
	}
	return out, nil
}

func (s *MemoryStore) MarkWebhookDelivered(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.webhookOutbox[id]
	if !ok {
		return fmt.Errorf("%w: webhook message %s", ErrNotFound, id)
	}
	m.Status = WebhookStatusSent
	s.webhookOutbox[id] = m
	return nil
}

func (s *MemoryStore) MarkWebhookFailed(ctx context.Context, id, errMsg string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.webhookOutbox[id]
	if !ok {
		return fmt.Errorf("%w: webhook message %s", ErrNotFound, id)
	}
	now := time.Now().UTC()
	attempts, dead, nextAt := webhookNextAttempt(m.Attempts, now)
	m.Attempts = attempts
	m.LastError = truncateErr(errMsg)
	if dead {
		m.Status = WebhookStatusDead
	} else {
		m.Status = WebhookStatusPending
		m.NextAttemptAt = nextAt
	}
	s.webhookOutbox[id] = m
	return nil
}

// truncateErr keeps last_error bounded; dispatcher error bodies are already
// size-capped at the HTTP layer, this is defense in depth.
func truncateErr(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max]
	}
	return s
}
