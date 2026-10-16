package mcp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"
)

// Tier is a tool's approval tier. Tiers are enforced server-side in
// executeTool; the annotations advertised in tools/list are hints only.
type Tier string

const (
	// TierAuto executes immediately once auth and scope/role gates pass.
	TierAuto Tier = "auto"
	// TierConfirm executes NOTHING until a human approves: the first call
	// returns {approval_required:true, resume_token, preview}; a follow-up
	// call carrying the resume token and approved:true executes.
	TierConfirm Tier = "confirm"
	// TierNever can never execute inline: the first call returns a preview
	// with counts, and the follow-up requires the resume token AND an
	// explicit confirmed:true in the arguments.
	TierNever Tier = "never"
)

// ValidTier reports whether t is a known tier.
func ValidTier(t Tier) bool {
	switch t {
	case TierAuto, TierConfirm, TierNever:
		return true
	}
	return false
}

// approvalTTL bounds how long a resume token stays valid.
const approvalTTL = 10 * time.Minute

// pendingApproval is one issued-but-unexecuted approval. args is the
// approved snapshot: the follow-up executes exactly these arguments, so a
// resume token can never be repurposed for different parameters.
type pendingApproval struct {
	token     string
	tool      string
	tier      Tier
	args      map[string]any
	preview   any
	createdAt time.Time
}

// approvalStore is the in-memory resume-token registry. Single-process,
// single-use tokens with a 10-minute TTL. It is safe for concurrent use.
type approvalStore struct {
	mu      sync.Mutex
	pending map[string]*pendingApproval
}

func newApprovalStore() *approvalStore {
	return &approvalStore{pending: make(map[string]*pendingApproval)}
}

// newResumeToken mints an unguessable single-use token.
func newResumeToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// snapshotArgs deep-copies args through JSON so later caller mutations
// can't alter the approved snapshot.
func snapshotArgs(args map[string]any) map[string]any {
	raw, err := json.Marshal(args)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// issue records a pending approval and returns it. Expired entries are
// swept lazily on every access; the map stays small (one entry per pending
// human confirmation).
func (g *approvalStore) issue(tool string, tier Tier, args map[string]any, preview any) (*pendingApproval, error) {
	token, err := newResumeToken()
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked()
	pa := &pendingApproval{
		token:     token,
		tool:      tool,
		tier:      tier,
		args:      snapshotArgs(args),
		preview:   preview,
		createdAt: time.Now(),
	}
	g.pending[token] = pa
	return pa, nil
}

// consume looks a resume token up and deletes it (single-use). It returns
// APPROVAL_NOT_FOUND for unknown tokens and APPROVAL_EXPIRED for lapsed
// ones; both are tool-level errors so the agent gets a structured reply.
func (g *approvalStore) consume(tool, token string) (*pendingApproval, *ToolError) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pa, ok := g.pending[token]
	if !ok {
		g.sweepLocked()
		return nil, &ToolError{Code: "APPROVAL_NOT_FOUND", Message: "unknown or already-used resume token"}
	}
	// Single-use: the token is consumed even when it turns out to be for
	// the wrong tool or expired.
	delete(g.pending, token)
	g.sweepLocked()
	if pa.tool != tool {
		return nil, &ToolError{Code: "APPROVAL_NOT_FOUND", Message: "resume token was issued for a different tool"}
	}
	if time.Since(pa.createdAt) > approvalTTL {
		return nil, &ToolError{Code: "APPROVAL_EXPIRED", Message: "resume token expired; request approval again"}
	}
	return pa, nil
}

// sweepLocked deletes expired entries. Callers must hold g.mu.
func (g *approvalStore) sweepLocked() {
	now := time.Now()
	for token, pa := range g.pending {
		if now.Sub(pa.createdAt) > approvalTTL {
			delete(g.pending, token)
		}
	}
}

// approvalRequiredResult builds the structured reply for a gated tool call
// that has not been approved yet. Nothing has executed at this point.
func approvalRequiredResult(tool string, tier Tier, pa *pendingApproval) map[string]any {
	next := "re-call " + tool + " with the same arguments plus " +
		`"resume_token": "` + pa.token + `"`
	if tier == TierNever {
		next += ` and "confirmed": true`
	} else {
		next += ` and "approved": true`
	}
	return map[string]any{
		"approval_required": true,
		"tier":              string(tier),
		"tool":              tool,
		"resume_token":      pa.token,
		"expires_at":        pa.createdAt.Add(approvalTTL).UTC().Format(time.RFC3339),
		"preview":           pa.preview,
		"next":              next,
	}
}
