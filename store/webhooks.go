package store

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Webhook event taxonomy (component 4, agent surface). Exact strings are the
// contract with consumers: handlers in the api package emit these via
// Server.emitWebhook, and subscriptions match on them (or on the wildcard).
const (
	// WebhookEventEmployeeCreated is emitted by POST /employees.
	WebhookEventEmployeeCreated = "employee.created"
	// WebhookEventEmployeeUpdated is emitted by PATCH /employees/{id}.
	WebhookEventEmployeeUpdated = "employee.updated"
	// WebhookEventEmployeeDeactivated is emitted by POST
	// /employees/{id}/deactivate.
	WebhookEventEmployeeDeactivated = "employee.deactivated"
	// WebhookEventClockInCreated is emitted by POST /clock-in.
	WebhookEventClockInCreated = "clock_in.created"
	// WebhookEventTimesheetSubmitted is emitted by POST
	// /pay-periods/{id}/submit.
	WebhookEventTimesheetSubmitted = "timesheet.submitted"
	// WebhookEventTimesheetApproved is emitted by POST
	// /pay-periods/{id}/approve.
	WebhookEventTimesheetApproved = "timesheet.approved"
	// WebhookEventTimesheetReopened is emitted by POST
	// /pay-periods/{id}/reopen.
	WebhookEventTimesheetReopened = "timesheet.reopened"
	// WebhookEventLeaveRequested is emitted by POST /leave-requests.
	WebhookEventLeaveRequested = "leave.requested"
	// WebhookEventLeaveApproved is emitted when a leave request's status
	// becomes approved (single decide or bulk approve).
	WebhookEventLeaveApproved = "leave.approved"
	// WebhookEventLeaveRejected is emitted when a leave request's status
	// becomes rejected.
	WebhookEventLeaveRejected = "leave.rejected"
	// WebhookEventLeaveBalanceChanged is emitted by POST
	// /leave-balances/recalculate when the balance actually changes.
	WebhookEventLeaveBalanceChanged = "leave.balance_changed"
	// WebhookEventAPITokenCreated is emitted by POST /credentials.
	WebhookEventAPITokenCreated = "api_token.created"
	// WebhookEventAPITokenRevoked is emitted by DELETE /credentials/{id}.
	WebhookEventAPITokenRevoked = "api_token.revoked"
	// WebhookEventWebhookTest is the synthetic event enqueued by POST
	// /webhooks/{id}/test (the Jibble-style TestWebhook).
	WebhookEventWebhookTest = "webhook.test"
	// WebhookEventWildcard matches every event type in a subscription.
	WebhookEventWildcard = "*"
)

// webhookEventTypes is the full set of subscribable event types.
var webhookEventTypes = map[string]bool{
	WebhookEventEmployeeCreated:     true,
	WebhookEventEmployeeUpdated:     true,
	WebhookEventEmployeeDeactivated: true,
	WebhookEventClockInCreated:      true,
	WebhookEventTimesheetSubmitted:  true,
	WebhookEventTimesheetApproved:   true,
	WebhookEventTimesheetReopened:   true,
	WebhookEventLeaveRequested:      true,
	WebhookEventLeaveApproved:       true,
	WebhookEventLeaveRejected:       true,
	WebhookEventLeaveBalanceChanged: true,
	WebhookEventAPITokenCreated:     true,
	WebhookEventAPITokenRevoked:     true,
	WebhookEventWebhookTest:         true,
	WebhookEventWildcard:            true,
}

// ValidWebhookEventType reports whether t is a known event type or "*".
func ValidWebhookEventType(t string) bool { return webhookEventTypes[t] }

// WebhookSubscription is one tenant's outbound webhook registration: a URL
// plus the event types it wants. The Secret signs every delivery with
// HMAC-SHA256 in the X-Hub-Signature-256 header (REA §5.4).
//
// Secrets are stored in plaintext (documented tradeoff: the dispatcher must
// use them at send time, and the threat model is a database breach, not a
// code/config leak). The future hardening path is KMS envelope encryption —
// wrap/unwrap in IssueCredential-style helpers and store the wrapped blob —
// but that needs an operator-managed key before it can land.
type WebhookSubscription struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	URL            string    `json:"url"`
	Secret         string    `json:"secret,omitempty"`
	Events         []string  `json:"events"`
	Active         bool      `json:"active"`
	CreatedAt      time.Time `json:"created_at"`
}

// Validate checks the subscription's URL, secret, and event list.
func (s WebhookSubscription) Validate() error {
	if strings.TrimSpace(s.URL) == "" {
		return &ValidationError{Field: "url", Message: "required"}
	}
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &ValidationError{Field: "url", Message: "must be an absolute http(s) URL"}
	}
	if strings.TrimSpace(s.Secret) == "" {
		return &ValidationError{Field: "secret", Message: "required"}
	}
	if len(s.Events) == 0 {
		return &ValidationError{Field: "events", Message: "at least one event type is required"}
	}
	for _, e := range s.Events {
		if !ValidWebhookEventType(e) {
			return &ValidationError{Field: "events", Message: fmt.Sprintf("unknown event type %q", e)}
		}
	}
	return nil
}

// webhookMatches reports whether the subscription wants eventType.
func webhookMatches(events []string, eventType string) bool {
	for _, e := range events {
		if e == eventType || e == WebhookEventWildcard {
			return true
		}
	}
	return false
}

// Webhook delivery states.
const (
	// WebhookStatusPending means the message is queued (or scheduled for
	// retry) and has not been delivered yet.
	WebhookStatusPending = "pending"
	// WebhookStatusSent means the endpoint answered 2xx.
	WebhookStatusSent = "sent"
	// WebhookStatusDead means delivery exhausted its attempts.
	WebhookStatusDead = "dead"
)

// WebhookOutbox is one queued delivery: an event fanned out to one
// subscription. The dispatcher claims pending rows, POSTs the envelope, and
// marks each row sent or failed (with backoff, then dead).
//
// Delivery is at-least-once: consumers MUST dedupe on ID, because a crash
// between a successful POST and MarkWebhookDelivered replays the message.
type WebhookOutbox struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	SubscriptionID string    `json:"subscription_id"`
	EventType      string    `json:"event_type"`
	Payload        []byte    `json:"payload"` // JSON; the envelope's "data"
	Status         string    `json:"status"`  // pending | sent | dead
	Attempts       int       `json:"attempts"`
	NextAttemptAt  time.Time `json:"next_attempt_at"`
	LastError      string    `json:"last_error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// webhookBackoff is the retry cadence after failed deliveries, indexed by
// the 1-based failure count (failure i < MaxWebhookAttempts retries after
// webhookBackoff[i-1]). The final 12h slot is unused at MaxWebhookAttempts=5
// (the 5th failure goes dead instead of scheduling) — it is retained so a
// future raise of MaxWebhookAttempts needs no schema or constant change.
var webhookBackoff = []time.Duration{
	30 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	12 * time.Hour,
}

// MaxWebhookAttempts caps delivery attempts before a message goes dead.
const MaxWebhookAttempts = 5

// maxWebhookAttempts is the internal alias kept for the backoff math.
const maxWebhookAttempts = MaxWebhookAttempts

// webhookNextAttempt computes the post-failure state: attempts+1, dead when
// attempts reaches MaxWebhookAttempts, otherwise the backoff slot.
func webhookNextAttempt(attempts int, now time.Time) (nextAttempts int, dead bool, nextAt time.Time) {
	nextAttempts = attempts + 1
	if nextAttempts >= maxWebhookAttempts {
		return nextAttempts, true, now
	}
	return nextAttempts, false, now.Add(webhookBackoff[nextAttempts-1])
}

// WebhookStore is the component 4 (agent surface: webhooks) contract:
// subscriptions plus a durable outbox the dispatcher drains.
//
// Implementations must be safe for concurrent use and must deep-copy values
// across the boundary, so a caller can never mutate stored state through a
// previously returned value.
type WebhookStore interface {
	// Webhook subscriptions.
	// CreateWebhookSubscription stores a subscription; an empty ID is
	// generated. URL must be http(s), Events non-empty and known (or "*").
	CreateWebhookSubscription(ctx context.Context, s WebhookSubscription) (WebhookSubscription, error)
	// GetWebhookSubscription returns the subscription or ErrNotFound.
	GetWebhookSubscription(ctx context.Context, orgID, id string) (WebhookSubscription, error)
	// ListWebhookSubscriptions returns the org's subscriptions ordered by
	// (created_at, id), paged.
	ListWebhookSubscriptions(ctx context.Context, orgID string, p Page) (subs []WebhookSubscription, next string, err error)
	// UpdateWebhookSubscription replaces URL/secret/events/active for the
	// stored subscription. ErrNotFound when unknown.
	UpdateWebhookSubscription(ctx context.Context, s WebhookSubscription) (WebhookSubscription, error)
	// DeleteWebhookSubscription removes the subscription; pending outbox
	// rows for it are dropped with it.
	DeleteWebhookSubscription(ctx context.Context, orgID, id string) error

	// Outbox.
	// EnqueueWebhookEvent fans one event out to every ACTIVE subscription
	// of the org whose Events include eventType or "*". The payload is the
	// marshaled data (it becomes the envelope's "data"). Returns the number
	// of outbox rows written. eventType must be a known type; unknown types
	// return a ValidationError.
	EnqueueWebhookEvent(ctx context.Context, orgID, eventType string, data any) (enqueued int, err error)
	// EnqueueWebhookEventForSubscription enqueues one event for a single
	// subscription (used by POST /webhooks/{id}/test). The subscription
	// must exist and be active; the event bypasses the subscription's
	// Events filter. Returns the outbox message ID.
	EnqueueWebhookEventForSubscription(ctx context.Context, orgID, subscriptionID, eventType string, data any) (messageID string, err error)
	// ClaimWebhookDeliveries atomically claims up to limit pending
	// deliveries whose next_attempt_at has passed AND whose subscription is
	// active, so concurrent dispatchers never double-send. Claimed rows get
	// a short lease on next_attempt_at so a crashed dispatcher releases
	// them instead of pinning them.
	ClaimWebhookDeliveries(ctx context.Context, limit int) ([]WebhookOutbox, error)
	// MarkWebhookDelivered marks the message sent. ErrNotFound when
	// unknown.
	MarkWebhookDelivered(ctx context.Context, id string) error
	// MarkWebhookFailed records a failed attempt: attempts+1, next attempt
	// on the backoff cadence, dead after maxWebhookAttempts.
	MarkWebhookFailed(ctx context.Context, id, errMsg string) error
}
