package api

import (
	"net/http"
	"strconv"

	"github.com/azmi2409/forge-hris/store"
)

const (
	defaultPageLimit = 100
	maxPageLimit     = 1000
)

// parsePage reads ?limit= (default 100, clamped to 1000) and ?cursor= into a
// store.Page. The cursor is opaque to the API: it is the base64url
// "RFC3339Nano|id" token produced by the store.
func parsePage(w http.ResponseWriter, r *http.Request) (store.Page, bool) {
	p := store.Page{Limit: defaultPageLimit, Cursor: r.URL.Query().Get("cursor")}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"limit must be a positive integer")
			return p, false
		}
		if n > maxPageLimit {
			n = maxPageLimit
		}
		p.Limit = n
	}
	return p, true
}

// pageEnvelope is the standard list response.
type pageEnvelope struct {
	Data       any    `json:"data"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

// writePage writes a cursor-paginated list. Callers must pass a non-nil
// slice so data encodes as [] rather than null.
func writePage(w http.ResponseWriter, data any, nextCursor string) {
	writeJSON(w, http.StatusOK, pageEnvelope{
		Data:       data,
		NextCursor: nextCursor,
		HasMore:    nextCursor != "",
	})
}
