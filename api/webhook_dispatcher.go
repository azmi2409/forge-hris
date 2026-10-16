// The webhook outbox dispatcher (component 4, agent surface).
//
// Delivery model: an in-process goroutine polls the outbox every 5 seconds,
// claims up to 20 due deliveries (atomic, SKIP LOCKED), and POSTs each as a
// JSON envelope signed with HMAC-SHA256:
//
//	{"id":"<message id>","type":"<event type>","time":"<RFC3339Nano, event time>","data":{...}}
//	X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(secret, body))>
//
// 2xx marks the message sent; anything else records a failure and schedules
// a retry on the store's backoff cadence (30s → 5m → 30m → 2h, then dead
// after 5 attempts). Because a crash between a successful POST and the sent
// mark replays the message, delivery is at-least-once: consumers MUST dedupe
// on the envelope's "id".
package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// webhookPollInterval is how often the dispatcher drains the outbox.
// webhookClaimBatch is the per-pass claim cap. webhookDeliveryTimeout bounds
// one endpoint POST.
const (
	webhookPollInterval    = 5 * time.Second
	webhookClaimBatch      = 20
	webhookDeliveryTimeout = 10 * time.Second
)

// StartWebhookDispatcher starts the in-process outbox dispatcher. It runs
// until ctx is cancelled; an initial pass runs immediately so a fresh boot
// doesn't wait a full interval. Safe to call once per process.
func StartWebhookDispatcher(ctx context.Context, st store.Store, logger *slog.Logger) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	go func() {
		dispatchWebhookPass(ctx, st, logger)
		ticker := time.NewTicker(webhookPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				dispatchWebhookPass(ctx, st, logger)
			}
		}
	}()
}

// dispatchWebhookPass claims one batch of due deliveries and delivers each.
// It is a separate function (rather than inlined in the goroutine) so tests
// can drive exactly one pass against an httptest endpoint.
func dispatchWebhookPass(ctx context.Context, st store.Store, logger *slog.Logger) {
	msgs, err := st.ClaimWebhookDeliveries(ctx, webhookClaimBatch)
	if err != nil {
		logger.Error("webhook claim failed", "error", err)
		return
	}
	for i := range msgs {
		deliverWebhookMessage(ctx, st, logger, msgs[i])
	}
}

// webhookEnvelope is the POST body: envelope metadata plus the event payload
// as "data". "time" is the event's enqueue time, stable across retries.
type webhookEnvelope struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Time string          `json:"time"`
	Data json.RawMessage `json:"data"`
}

// deliverWebhookMessage POSTs one claimed delivery and marks it sent or
// failed. A missing subscription can't happen through the API (FK cascade),
// but a defensive failed-mark keeps the row from wedging the outbox.
func deliverWebhookMessage(ctx context.Context, st store.Store, logger *slog.Logger, m store.WebhookOutbox) {
	sub, err := st.GetWebhookSubscription(ctx, m.OrganizationID, m.SubscriptionID)
	if err != nil {
		markFailed(ctx, st, logger, m, "subscription lookup: "+err.Error())
		return
	}
	env := webhookEnvelope{
		ID:   m.ID,
		Type: m.EventType,
		Time: m.CreatedAt.UTC().Format(time.RFC3339Nano),
		Data: json.RawMessage(m.Payload),
	}
	body, err := json.Marshal(env)
	if err != nil {
		markFailed(ctx, st, logger, m, "marshal envelope: "+err.Error())
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(body))
	if err != nil {
		markFailed(ctx, st, logger, m, "build request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(webhookHMAC(sub.Secret, body)))
	req.Header.Set("X-Webhook-ID", m.ID)
	req.Header.Set("X-Webhook-Event", m.EventType)
	req.Header.Set("User-Agent", "forge-hris-webhooks/1.0")

	client := &http.Client{Timeout: webhookDeliveryTimeout}
	resp, err := client.Do(req)
	if err != nil {
		markFailed(ctx, st, logger, m, "POST: "+err.Error())
		return
	}
	defer resp.Body.Close()
	// Drain a bounded error snippet for last_error; the store truncates too.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		markFailed(ctx, st, logger, m,
			"endpoint returned "+resp.Status+": "+string(snippet))
		return
	}
	if err := st.MarkWebhookDelivered(ctx, m.ID); err != nil {
		logger.Error("webhook mark delivered failed",
			"message_id", m.ID, "subscription_id", m.SubscriptionID, "error", err)
		return
	}
	logger.Debug("webhook delivered",
		"message_id", m.ID, "event", m.EventType, "url", sub.URL)
}

// markFailed records the failure; when the message goes dead the store
// marks it and the dispatcher logs it loudly — a dead webhook is an
// operator-actionable signal (rotated secret, dead endpoint).
func markFailed(ctx context.Context, st store.Store, logger *slog.Logger, m store.WebhookOutbox, errMsg string) {
	if err := st.MarkWebhookFailed(ctx, m.ID, errMsg); err != nil {
		logger.Error("webhook mark failed failed",
			"message_id", m.ID, "error", err)
		return
	}
	if m.Attempts+1 >= store.MaxWebhookAttempts {
		logger.Error("webhook delivery dead",
			"message_id", m.ID,
			"subscription_id", m.SubscriptionID,
			"event", m.EventType,
			"attempts", m.Attempts+1,
			"last_error", errMsg,
		)
	}
}

// webhookHMAC returns HMAC-SHA256(secret, body).
func webhookHMAC(secret string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return mac.Sum(nil)
}
