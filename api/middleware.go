package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// --- fixed-window rate limiting ---

// rateLimiter is a simple in-memory fixed-window limiter keyed by credential
// ID. It is per-process state; behind multiple replicas each replica enforces
// its own window (fail-safe direction: slightly stricter, never looser).
type rateLimiter struct {
	mu      sync.Mutex
	perMin  int
	windows map[string]*rateWindow
}

type rateWindow struct {
	count int
	reset time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{perMin: perMin, windows: make(map[string]*rateWindow)}
}

// allow reports whether the key may proceed, and how long until the window
// resets when it may not.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.windows) > 4096 {
		for k, w := range l.windows {
			if now.After(w.reset) {
				delete(l.windows, k)
			}
		}
	}
	w, ok := l.windows[key]
	if !ok || now.After(w.reset) {
		l.windows[key] = &rateWindow{count: 1, reset: now.Add(time.Minute)}
		return true, 0
	}
	if w.count >= l.perMin {
		return false, time.Until(w.reset)
	}
	w.count++
	return true, 0
}

func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ai := authOf(r)
		if ai == nil {
			next.ServeHTTP(w, r) // healthz, bootstrap: no credential to key on
			return
		}
		if ok, retry := s.limiter.allow(ai.CredentialID); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			writeError(w, r, http.StatusTooManyRequests, "RATE_LIMITED",
				"rate limit exceeded: 100 requests/minute per credential")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- idempotency for POSTs ---

// keyedMutex serializes in-flight requests sharing an idempotency key so two
// concurrent retries can't both execute the mutation on one replica.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex { return &keyedMutex{m: make(map[string]*sync.Mutex)} }

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	m, ok := k.m[key]
	if !ok {
		m = &sync.Mutex{}
		k.m[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// recordWriter captures the handler's response so it can be replayed and
// persisted as the idempotent result.
type recordWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func newRecordWriter() *recordWriter {
	return &recordWriter{header: http.Header{}, status: http.StatusOK}
}

func (w *recordWriter) Header() http.Header { return w.header }

func (w *recordWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
}

func (w *recordWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

// withIdempotency implements Idempotency-Key for POSTs. The fingerprint is
// hex(sha256(method|path|rawBody)); a repeat with the same fingerprint
// replays the stored status and body, a repeat with a different fingerprint
// is rejected with 409 IDEMPOTENCY_KEY_REUSED. Only 2xx responses are
// persisted (4xx/5xx are safe to retry). The store applies the 24h TTL.
//
// Requests without an authenticated caller (the bootstrap) skip idempotency:
// there is no organization to scope the key to yet, and the slug clash
// already returns 409 ALREADY_EXISTS on a retried bootstrap.
func (s *Server) withIdempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ai := authOf(r)
		key := r.Header.Get("Idempotency-Key")
		if r.Method != http.MethodPost || key == "" || ai == nil {
			next.ServeHTTP(w, r)
			return
		}
		// The outer middleware already capped the body at 1 MiB.
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			next.ServeHTTP(w, r) // handler will reject the bad body
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))

		sum := sha256.Sum256([]byte(r.Method + "|" + r.URL.Path + "|" + string(raw)))
		fingerprint := hex.EncodeToString(sum[:])

		unlock := s.idemMu.lock(ai.OrgID + "\x00" + key)
		defer unlock()

		if rec, found, err := s.store.IdempotencyGet(r.Context(), ai.OrgID, key); !mapError(w, r, err) {
			return
		} else if found {
			if rec.Fingerprint != fingerprint {
				writeError(w, r, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED",
					"idempotency key was already used with a different request")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.StatusCode)
			_, _ = w.Write(rec.ResponseBody)
			return
		}

		rec := newRecordWriter()
		next.ServeHTTP(rec, r)

		for k, vv := range rec.header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.status)
		body := append([]byte(nil), rec.body.Bytes()...)
		_, _ = w.Write(body)

		if rec.status < 200 || rec.status >= 300 {
			return // error responses are not cached; the client may retry
		}
		if err := s.store.IdempotencyPut(r.Context(), store.IdempotencyRecord{
			OrganizationID: ai.OrgID,
			Key:            key,
			Fingerprint:    fingerprint,
			StatusCode:     rec.status,
			ResponseBody:   body,
			CreatedAt:      time.Now().UTC(),
		}); err != nil {
			s.logger.Error("idempotency put failed", "key", key, "error", err)
		}
	})
}
