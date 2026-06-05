// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_events.go — /events SSE handlers with intent_id + phase
// filtering, plus /events/replay/:intent_id for backfill (sess#27).
//
// Routes:
//
//   GET /events?intent_id=&phase=&since_seq=    live SSE firehose, filtered
//   GET /events/replay/:intent_id               re-emit historical events
//
// The legacy unfiltered /events handler in daemon_server.go is replaced
// by handleEventsFiltered. Filters are applied at Publish-time inside
// the broker so per-subscriber CPU cost stays bounded.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// handleEventsFiltered serves GET /events with optional filtering.
//
// Filter precedence:
//
//	?intent_id=<id>      restrict to events whose Fields.intent_id matches
//	?phase=<phase>       restrict to events from a single phase
//	                     (boot|compile|synth|walk|attest|envelope|
//	                      lifecycle|infra|http|snapshot)
//	?since_seq=<int>     skip events with seq < this (pagination)
//
// All filters compose; the empty-string default disables a filter.
func (d *daemonState) handleEventsFiltered(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "streaming unsupported",
		})
		return
	}
	filter := sseFilter{
		IntentID: queryString(r, "intent_id", ""),
		Phase:    queryString(r, "phase", ""),
	}
	if v := r.URL.Query().Get("since_seq"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err == nil {
			filter.SinceSeq = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": connected actor=%s filter=%v\n\n", d.actor.UserURI, filter)
	flusher.Flush()

	id, ch := d.broker.SubscribeFiltered(filter)
	defer d.broker.Unsubscribe(id)

	ctx := r.Context()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := encodeSSEEvent(ev)
			if err != nil {
				continue
			}
			if _, err := w.Write(payload); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": heartbeat %d\n\n", time.Now().Unix()); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleEventsReplay serves GET /events/replay/:intent_id.
//
// Re-emits every event from the historical transcript JSONL for one
// intent in chronological order. Useful for clients that joined the
// SSE feed mid-flight and want to backfill.
//
// Streams the same SSE wire format as live /events so the same client
// parser works without branching on shape.
func (d *daemonState) handleEventsReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	intentID := strings.TrimPrefix(r.URL.Path, "/events/replay/")
	intentID = strings.Trim(intentID, "/")
	if intentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "intent id required"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "streaming unsupported",
		})
		return
	}
	path := filepath.Join(d.transcriptsDir, intentID+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "transcript not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": replay intent=%s\n\n", intentID)
	flusher.Flush()

	dec := json.NewDecoder(f)
	for {
		var ev sseEvent
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				break
			}
			break // tolerate partial reads
		}
		payload, err := encodeSSEEvent(ev)
		if err != nil {
			continue
		}
		if _, err := w.Write(payload); err != nil {
			return
		}
		flusher.Flush()
	}
	// Replay terminator so clients can distinguish "done backfilling"
	// from "stream still alive".
	fmt.Fprintf(w, ": replay-complete\n\n")
	flusher.Flush()
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
