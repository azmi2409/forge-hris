// Webhook endpoints and the outbox dispatcher (component 4, agent surface).
//
// Subscriptions are admin-managed per organization. Deliveries go through a
// durable outbox: handlers enqueue events (best-effort, never failing the
// request) and the in-process dispatcher (StartWebhookDispatcher) POSTs them
// with an HMAC-SHA256 signature. Delivery is at-least-once — consumers must
// dedupe on the envelope's "id".
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/azmi2409/forge-hris/store"
)

// webhookRoutes registers the webhook management endpoints. It is called
// from Server.routes (see server.go); admin-only throughout.
func (s *Server) webhookRoutes() {
	m := s.mux
	m.HandleFunc("POST /api/v1/organizations/{orgID}/webhooks",
		s.withAudit("webhook_subscription.created", "webhook_subscription", s.createWebhookSubscription))
	m.HandleFunc("GET /api/v1/organizations/{orgID}/webhooks", s.handleListWebhookSubscriptions)
	m.HandleFunc("GET /api/v1/organizations/{orgID}/webhooks/{webhookID}", s.handleGetWebhookSubscription)
	m.HandleFunc("PATCH /api/v1/organizations/{orgID}/webhooks/{webhookID}",
		s.withAudit("webhook_subscription.updated", "webhook_subscription", s.patchWebhookSubscription))
	m.HandleFunc("DELETE /api/v1/organizations/{orgID}/webhooks/{webhookID}",
		s.withAudit("webhook_subscription.deleted", "webhook_subscription", s.deleteWebhookSubscription))
	m.HandleFunc("POST /api/v1/organizations/{orgID}/webhooks/{webhookID}/test",
		s.withAudit("webhook_subscription.tested", "webhook_subscription", s.testWebhookSubscription))
}

// webhookSubscriptionResponse is the public shape of a subscription. The
// secret is shown exactly once: on create (and echoed on PATCH when the
// caller rotates it). List/get responses omit it.
type webhookSubscriptionResponse struct {
	ID             string   `json:"id"`
	OrganizationID string   `json:"organization_id"`
	URL            string   `json:"url"`
	Secret         string   `json:"secret,omitempty"`
	Events         []string `json:"events"`
	Active         bool     `json:"active"`
	CreatedAt      string   `json:"created_at"`
}

func toWebhookSubscriptionResponse(sub store.WebhookSubscription, revealSecret bool) webhookSubscriptionResponse {
	secret := ""
	if revealSecret {
		secret = sub.Secret
	}
	events := sub.Events
	if events == nil {
		events = []string{}
	}
	return webhookSubscriptionResponse{
		ID:             sub.ID,
		OrganizationID: sub.OrganizationID,
		URL:            sub.URL,
		Secret:         secret,
		Events:         events,
		Active:         sub.Active,
		CreatedAt:      sub.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
}

// newWebhookSecret generates the signing secret revealed once at creation.
func newWebhookSecret() (string, bool) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(b[:]), true
}

type createWebhookSubscriptionRequest struct {
	URL    string   `json:"url"`
	Secret *string  `json:"secret,omitempty"`
	Events []string `json:"events"`
	Active *bool    `json:"active,omitempty"`
}

func (s *Server) createWebhookSubscription(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to manage webhook subscriptions") {
		return zero, false
	}
	var req createWebhookSubscriptionRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	secret := ""
	if req.Secret != nil {
		secret = strings.TrimSpace(*req.Secret)
	}
	if secret == "" {
		var ok bool
		secret, ok = newWebhookSecret()
		if !ok {
			writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
			return zero, false
		}
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	created, err := s.store.CreateWebhookSubscription(r.Context(), store.WebhookSubscription{
		OrganizationID: orgID,
		URL:            req.URL,
		Secret:         secret,
		Events:         req.Events,
		Active:         active,
	})
	if !mapError(w, r, err) {
		return zero, false
	}
	// The secret is shown exactly once; it cannot be retrieved again.
	writeJSON(w, http.StatusCreated, toWebhookSubscriptionResponse(created, true))
	return auditData{ResourceID: created.ID, After: toWebhookSubscriptionResponse(created, false)}, true
}

func (s *Server) handleListWebhookSubscriptions(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to manage webhook subscriptions") {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	subs, next, err := s.store.ListWebhookSubscriptions(r.Context(), orgID, page)
	if !mapError(w, r, err) {
		return
	}
	out := make([]webhookSubscriptionResponse, 0, len(subs))
	for _, sub := range subs {
		out = append(out, toWebhookSubscriptionResponse(sub, false))
	}
	writePage(w, out, next)
}

func (s *Server) handleGetWebhookSubscription(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to manage webhook subscriptions") {
		return
	}
	sub, err := s.store.GetWebhookSubscription(r.Context(), orgID, r.PathValue("webhookID"))
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, toWebhookSubscriptionResponse(sub, false))
}

type patchWebhookSubscriptionRequest struct {
	URL    *string  `json:"url,omitempty"`
	Secret *string  `json:"secret,omitempty"`
	Events []string `json:"events,omitempty"`
	Active *bool    `json:"active,omitempty"`
}

func (s *Server) patchWebhookSubscription(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to manage webhook subscriptions") {
		return zero, false
	}
	var req patchWebhookSubscriptionRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	cur, err := s.store.GetWebhookSubscription(r.Context(), orgID, r.PathValue("webhookID"))
	if !mapError(w, r, err) {
		return zero, false
	}
	before := toWebhookSubscriptionResponse(cur, false)
	rotated := false
	if req.URL != nil {
		cur.URL = *req.URL
	}
	if req.Secret != nil && strings.TrimSpace(*req.Secret) != "" {
		cur.Secret = strings.TrimSpace(*req.Secret)
		rotated = true
	}
	if req.Events != nil {
		cur.Events = req.Events
	}
	if req.Active != nil {
		cur.Active = *req.Active
	}
	updated, err := s.store.UpdateWebhookSubscription(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, toWebhookSubscriptionResponse(updated, rotated))
	return auditData{ResourceID: updated.ID, Before: before, After: toWebhookSubscriptionResponse(updated, false)}, true
}

func (s *Server) deleteWebhookSubscription(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to manage webhook subscriptions") {
		return zero, false
	}
	webhookID := r.PathValue("webhookID")
	before, err := s.store.GetWebhookSubscription(r.Context(), orgID, webhookID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if err := s.store.DeleteWebhookSubscription(r.Context(), orgID, webhookID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: webhookID, Before: toWebhookSubscriptionResponse(before, false)}, true
}

// testWebhookSubscription is the Jibble-style TestWebhook: it enqueues a
// webhook.test event for exactly this subscription through the normal
// outbox→dispatcher pipeline and returns the outbox message id so the caller
// can correlate it with the delivery.
func (s *Server) testWebhookSubscription(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to manage webhook subscriptions") {
		return zero, false
	}
	webhookID := r.PathValue("webhookID")
	sub, err := s.store.GetWebhookSubscription(r.Context(), orgID, webhookID)
	if !mapError(w, r, err) {
		return zero, false
	}
	messageID, err := s.store.EnqueueWebhookEventForSubscription(r.Context(), orgID, webhookID,
		store.WebhookEventWebhookTest, map[string]any{
			"subscription_id": webhookID,
			"url":             sub.URL,
			"note":            "test delivery from forge-hris",
		})
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message_id": messageID})
	return auditData{ResourceID: webhookID, After: map[string]any{"message_id": messageID}}, true
}

// emitWebhook enqueues one webhook event, fanning out to the org's matching
// subscriptions. It is best-effort by design: enqueue failures are logged
// and never fail the request that produced the event.
func (s *Server) emitWebhook(ctx context.Context, orgID, eventType string, data any) {
	n, err := s.store.EnqueueWebhookEvent(ctx, orgID, eventType, data)
	if err != nil {
		s.logger.Error("webhook enqueue failed",
			"organization_id", orgID,
			"event", eventType,
			"error", err,
		)
		return
	}
	if n > 0 {
		s.logger.Debug("webhook event enqueued",
			"organization_id", orgID,
			"event", eventType,
			"deliveries", n,
		)
	}
}
