package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// protocolVersion is the MCP protocol version this server speaks.
const protocolVersion = "2024-11-05"

// serverName and serverVersion identify the server in initialize.
const (
	serverName    = "forge-hris-mcp"
	serverVersion = "0.1.0"
)

// maxBodyBytes caps JSON-RPC request bodies (HTTP transport).
const maxBodyBytes = 1 << 20 // 1 MiB

// JSON-RPC 2.0 error codes. -32700..-32603 are standard; -32002 is the MCP
// "server not initialized" code; -32001 and -32003 are server-defined.
const (
	rpcParseError      = -32700
	rpcInvalidRequest  = -32600
	rpcMethodNotFound  = -32601
	rpcInvalidParams   = -32602
	rpcInternalError   = -32603
	rpcNotInitialized  = -32002
	rpcUnauthenticated = -32001
	rpcForbidden       = -32003
)

// rpcError is a JSON-RPC 2.0 error: request-level failures (bad envelope,
// unknown method, auth failures). Tool execution failures instead return a
// result with isError=true so the agent gets structured content.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// ToolError is a tool-level failure, returned inside a result with
// isError=true. Code is a stable UPPER_SNAKE string mirroring the REST
// error envelope.
type ToolError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// rpcRequest is one JSON-RPC 2.0 message. ID is kept raw so numbers and
// strings echo back verbatim; a missing ID marks a notification.
type rpcRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params"`
}

func (r *rpcRequest) isNotification() bool { return r.ID == nil }

// Server is the MCP server: JSON-RPC dispatch over a store.Store, with the
// tool registry, the approval gates, and the SSE notification fan-out.
type Server struct {
	store     store.Store
	logger    *slog.Logger
	registry  *Registry
	approvals *approvalStore

	mu       sync.Mutex
	notifyCh []chan serverNotification
}

// serverNotification is one server->client notification for the SSE stream.
type serverNotification struct {
	Method string
	Params any
}

// NewServer builds the MCP server. Pass nil logger for a discard logger.
func NewServer(st store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		store:     st,
		logger:    logger,
		approvals: newApprovalStore(),
	}
	s.registry = newRegistry(s)
	return s
}

// Registry returns the tool registry.
func (s *Server) Registry() *Registry { return s.registry }

// Notify queues a server notification for SSE subscribers. It never blocks;
// a slow subscriber drops the notification (documented limit).
func (s *Server) Notify(method string, params any) {
	s.mu.Lock()
	subs := append([]chan serverNotification(nil), s.notifyCh...)
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- serverNotification{Method: method, Params: params}:
		default:
		}
	}
}

// session is one client's connection state. stdio has exactly one session
// for the process lifetime; HTTP builds an ephemeral session per request.
type session struct {
	server      *Server
	token       string // resolved bearer token
	ai          *AuthInfo
	initialized bool
	stateless   bool // HTTP: skip the initialize-before-use requirement
}

func (sess *session) ensureAuth(ctx context.Context) *rpcError {
	if sess.ai != nil {
		return nil
	}
	if strings.TrimSpace(sess.token) == "" {
		return &rpcError{Code: rpcUnauthenticated, Message: "unauthenticated: no bearer token for this session",
			Data: map[string]any{"code": "UNAUTHENTICATED"}}
	}
	ai, rerr := sess.server.Authenticate(ctx, strings.TrimSpace(sess.token))
	if rerr != nil {
		return rerr
	}
	sess.ai = ai
	return nil
}

// dispatch handles one raw JSON-RPC message and returns the response bytes,
// or nil for notifications (no response).
func (s *Server) dispatch(ctx context.Context, sess *session, raw []byte) []byte {
	var req rpcRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return marshalResponse(nil, &rpcError{Code: rpcParseError, Message: "parse error: " + err.Error()})
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return marshalResponse(req.ID, &rpcError{Code: rpcInvalidRequest, Message: "invalid request: jsonrpc must be \"2.0\" with a method"})
	}
	notification := req.isNotification()

	// Methods that never need initialization or auth.
	switch req.Method {
	case "ping":
		return respondOrNil(notification, req.ID, map[string]any{}, nil)
	case "notifications/initialized":
		sess.initialized = true
		return nil
	case "notifications/cancelled":
		return nil
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil // unknown notifications are ignored, never answered
	}
	if !sess.stateless && !sess.initialized && req.Method != "initialize" {
		return marshalResponse(req.ID, &rpcError{Code: rpcNotInitialized,
			Message: "server not initialized: send initialize first",
			Data:    map[string]any{"code": "NOT_INITIALIZED"}})
	}

	switch req.Method {
	case "initialize":
		res, rerr := s.handleInitialize(ctx, sess, req.Params)
		return respondOrNil(notification, req.ID, res, rerr)
	case "tools/list":
		if rerr := sess.ensureAuth(ctx); rerr != nil {
			return marshalResponse(req.ID, rerr)
		}
		return respondOrNil(notification, req.ID, s.handleToolsList(req.Params), nil)
	case "tools/call":
		if rerr := sess.ensureAuth(ctx); rerr != nil {
			return marshalResponse(req.ID, rerr)
		}
		res, rerr := s.handleToolsCall(ctx, sess, req.Params)
		return respondOrNil(notification, req.ID, res, rerr)
	default:
		return marshalResponse(req.ID, &rpcError{Code: rpcMethodNotFound,
			Message: "method not found: " + req.Method})
	}
}

func respondOrNil(notification bool, id *json.RawMessage, res any, rerr *rpcError) []byte {
	if notification {
		return nil
	}
	return marshalResponse(id, rerr, res)
}

func marshalResponse(id *json.RawMessage, rerr *rpcError, res ...any) []byte {
	out := map[string]any{"jsonrpc": "2.0"}
	if id != nil {
		out["id"] = *id
	} else {
		out["id"] = nil
	}
	if rerr != nil {
		out["error"] = rerr
	} else if len(res) > 0 {
		out["result"] = res[0]
	} else {
		out["result"] = map[string]any{}
	}
	raw, _ := json.Marshal(out)
	return raw
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      map[string]any `json:"clientInfo"`
	Token           string         `json:"token"` // forge extension: stdio auth without env
}

func (s *Server) handleInitialize(ctx context.Context, sess *session, raw json.RawMessage) (any, *rpcError) {
	var p initializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid params: " + err.Error()}
		}
	}
	if strings.TrimSpace(p.Token) != "" {
		sess.token = strings.TrimSpace(p.Token)
		sess.ai = nil // re-resolve with the new token
	}
	if rerr := sess.ensureAuth(ctx); rerr != nil {
		return nil, rerr
	}
	sess.initialized = true
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools":       map[string]any{},
			"elicitation": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": serverVersion,
		},
		"instructions": "forge-hris agent surface. Mutating tools are tiered: auto executes at once; " +
			"confirm returns {approval_required:true, resume_token, preview} and executes nothing until you " +
			"re-call with the resume token and approved:true; never (bulk approvals) additionally requires " +
			"confirmed:true on the follow-up. Use on_behalf_of to record delegated act_as in the audit log.",
	}, nil
}

// toolJSON is the tools/list item shape.
type toolJSON struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
	Meta         map[string]any `json:"_meta,omitempty"`
}

func (s *Server) handleToolsList(raw json.RawMessage) any {
	tools := make([]toolJSON, 0, len(s.registry.defs))
	for _, d := range s.registry.defs {
		a := d.Annotations
		tools = append(tools, toolJSON{
			Name:         d.Name,
			Description:  d.Description,
			InputSchema:  d.InputSchema,
			OutputSchema: d.OutputSchema,
			Annotations: map[string]any{
				"readOnlyHint":    a.ReadOnlyHint,
				"destructiveHint": a.DestructiveHint,
				"idempotentHint":  a.IdempotentHint,
				"openWorldHint":   a.OpenWorldHint,
				"title":           a.Title,
			},
			Meta: map[string]any{
				"forge": map[string]any{
					"tier":          string(d.Tier),
					"requiredScope": d.RequiredScope,
					"minRole":       string(d.MinRole),
				},
			},
		})
	}
	return map[string]any{"tools": tools}
}

type toolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (s *Server) handleToolsCall(ctx context.Context, sess *session, raw json.RawMessage) (any, *rpcError) {
	var p toolsCallParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid params: " + err.Error()}
		}
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid params: name is required"}
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	res, rerr := s.executeTool(ctx, sess.ai, p.Name, p.Arguments)
	if rerr != nil {
		return nil, rerr
	}
	return res, nil
}

// contentBlock is one MCP content item.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolCallResult is the tools/call result body.
type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

func textResult(v any, isError bool) *toolCallResult {
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte(`{"code":"INTERNAL","message":"result encoding failed"}`)
		isError = true
	}
	return &toolCallResult{Content: []contentBlock{{Type: "text", Text: string(raw)}}, IsError: isError}
}

func toolErrorResult(terr *ToolError) *toolCallResult {
	return textResult(map[string]any{
		"code": terr.Code, "message": terr.Message, "details": terr.Details,
	}, true)
}

// executeTool runs one tool call: gates (scope/role), the approval-tier
// flow, the handler, and the agent-identity audit event on success.
func (s *Server) executeTool(ctx context.Context, ai *AuthInfo, name string, args map[string]any) (*toolCallResult, *rpcError) {
	def, ok := s.registry.Get(name)
	if !ok {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid params: unknown tool " + name}
	}
	if def.RequiredScope != "" && !hasScope(def.RequiredScope, ai) {
		return nil, &rpcError{Code: rpcForbidden,
			Message: "forbidden: requires scope " + def.RequiredScope,
			Data:    map[string]any{"code": "FORBIDDEN"}}
	}
	if def.MinRole != "" && !roleAtLeast(def.MinRole, ai) {
		return nil, &rpcError{Code: rpcForbidden,
			Message: "forbidden: requires " + string(def.MinRole) + " role or higher",
			Data:    map[string]any{"code": "FORBIDDEN"}}
	}

	// Split the reserved control keys off the handler arguments.
	clean := make(map[string]any, len(args))
	var resumeToken, onBehalfOf string
	var approved, confirmed bool
	for k, v := range args {
		switch {
		case k == "resume_token":
			resumeToken, _ = v.(string)
		case k == "approved":
			approved, _ = v.(bool)
		case k == "confirmed":
			confirmed, _ = v.(bool)
		case k == "on_behalf_of":
			onBehalfOf, _ = v.(string)
		default:
			clean[k] = v
		}
	}
	var obo *string
	if strings.TrimSpace(onBehalfOf) != "" {
		v := strings.TrimSpace(onBehalfOf)
		obo = &v
	}

	switch def.Tier {
	case TierAuto:
		return s.runHandler(ctx, ai, def, clean, obo)
	case TierConfirm, TierNever:
		if def.Preview == nil {
			s.logger.Error("gated tool without preview", "tool", name)
			return nil, &rpcError{Code: rpcInternalError, Message: "internal error"}
		}
		if resumeToken != "" {
			pa, terr := s.approvals.consume(name, resumeToken)
			if terr != nil {
				return toolErrorResult(terr), nil
			}
			if def.Tier == TierNever {
				if !confirmed {
					return toolErrorResult(toolErr("APPROVAL_NOT_CONFIRMED",
						"this tool never auto-approves: re-call with the resume token AND confirmed:true")), nil
				}
			} else if !approved {
				return toolErrorResult(toolErr("APPROVAL_NOT_CONFIRMED",
					"resume token presented without approved:true; nothing was executed")), nil
			}
			// The token is consumed (single-use). Execute the approved
			// snapshot: the token binds to these exact arguments.
			return s.runHandler(ctx, ai, def, pa.args, obo)
		}
		preview, terr := def.Preview(ctx, ai, clean)
		if terr != nil {
			return toolErrorResult(terr), nil
		}
		pa, err := s.approvals.issue(name, def.Tier, clean, preview)
		if err != nil {
			s.logger.Error("approval issue failed", "tool", name, "error", err)
			return nil, &rpcError{Code: rpcInternalError, Message: "internal error"}
		}
		return textResult(approvalRequiredResult(name, def.Tier, pa), false), nil
	default:
		return nil, &rpcError{Code: rpcInternalError, Message: "internal error: unknown tier"}
	}
}

// runHandler executes the tool handler and appends the agent-identity audit
// event on success. Audit failures are logged, never returned: the mutation
// already succeeded.
func (s *Server) runHandler(ctx context.Context, ai *AuthInfo, def *ToolDef, args map[string]any, onBehalfOf *string) (*toolCallResult, *rpcError) {
	result, audit, terr := def.Handler(ctx, ai, args)
	if terr != nil {
		return toolErrorResult(terr), nil
	}
	if audit != nil {
		s.appendAudit(ctx, ai, onBehalfOf, audit)
	}
	// Mirror the REST API's webhook emission for tool mutations (best
	// effort: enqueue failures are logged inside emitWebhookEvent).
	s.emitWebhookEvent(ctx, ai, def.Name, result)
	return textResult(result, false), nil
}

// appendAudit records one audit event for a successful tool execution with
// the agent's identity: ActorType=agent, ActorID=credential ID, and the
// delegated act_as identity in OnBehalfOf when given.
func (s *Server) appendAudit(ctx context.Context, ai *AuthInfo, onBehalfOf *string, ar *auditRecord) {
	ev := store.AuditEvent{
		OrganizationID: ai.OrgID,
		ActorType:      store.ActorAgent,
		ActorID:        ai.CredentialID,
		OnBehalfOf:     onBehalfOf,
		Action:         ar.action,
		ResourceType:   ar.resourceType,
		ResourceID:     ar.resourceID,
		Before:         mustJSON(ar.before),
		After:          mustJSON(ar.after),
	}
	if _, err := s.store.AppendAuditEvent(ctx, ev); err != nil {
		s.logger.Error("audit append failed",
			"action", ev.Action, "resource_type", ev.ResourceType,
			"resource_id", ev.ResourceID, "error", err)
	}
}

func mustJSON(v any) []byte {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"marshal_error":true}`)
	}
	return raw
}

// Elicit is the server-side elicitation entry point. Elicitation (human in
// the loop over the MCP channel) is NOT supported on either transport:
// approval-gated tools use the resume-token flow instead, which works the
// same over stdio and HTTP. The capability is still advertised so clients
// know the server understands the shape; calling this returns the
// structured not-supported error.
func (s *Server) Elicit(ctx context.Context, message string, schema map[string]any) (map[string]any, *ToolError) {
	return nil, &ToolError{
		Code:    "ELICITATION_NOT_SUPPORTED",
		Message: "elicitation is not supported on this transport; use the approval resume flow (resume_token + approved:true, or confirmed:true for never-tier tools)",
		Details: map[string]any{"message": message},
	}
}

// --- stdio transport ---

// ServeStdio runs newline-delimited JSON-RPC 2.0 over r/w until EOF or ctx
// cancellation. The bearer token comes from FORGE_HRIS_MCP_TOKEN or the
// token field of initialize params; without either, tools/* calls are
// rejected as unauthenticated.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer, token string) error {
	sess := &session{server: s, token: token}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxBodyBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if resp := s.dispatch(ctx, sess, line); resp != nil {
			resp = append(resp, '\n')
			if _, err := w.Write(resp); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return scanner.Err()
}

// --- HTTP transport ---

// HTTPHandler returns the HTTP transport: POST /mcp for JSON-RPC
// request->response, GET /mcp for the SSE notification stream.
func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", s.handleMCPPOST)
	mux.HandleFunc("GET /mcp", s.handleMCPGET)
	return mux
}

func bearerToken(r *http.Request) string {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func writeRPCError(w http.ResponseWriter, id *json.RawMessage, rerr *rpcError) {
	status := http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(marshalResponse(id, rerr))
}

func (s *Server) handleMCPPOST(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := &session{server: s, token: bearerToken(r), stateless: true}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeRPCError(w, nil, &rpcError{Code: rpcParseError, Message: "parse error: " + err.Error()})
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		writeRPCError(w, nil, &rpcError{Code: rpcInvalidRequest, Message: "invalid request: empty body"})
		return
	}
	// MCP forbids JSON-RPC batching; reject arrays explicitly.
	if raw[0] == '[' {
		writeRPCError(w, nil, &rpcError{Code: rpcInvalidRequest, Message: "invalid request: batching is not supported"})
		return
	}
	resp := s.dispatch(ctx, sess, raw)
	if resp == nil {
		// Notification: 202 Accepted, no body (MCP Streamable HTTP).
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// handleMCPGET serves the SSE stream: one `ready` event, then server
// notifications until the client disconnects. Limits (documented): no
// resumability (no Last-Event-ID), no session IDs, one broadcast domain
// per process.
func (s *Server) handleMCPGET(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := &session{server: s, token: bearerToken(r), stateless: true}
	if rerr := sess.ensureAuth(ctx); rerr != nil {
		writeRPCError(w, nil, rerr)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRPCError(w, nil, &rpcError{Code: rpcInternalError, Message: "streaming not supported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan serverNotification, 16)
	s.mu.Lock()
	s.notifyCh = append(s.notifyCh, ch)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for i, c := range s.notifyCh {
			if c == ch {
				s.notifyCh = append(s.notifyCh[:i], s.notifyCh[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()

	ready, _ := json.Marshal(map[string]any{
		"protocol": protocolVersion, "server": serverName, "version": serverVersion,
	})
	fmt.Fprintf(w, "event: ready\ndata: %s\n\n", ready)
	flusher.Flush()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-ch:
			data, _ := json.Marshal(n.Params)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", n.Method, data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
