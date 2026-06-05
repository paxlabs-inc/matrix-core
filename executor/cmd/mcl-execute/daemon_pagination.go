// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_pagination.go — opaque cursor encode/decode for list endpoints.
//
// Cursor shape (base64-url-encoded JSON):
//
//   {"ts": <unix_nano>, "id": "<entity-specific-id>"}
//
// Cursors are stable across daemon restarts because both fields are
// content-derivable from the entity itself (intent terminal time +
// intent_id, memory created_at + memory id, etc.). Clients may treat
// the cursor as fully opaque.
//
// Pagination contract:
//
//   GET /<list>?cursor=<opaque>&limit=<int>     20 default, 200 max
//
// Response body always carries:
//
//   {"items": [...], "next_cursor": "<opaque>", "total_estimate": <int?>}
//
// next_cursor is omitted when the page is the last one.
// total_estimate is "best effort" — it MAY be approximate or absent.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// listCursor is the typed cursor that gets base64-encoded to opaque
// strings on the wire. Unexported field tags keep wire size compact.
type listCursor struct {
	TS int64  `json:"t"`
	ID string `json:"i,omitempty"`
}

// pageParams parses ?cursor= and ?limit= from a request, applies caps,
// and returns the typed cursor (zero-value when no cursor was supplied)
// plus the resolved limit.
//
// Errors are written to w as 400 and (false, ...) returned; callers
// MUST return after a false result.
func pageParams(w http.ResponseWriter, r *http.Request, defaultLimit, maxLimit int) (listCursor, int, bool) {
	if defaultLimit <= 0 {
		defaultLimit = 20
	}
	if maxLimit <= 0 {
		maxLimit = 200
	}

	q := r.URL.Query()
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("invalid limit=%q (want positive integer)", v),
			})
			return listCursor{}, 0, false
		}
		if n > maxLimit {
			n = maxLimit
		}
		limit = n
	}

	var cur listCursor
	if v := q.Get("cursor"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid cursor: " + err.Error(),
			})
			return listCursor{}, 0, false
		}
		if err := json.Unmarshal(raw, &cur); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid cursor body: " + err.Error(),
			})
			return listCursor{}, 0, false
		}
	}
	return cur, limit, true
}

// encodeCursor renders a listCursor to its opaque wire form.
func encodeCursor(c listCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// pagedResult is the standard list-response envelope. Generic over the
// item type via interface{} so each list endpoint can return its own
// typed slice without per-endpoint envelope structs.
type pagedResult struct {
	Items         interface{} `json:"items"`
	NextCursor    string      `json:"next_cursor,omitempty"`
	TotalEstimate *int        `json:"total_estimate,omitempty"`
}

// writePaged is the canonical response writer for paginated GET
// endpoints. nextCursor empty => no further pages. totalEstimate < 0 =>
// omit from the response.
func writePaged(w http.ResponseWriter, items interface{}, nextCursor string, totalEstimate int) {
	out := pagedResult{
		Items:      items,
		NextCursor: nextCursor,
	}
	if totalEstimate >= 0 {
		out.TotalEstimate = &totalEstimate
	}
	writeJSON(w, http.StatusOK, out)
}

// queryString reads a single query string param with an optional
// default. Empty string is treated as absent.
func queryString(r *http.Request, name, def string) string {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	return v
}

// queryInt reads a single integer query param with a default. Negative
// def disables presence-required validation; pass def=-1 + check the
// returned ok if you need to distinguish "absent" from "zero".
func queryInt(r *http.Request, name string, def int) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, false
	}
	return n, true
}

// queryBool reads a single boolean query param. Accepts "1"/"true"/
// "yes" as true, "0"/"false"/"no"/"" as false.
func queryBool(r *http.Request, name string, def bool) bool {
	v := r.URL.Query().Get(name)
	switch v {
	case "1", "true", "True", "TRUE", "yes", "Yes", "YES":
		return true
	case "0", "false", "False", "FALSE", "no", "No", "NO":
		return false
	case "":
		return def
	}
	return def
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
