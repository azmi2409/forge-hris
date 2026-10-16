// Package teams — Microsoft Teams inbound webhook clock bot.
//
// Inbound flow (POST /api/v1/integrations/teams/webhook?key=<shared-secret>):
//
//	shared-secret check (constant-time, against every stored Teams config)
//	-> map Teams user_id to a forge-hris employee ->
//	"clock in" / "clock out" / "break" -> ClockIn / ClockOut / StartBreak / EndBreak.
//
// The body is JSON: {"text": "clock in", "user_id": "..."}. Commands match
// case-insensitively by prefix, so "Clock in please" works. "break" toggles:
// on a break it ends it, otherwise it starts one. Responses are
// {"text": "..."} JSON.
//
// Security notes:
//   - The ?key= parameter is compared in constant time against each
//     config's decrypted shared secret. Secrets live in the store only as
//     AES-GCM envelopes (see integrations/secrets).
//   - There is no forge-hris bearer auth on this route; the shared secret
//     is the auth. Rotate it from the admin endpoint if it ever leaks.
package teams

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/integrations/secrets"
	"github.com/azmi2409/forge-hris/store"
)

// maxBodyBytes caps inbound Teams payloads.
const maxBodyBytes = 1 << 20 // 1 MiB

// Store is the narrow persistence contract the Teams handler needs.
type Store interface {
	ListTeamsConfigs(ctx context.Context, p store.Page) ([]store.TeamsConfig, string, error)
	GetTeamsUserEmployee(ctx context.Context, orgID, teamsUserID string) (string, error)
	ClockIn(ctx context.Context, orgID, employeeID string, e store.TimeEntry) (store.TimeEntry, error)
	ClockOut(ctx context.Context, orgID, employeeID string, e store.TimeEntry) (store.TimeEntry, error)
	StartBreak(ctx context.Context, orgID, employeeID string, e store.TimeEntry) (store.TimeEntry, error)
	EndBreak(ctx context.Context, orgID, employeeID string, e store.TimeEntry) (store.TimeEntry, error)
	OpenTimeEntry(ctx context.Context, orgID, employeeID string) (store.TimeEntry, bool, error)
}

// Config configures the handler.
type Config struct {
	Store Store
	// Now is the clock (defaults to time.Now); injectable for tests.
	Now func() time.Time
}

// Handler serves the Teams inbound webhook.
type Handler struct {
	store Store
	now   func() time.Time
}

// NewHandler builds a Teams webhook handler.
func NewHandler(st Store, cfg Config) *Handler {
	if cfg.Store != nil {
		st = cfg.Store
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Handler{store: st, now: now}
}

// webhookBody is the JSON the Teams side sends. Extra fields are ignored.
type webhookBody struct {
	Text   string `json:"text"`
	UserID string `json:"user_id"`
}

// HandleWebhook serves POST /api/v1/integrations/teams/webhook?key=....
func (h *Handler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTeamsError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		writeTeamsError(w, http.StatusUnauthorized, "missing key")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeTeamsError(w, http.StatusBadRequest, "unreadable body")
		return
	}
	var wb webhookBody
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&wb); err != nil {
		writeTeamsError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(wb.UserID) == "" {
		writeTeamsError(w, http.StatusBadRequest, "user_id required")
		return
	}

	// Find the org whose shared secret matches, in constant time per
	// config. The configs list is small (one per org).
	cfg, ok := h.matchConfig(r.Context(), key)
	if !ok {
		writeTeamsError(w, http.StatusUnauthorized, "bad key")
		return
	}

	empID, err := h.store.GetTeamsUserEmployee(r.Context(), cfg.OrganizationID, wb.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeText(w, "Your Teams account isn't linked to a forge-hris employee yet. Ask an admin to link it under Integrations.")
			return
		}
		writeTeamsError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.handleCommand(w, r, cfg.OrganizationID, empID, strings.TrimSpace(wb.Text))
}

// matchConfig returns the first Teams config whose decrypted shared secret
// equals key under constant-time comparison.
func (h *Handler) matchConfig(ctx context.Context, key string) (store.TeamsConfig, bool) {
	configs, _, err := h.store.ListTeamsConfigs(ctx, store.Page{})
	if err != nil {
		return store.TeamsConfig{}, false
	}
	for _, c := range configs {
		plain, err := secrets.Decrypt(c.EncSharedSecret)
		if err != nil {
			continue // misconfigured row; skip rather than fail open
		}
		if subtle.ConstantTimeCompare([]byte(plain), []byte(key)) == 1 {
			return c, true
		}
	}
	return store.TeamsConfig{}, false
}

func (h *Handler) handleCommand(w http.ResponseWriter, r *http.Request, orgID, empID, text string) {
	ctx := r.Context()
	now := h.now().UTC()
	lower := strings.ToLower(text)
	switch {
	case strings.HasPrefix(lower, "clock in"):
		e, err := h.store.ClockIn(ctx, orgID, empID, store.TimeEntry{ClientType: store.TimeEntryClientTeams, OccurredAt: now})
		if err != nil {
			writeText(w, clockErrText("clock in", err))
			return
		}
		writeText(w, fmt.Sprintf("✅ Clocked in at %s. Have a good shift!", e.OccurredAt.Format("15:04")))
	case strings.HasPrefix(lower, "clock out"):
		e, err := h.store.ClockOut(ctx, orgID, empID, store.TimeEntry{ClientType: store.TimeEntryClientTeams, OccurredAt: now})
		if err != nil {
			writeText(w, clockErrText("clock out", err))
			return
		}
		writeText(w, fmt.Sprintf("👋 Clocked out at %s. Shift complete.", e.OccurredAt.Format("15:04")))
	case strings.HasPrefix(lower, "break"):
		// Toggle: on break -> end it, otherwise start it.
		if open, isOpen, err := h.store.OpenTimeEntry(ctx, orgID, empID); err == nil && isOpen && open.Kind == store.TimeEntryStartBreak {
			e, err := h.store.EndBreak(ctx, orgID, empID, store.TimeEntry{ClientType: store.TimeEntryClientTeams, OccurredAt: now})
			if err != nil {
				writeText(w, clockErrText("end break", err))
				return
			}
			writeText(w, fmt.Sprintf("▶️ Break ended at %s — back on the clock.", e.OccurredAt.Format("15:04")))
			return
		}
		e, err := h.store.StartBreak(ctx, orgID, empID, store.TimeEntry{ClientType: store.TimeEntryClientTeams, OccurredAt: now})
		if err != nil {
			writeText(w, clockErrText("start break", err))
			return
		}
		writeText(w, fmt.Sprintf("☕ Break started at %s.", e.OccurredAt.Format("15:04")))
	default:
		writeText(w, "I understand: \"clock in\", \"clock out\", \"break\".")
	}
}

func clockErrText(action string, err error) string {
	switch {
	case errors.Is(err, store.ErrAlreadyClockedIn):
		return "You're already clocked in. Send \"clock out\" first."
	case errors.Is(err, store.ErrInvalidTransition):
		return fmt.Sprintf("Can't %s right now: %s", action, err.Error())
	case errors.Is(err, store.ErrPeriodLocked):
		return fmt.Sprintf("Can't %s: the pay period is locked.", action)
	case errors.Is(err, store.ErrNotFound):
		return fmt.Sprintf("Can't %s: you're not clocked in.", action)
	default:
		return fmt.Sprintf("Couldn't %s: %s", action, err.Error())
	}
}

func writeText(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
}

func writeTeamsError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
