// Package slack — Slack slash-command + interactive-component clock bot.
//
// Inbound flow (POST /api/v1/integrations/slack/events):
//
//	verify X-Slack-Signature -> look up workspace by team_id ->
//	decrypt + re-validate the stored service PAT (revocation disables the
//	integration) -> map Slack user_id to a forge-hris employee ->
//	ClockIn / ClockOut / StartBreak / EndBreak.
//
// The bot speaks the two Slack request shapes: slash commands
// (application/x-www-form-urlencoded) and interactive block_actions
// payloads (a JSON document in the "payload" form field). Responses are
// ephemeral Slack messages; clock-in/out answers carry break start/end
// buttons so the whole shift can run from the chat client.
//
// Security notes:
//   - VerifySignature implements Slack's v0 HMAC-SHA256 scheme with a
//     5-minute replay window. Timestamps are compared against the clock
//     (injectable for tests); stale or malformed signatures fail closed.
//   - Secrets live in the store only as AES-GCM envelopes (see
//     integrations/secrets). The signing secret is decrypted just long
//     enough to verify the request; the service token is decrypted to
//     re-validate it on every event, so revoking the PAT in forge-hris
//     immediately disables the workspace's bot.
package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/integrations/secrets"
	"github.com/azmi2409/forge-hris/store"
)

// signatureVersion is the Slack signature scheme prefix.
const signatureVersion = "v0"

// replayWindow is how old a Slack timestamp may be before it is rejected.
const replayWindow = 5 * time.Minute

// maxBodyBytes caps inbound Slack payloads.
const maxBodyBytes = 1 << 20 // 1 MiB

// VerifySignature verifies a Slack request signature per the signing-secret
// docs: signature == "v0=" + hex(HMAC-SHA256("v0:<timestamp>:<body>")),
// with the timestamp rejected when it is older than replayWindow (or in
// the future beyond it). Malformed timestamps or headers fail closed.
func VerifySignature(secret, timestamp, body string, signature string) bool {
	return verifySignatureAt(secret, timestamp, body, signature, time.Now())
}

func verifySignatureAt(secret, timestamp, body, signature string, now time.Time) bool {
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return false
	}
	if !strings.HasPrefix(signature, signatureVersion+"=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(signature, signatureVersion+"="))
	if err != nil || len(want) != sha256.Size {
		return false
	}
	// Replay protection: reject timestamps outside the window.
	if dt := now.Sub(time.Unix(ts, 0)); dt < 0 {
		dt = -dt
		if dt > replayWindow {
			return false
		}
	} else if dt > replayWindow {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%s:%s:%s", signatureVersion, timestamp, body)
	return hmac.Equal(mac.Sum(nil), want)
}

// Store is the narrow persistence contract the Slack handler needs. The
// concrete *store.MemoryStore / *store.PostgresStore satisfy it via the
// ChatStore and TimeTrackingStore contracts.
type Store interface {
	// ChatStore subset.
	GetSlackWorkspace(ctx context.Context, teamID string) (store.SlackWorkspace, error)
	GetSlackUserEmployee(ctx context.Context, teamID, slackUserID string) (string, error)
	GetCredentialByTokenHash(ctx context.Context, hash string) (store.Credential, error)
	// Identity read for friendly replies.
	GetEmployee(ctx context.Context, orgID, id string) (store.Employee, error)
	// TimeTrackingStore subset.
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

// Handler serves Slack event callbacks.
type Handler struct {
	store Store
	now   func() time.Time
}

// NewHandler builds a Slack event handler.
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

// HandleEvents serves POST /api/v1/integrations/slack/events: slash
// commands (/clockin, /clockout) and interactive block_actions (break
// start/end buttons). Authentication is the Slack signature; there is no
// forge-hris bearer auth on this route.
func (h *Handler) HandleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeSlackError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeSlackError(w, http.StatusBadRequest, "unreadable body")
		return
	}
	raw := string(body)
	form, err := url.ParseQuery(raw)
	if err != nil {
		writeSlackError(w, http.StatusBadRequest, "unparseable form")
		return
	}
	teamID := form.Get("team_id")
	userID := form.Get("user_id")

	// The signing secret is workspace-scoped: look the workspace up first
	// (by the team Slack sent), then verify against its secret.
	ws, err := h.store.GetSlackWorkspace(r.Context(), teamID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeEphemeral(w, "This Slack workspace isn't connected to forge-hris yet. Ask an admin to connect it under Integrations.")
			return
		}
		writeSlackError(w, http.StatusInternalServerError, "internal error")
		return
	}
	signingSecret, err := secrets.Decrypt(ws.EncSigningSecret)
	if err != nil {
		writeSlackError(w, http.StatusInternalServerError, "integration misconfigured")
		return
	}
	if !verifySignatureAt(string(signingSecret),
		r.Header.Get("X-Slack-Request-Timestamp"), raw,
		r.Header.Get("X-Slack-Signature"), h.now().UTC()) {
		writeSlackError(w, http.StatusUnauthorized, "bad signature")
		return
	}

	// Re-validate the stored service PAT on every event: revoking it in
	// forge-hris disables the workspace's bot immediately.
	empID, orgID, ok := h.authorize(r.Context(), w, ws, userID)
	if !ok {
		return
	}

	if payload := form.Get("payload"); payload != "" {
		h.handleInteractive(w, r, ws, orgID, empID, userID, teamID, payload)
		return
	}
	h.handleSlashCommand(w, r, orgID, empID, form.Get("command"), strings.TrimSpace(form.Get("text")))
}

// authorize decrypts the workspace's service token, re-validates it
// (revocation/expiry + time:write scope), and maps the Slack user to an
// employee. ok=false means the response is already written.
func (h *Handler) authorize(ctx context.Context, w http.ResponseWriter, ws store.SlackWorkspace, slackUserID string) (empID, orgID string, ok bool) {
	token, err := secrets.Decrypt(ws.EncServiceToken)
	if err != nil {
		writeSlackError(w, http.StatusInternalServerError, "integration misconfigured")
		return "", "", false
	}
	cred, err := h.store.GetCredentialByTokenHash(ctx, hashToken(string(token)))
	if err != nil {
		// Revoked, expired, or unknown: the integration is disabled.
		writeEphemeral(w, "This workspace's forge-hris integration token is invalid or revoked. Ask an admin to reconnect it.")
		return "", "", false
	}
	if cred.OrganizationID != ws.OrganizationID {
		writeEphemeral(w, "This workspace's forge-hris integration token belongs to a different organization. Ask an admin to reconnect it.")
		return "", "", false
	}
	if !hasScope(cred.Scopes, store.ScopeTimeWrite) {
		writeEphemeral(w, "This workspace's forge-hris integration token lacks the time:write scope. Ask an admin to reconnect it.")
		return "", "", false
	}
	empID, err = h.store.GetSlackUserEmployee(ctx, ws.TeamID, slackUserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeEphemeral(w, "Your Slack account isn't linked to a forge-hris employee yet. Ask an admin to link it under Integrations.")
			return "", "", false
		}
		writeSlackError(w, http.StatusInternalServerError, "internal error")
		return "", "", false
	}
	return empID, ws.OrganizationID, true
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// --- slash commands ---

func (h *Handler) handleSlashCommand(w http.ResponseWriter, r *http.Request, orgID, empID, command, text string) {
	ctx := r.Context()
	now := h.now().UTC()
	switch command {
	case "/clockin":
		e, err := h.store.ClockIn(ctx, orgID, empID, store.TimeEntry{ClientType: store.TimeEntryClientSlack, OccurredAt: now})
		if err != nil {
			writeEphemeral(w, clockErrText("clock in", err))
			return
		}
		writeMessageWithBreakButton(w,
			fmt.Sprintf(":white_check_mark: Clocked in at %s. Have a good shift!", e.OccurredAt.Format("15:04")),
			"break_start", "Start break")
	case "/clockout":
		e, err := h.store.ClockOut(ctx, orgID, empID, store.TimeEntry{ClientType: store.TimeEntryClientSlack, OccurredAt: now})
		if err != nil {
			writeEphemeral(w, clockErrText("clock out", err))
			return
		}
		writeEphemeral(w, fmt.Sprintf(":wave: Clocked out at %s. Shift complete.", e.OccurredAt.Format("15:04")))
	case "/break":
		// Toggle: on break -> end it, otherwise start it.
		if open, isOpen, err := h.store.OpenTimeEntry(ctx, orgID, empID); err == nil && isOpen && open.Kind == store.TimeEntryStartBreak {
			h.endBreak(w, r, orgID, empID)
			return
		}
		h.startBreak(w, r, orgID, empID)
	default:
		writeEphemeral(w, fmt.Sprintf("Unknown command %q. Try /clockin, /clockout, or /break.", command))
	}
}

func (h *Handler) startBreak(w http.ResponseWriter, r *http.Request, orgID, empID string) {
	e, err := h.store.StartBreak(r.Context(), orgID, empID, store.TimeEntry{ClientType: store.TimeEntryClientSlack, OccurredAt: h.now().UTC()})
	if err != nil {
		writeEphemeral(w, clockErrText("start break", err))
		return
	}
	writeMessageWithBreakButton(w,
		fmt.Sprintf(":coffee: Break started at %s.", e.OccurredAt.Format("15:04")),
		"break_end", "End break")
}

func (h *Handler) endBreak(w http.ResponseWriter, r *http.Request, orgID, empID string) {
	e, err := h.store.EndBreak(r.Context(), orgID, empID, store.TimeEntry{ClientType: store.TimeEntryClientSlack, OccurredAt: h.now().UTC()})
	if err != nil {
		writeEphemeral(w, clockErrText("end break", err))
		return
	}
	writeMessageWithBreakButton(w,
		fmt.Sprintf(":arrow_forward: Break ended at %s — back on the clock.", e.OccurredAt.Format("15:04")),
		"break_start", "Start break")
}

func clockErrText(action string, err error) string {
	switch {
	case errors.Is(err, store.ErrAlreadyClockedIn):
		return "You're already clocked in. Use /clockout first."
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

// --- interactive block_actions ---

// interactivePayload is the subset of Slack's block_actions payload we act on.
type interactivePayload struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

func (h *Handler) handleInteractive(w http.ResponseWriter, r *http.Request, ws store.SlackWorkspace, orgID, empID, userID, teamID, payload string) {
	var p interactivePayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		writeEphemeral(w, "Couldn't understand that button press. Try /clockin instead.")
		return
	}
	if p.Type != "block_actions" || len(p.Actions) == 0 {
		writeEphemeral(w, "Nothing to do for that interaction.")
		return
	}
	// The interactive payload carries its own team/user; re-check they
	// match the signed form fields (defense in depth).
	if p.Team.ID != "" && p.Team.ID != teamID {
		writeSlackError(w, http.StatusBadRequest, "team mismatch")
		return
	}
	if p.User.ID != "" && p.User.ID != userID {
		writeSlackError(w, http.StatusBadRequest, "user mismatch")
		return
	}
	action := p.Actions[0].ActionID
	if action == "" {
		action = p.Actions[0].Value
	}
	switch action {
	case "break_start":
		h.startBreak(w, r, orgID, empID)
	case "break_end":
		h.endBreak(w, r, orgID, empID)
	default:
		writeEphemeral(w, fmt.Sprintf("Unknown button %q.", action))
	}
}

// --- Slack response helpers ---

func writeSlackError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeEphemeral writes an ephemeral text reply (visible only to the user).
func writeEphemeral(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"response_type": "ephemeral",
		"text":          text,
	})
}

// writeMessageWithBreakButton writes an ephemeral message with a single
// break toggle button (actionID "break_start" or "break_end").
func writeMessageWithBreakButton(w http.ResponseWriter, text, actionID, label string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"response_type": "ephemeral",
		"text":          text,
		"blocks": []any{
			map[string]any{
				"type": "section",
				"text": map[string]string{"type": "mrkdwn", "text": text},
			},
			map[string]any{
				"type": "actions",
				"elements": []any{
					map[string]any{
						"type":      "button",
						"text":      map[string]string{"type": "plain_text", "text": label},
						"action_id": actionID,
						"value":     actionID,
					},
				},
			},
		},
	})
}
