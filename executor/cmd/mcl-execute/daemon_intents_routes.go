// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_intents_routes.go — full /intents/* route surface for the
// frontend client (sess#27).
//
// Route map:
//
//   GET  /intents                         list summaries (paginated)
//   GET  /intents/:id                     EXISTING — full envelope chain
//   GET  /intents/:id/summary             compact lifecycle + counters
//   GET  /intents/:id/lifecycle           lifecycle history per envelope
//   GET  /intents/:id/plan                parsed plan tree from plan.proposed
//   GET  /intents/:id/transcript          replay every transcript event (jsonl)
//   GET  /intents/:id/attestation         parsed intent.attest body
//   GET  /intents/:id/envelopes/:seq      single envelope JSON
//   GET  /intents/:id/replay-roots        pre/post overall_root + match flag
//   GET  /intents/:id/gates               pending gates for this intent
//   POST /intents/:id/gates/:nid/answer   answer a pending gate
//   POST /intents/:id/cancel              cancel an in-flight intent
//   POST /intents/:id/correct             D8 typed-patch correction (stub)
//   POST /messages/async                  async-mode start; returns 202 + intent_id
//   GET  /messages/async/:id              async-mode poll; returns terminal result
//
// The legacy GET /intents/:id route persists (existing handleIntents
// in daemon_server.go); we extend it with the sub-paths above. The
// sub-router lives in handleIntentsAndSubpaths and dispatches on the
// trailing path components, falling through to the legacy chain
// handler when no sub-path matches.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"matrix/mcl/envelope"
)

// handleIntentsList serves GET /intents (the list endpoint).
//
// Query params:
//
//	?cursor=<opaque>      pagination cursor (mtime-keyed, descending)
//	?limit=<int>          page size (default 20, cap 200)
//	?state=<name>         optional terminal-state filter
//	                      (drafting|proposed|clarifying|accepted|
//	                       executing|completed|failed|cancelled)
func (d *daemonState) handleIntentsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	cur, limit, ok := pageParams(w, r, 20, 200)
	if !ok {
		return
	}
	stateFilter := queryString(r, "state", "")
	items, next, total, err := listIntentSummaries(d, cur, limit, stateFilter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "list intents: " + err.Error(),
		})
		return
	}
	writePaged(w, items, next, total)
}

// handleIntentsRouter dispatches every GET/POST /intents/* request.
// Replaces the legacy handleIntents handler so we own the path
// matching for the new sub-routes.
func (d *daemonState) handleIntentsRouter(t *transcript) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/intents")
		path = strings.TrimPrefix(path, "/")
		if path == "" {
			d.handleIntentsList(w, r)
			return
		}
		// Split on first "/" to peel off the intent_id; rest is the
		// sub-path the route table dispatches on.
		parts := strings.SplitN(path, "/", 2)
		intentID := parts[0]
		if intentID == "" || strings.ContainsAny(intentID, "\\") {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "intent id required",
			})
			return
		}
		sub := ""
		if len(parts) == 2 {
			sub = parts[1]
		}
		switch {
		case sub == "":
			d.handleIntentChain(w, r, intentID)
		case sub == "summary":
			d.handleIntentSummary(w, r, intentID)
		case sub == "lifecycle":
			d.handleIntentLifecycle(w, r, intentID)
		case sub == "plan":
			d.handleIntentPlan(w, r, intentID)
		case sub == "transcript":
			d.handleIntentTranscript(w, r, intentID)
		case sub == "attestation":
			d.handleIntentAttestation(w, r, intentID)
		case sub == "replay-roots":
			d.handleIntentReplayRoots(w, r, intentID)
		case sub == "cancel":
			d.handleIntentCancel(w, r, intentID, t)
		case sub == "correct":
			d.handleIntentCorrect(w, r, intentID, t)
		case sub == "gates":
			d.handleIntentGates(w, r, intentID)
		case strings.HasPrefix(sub, "gates/"):
			rest := strings.TrimPrefix(sub, "gates/")
			rest = strings.TrimSuffix(rest, "/answer")
			if rest == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "node id required",
				})
				return
			}
			d.handleIntentGateAnswer(w, r, intentID, rest)
		case strings.HasPrefix(sub, "envelopes/"):
			rest := strings.TrimPrefix(sub, "envelopes/")
			seq, err := strconv.Atoi(rest)
			if err != nil || seq <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "invalid seq: " + rest,
				})
				return
			}
			d.handleIntentEnvelope(w, r, intentID, seq)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "no route for /intents/" + intentID + "/" + sub,
			})
		}
	}
}

// handleIntentChain is the production replacement for the legacy
// handleIntents in daemon_server.go. Returns the full envelope chain.
func (d *daemonState) handleIntentChain(w http.ResponseWriter, r *http.Request, intentID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	dir := filepath.Join(d.journalDir, intentID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "intent not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("read intent dir: %v", err),
		})
		return
	}
	files := make([]intentEnvelopeFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		seq, kind := parseEnvelopeFilename(e.Name())
		body, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		files = append(files, intentEnvelopeFile{
			Seq:      seq,
			Filename: e.Name(),
			Kind:     kind,
			Envelope: json.RawMessage(body),
		})
	}
	sortByEnvelopeSeq(files)
	writeJSON(w, http.StatusOK, intentResponse{
		IntentID:  intentID,
		Path:      dir,
		Envelopes: files,
	})
}

// handleIntentSummary serves GET /intents/:id/summary.
func (d *daemonState) handleIntentSummary(w http.ResponseWriter, r *http.Request, intentID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	dir := filepath.Join(d.journalDir, intentID)
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "intent not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	sum := d.indexCache.summaryFor(intentID, dir, info.ModTime())
	if sum == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "intent has no envelopes"})
		return
	}
	// Decorate with async-job state (when present) so the frontend
	// can distinguish "in flight" from "queued" without a separate
	// poll.
	resp := struct {
		intentSummary
		AsyncStatus string `json:"async_status,omitempty"`
		PendingGate int    `json:"pending_gates"`
	}{intentSummary: *sum}
	if d.asyncReg != nil {
		if j := d.asyncReg.Get(intentID); j != nil {
			resp.AsyncStatus = string(j.Status)
		}
	}
	if d.gateBroker != nil {
		resp.PendingGate = len(d.gateBroker.ListByIntent(intentID))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleIntentLifecycle serves GET /intents/:id/lifecycle.
//
// Returns one entry per state transition derived from the envelope
// kind sequence. Mirrors the lifecycle.History() shape but built from
// the on-disk journal so the daemon doesn't need to keep the machine
// in memory across HTTP requests.
func (d *daemonState) handleIntentLifecycle(w http.ResponseWriter, r *http.Request, intentID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	dir := filepath.Join(d.journalDir, intentID)
	envelopes, err := readSortedEnvelopes(dir)
	if err != nil {
		writeJSON(w, statusFromFsErr(err), map[string]string{"error": err.Error()})
		return
	}
	type transition struct {
		Seq   int    `json:"seq"`
		At    string `json:"at"`
		Kind  string `json:"kind"`
		From  string `json:"from"`
		To    string `json:"to"`
		EnvID string `json:"envelope_id,omitempty"`
	}
	out := make([]transition, 0, len(envelopes))
	prev := "drafting"
	for _, env := range envelopes {
		nextState := lifecycleAdvance(prev, env.kind)
		if nextState == prev {
			continue
		}
		out = append(out, transition{
			Seq:   env.seq,
			At:    env.body.At,
			Kind:  env.kind,
			From:  prev,
			To:    nextState,
			EnvID: env.body.ID,
		})
		prev = nextState
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"intent_id":   intentID,
		"final_state": prev,
		"transitions": out,
	})
}

// handleIntentPlan serves GET /intents/:id/plan.
func (d *daemonState) handleIntentPlan(w http.ResponseWriter, r *http.Request, intentID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	dir := filepath.Join(d.journalDir, intentID)
	envelopes, err := readSortedEnvelopes(dir)
	if err != nil {
		writeJSON(w, statusFromFsErr(err), map[string]string{"error": err.Error()})
		return
	}
	for _, env := range envelopes {
		if env.kind != "plan-proposed" {
			continue
		}
		var body envelope.PlanProposedBody
		if err := env.body.DecodeBody(&body); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "decode plan body: " + err.Error(),
			})
			return
		}
		// PlanJSON is the canonical-JSON ir.PlanTree; pass through
		// without re-marshaling so the frontend sees byte-equal bytes.
		var planObj interface{}
		_ = json.Unmarshal(body.PlanJSON, &planObj)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"intent_id":   intentID,
			"envelope_id": env.body.ID,
			"plan":        planObj,
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": "no plan.proposed envelope for this intent",
	})
}

// handleIntentTranscript serves GET /intents/:id/transcript.
//
// Streams the per-message transcript JSONL file (produced by
// daemon_pipeline.go runMessage) as a JSON array so single-shot
// consumers can decode directly without line-parsing.
func (d *daemonState) handleIntentTranscript(w http.ResponseWriter, r *http.Request, intentID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
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
	dec := json.NewDecoder(f)
	events := make([]map[string]interface{}, 0, 128)
	for {
		var ev map[string]interface{}
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				break
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "decode transcript: " + err.Error(),
			})
			return
		}
		events = append(events, ev)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"intent_id":   intentID,
		"event_count": len(events),
		"events":      events,
	})
}

// handleIntentAttestation serves GET /intents/:id/attestation.
//
// Returns the parsed intent.attest body when present, OR the parsed
// intent.fail body when failed, OR 404 when terminal envelope absent.
func (d *daemonState) handleIntentAttestation(w http.ResponseWriter, r *http.Request, intentID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	dir := filepath.Join(d.journalDir, intentID)
	envelopes, err := readSortedEnvelopes(dir)
	if err != nil {
		writeJSON(w, statusFromFsErr(err), map[string]string{"error": err.Error()})
		return
	}
	for i := len(envelopes) - 1; i >= 0; i-- {
		env := envelopes[i]
		switch env.kind {
		case "intent-attest":
			var body envelope.IntentAttestBody
			if err := env.body.DecodeBody(&body); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "decode attest: " + err.Error(),
				})
				return
			}
			var ev interface{}
			_ = json.Unmarshal(body.EvidenceJSON, &ev)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"intent_id":   intentID,
				"envelope_id": env.body.ID,
				"kind":        "intent.attest",
				"outcome":     body.Outcome,
				"completed":   body.CompletedAt,
				"cited_uris":  body.CitedURIs,
				"evidence":    ev,
				"anchor_tx":   body.AnchorTx,
			})
			return
		case "intent-fail":
			var body envelope.IntentFailBody
			if err := env.body.DecodeBody(&body); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "decode fail: " + err.Error(),
				})
				return
			}
			var ev interface{}
			_ = json.Unmarshal(body.EvidenceJSON, &ev)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"intent_id":    intentID,
				"envelope_id":  env.body.ID,
				"kind":         "intent.fail",
				"reason":       body.Reason,
				"message":      body.Message,
				"failed":       body.FailedAt,
				"partial_uris": body.PartialURIs,
				"evidence":     ev,
			})
			return
		case "intent-cancel":
			var body envelope.IntentCancelBody
			if err := env.body.DecodeBody(&body); err == nil {
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"intent_id":   intentID,
					"envelope_id": env.body.ID,
					"kind":        "intent.cancel",
					"reason":      body.Reason,
					"cancelled":   body.CancelledAt,
				})
				return
			}
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": "no terminal envelope (attest/fail/cancel) for this intent",
	})
}

// handleIntentReplayRoots serves GET /intents/:id/replay-roots.
//
// Pulls pre/post overall_root from the transcript's walk.cortex.pre
// and walk.cortex.post events. Returns match=true iff both roots are
// non-empty AND equal.
func (d *daemonState) handleIntentReplayRoots(w http.ResponseWriter, r *http.Request, intentID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
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
	dec := json.NewDecoder(f)
	var pre, post string
	for {
		var ev struct {
			Type   string                 `json:"type"`
			Fields map[string]interface{} `json:"fields"`
		}
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				break
			}
			break // tolerate partial reads
		}
		switch ev.Type {
		case "walk.cortex.pre":
			if v, ok := ev.Fields["overall_root"].(string); ok {
				pre = v
			}
		case "walk.cortex.post":
			if v, ok := ev.Fields["overall_root"].(string); ok {
				post = v
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"intent_id":        intentID,
		"pre_replay_root":  pre,
		"post_replay_root": post,
		"changed":          pre != "" && post != "" && pre != post,
		"match":            pre != "" && post != "" && pre == post,
	})
}

// handleIntentEnvelope serves GET /intents/:id/envelopes/:seq.
func (d *daemonState) handleIntentEnvelope(w http.ResponseWriter, r *http.Request, intentID string, seq int) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	dir := filepath.Join(d.journalDir, intentID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, statusFromFsErr(err), map[string]string{"error": err.Error()})
		return
	}
	prefix := fmt.Sprintf("%04d-", seq)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": rerr.Error()})
			return
		}
		var envBody json.RawMessage = json.RawMessage(body)
		_, kind := parseEnvelopeFilename(e.Name())
		writeJSON(w, http.StatusOK, intentEnvelopeFile{
			Seq:      seq,
			Filename: e.Name(),
			Kind:     kind,
			Envelope: envBody,
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": fmt.Sprintf("envelope seq %d not found for intent %s", seq, intentID),
	})
}

// handleIntentGates serves GET /intents/:id/gates.
func (d *daemonState) handleIntentGates(w http.ResponseWriter, r *http.Request, intentID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.gateBroker == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"intent_id": intentID,
			"pending":   []*pendingGate{},
		})
		return
	}
	pending := d.gateBroker.ListByIntent(intentID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"intent_id": intentID,
		"pending":   pending,
	})
}

// handleIntentGateAnswer serves POST /intents/:id/gates/:nid/answer.
func (d *daemonState) handleIntentGateAnswer(w http.ResponseWriter, r *http.Request, intentID, nodeID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.gateBroker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "gate broker not enabled (sync-only daemon)",
		})
		return
	}
	var ans gateAnswer
	if err := json.NewDecoder(r.Body).Decode(&ans); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "decode body: " + err.Error(),
		})
		return
	}
	if err := d.gateBroker.Answer(intentID, nodeID, ans); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":    "answered",
		"intent_id": intentID,
		"node_id":   nodeID,
	})
}

// handleIntentCancel serves POST /intents/:id/cancel.
func (d *daemonState) handleIntentCancel(w http.ResponseWriter, r *http.Request, intentID string, t *transcript) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.asyncReg == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "async registry not enabled (sync-only daemon)",
		})
		return
	}
	var req struct {
		Reason string `json:"reason,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := d.asyncReg.Cancel(intentID); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	t.Event("intent.cancel.requested", "lifecycle", map[string]interface{}{
		"intent_id": intentID,
		"reason":    req.Reason,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":    "cancellation requested",
		"intent_id": intentID,
	})
}

// handleIntentCorrect serves POST /intents/:id/correct.
//
// v1 stub: validates the request shape and records the correction
// envelope but does not yet rewind the executing walker. The full
// material/non-material classification path (D9 + walker correction
// inbox) lands when the executor's correction inbox is wired through
// the async registry. The route is in place so the frontend can
// build against it.
func (d *daemonState) handleIntentCorrect(w http.ResponseWriter, r *http.Request, intentID string, t *transcript) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	var req struct {
		Target  string          `json:"target"` // "intent" or "plan"
		Patches json.RawMessage `json:"patches"`
		Reason  string          `json:"reason,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "decode body: " + err.Error(),
		})
		return
	}
	if req.Target != "intent" && req.Target != "plan" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "target must be 'intent' or 'plan'",
		})
		return
	}
	if len(req.Patches) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "patches required (RFC 6902 JSON Patch array)",
		})
		return
	}
	t.Event("intent.correct.received", "lifecycle", map[string]interface{}{
		"intent_id":   intentID,
		"target":      req.Target,
		"patch_bytes": len(req.Patches),
		"reason":      req.Reason,
	})
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"intent_id": intentID,
		"target":    req.Target,
		"status":    "queued",
		"note":      "v1: correction recorded; live materiality classification + walker rewind lands in v1.1",
	})
}

// ---- async /messages routes ----

// handleMessagesAsyncStart serves POST /messages/async.
//
// Returns 202 + {intent_id} immediately and runs the pipeline in a
// goroutine. The frontend tracks progress via /events?intent_id=<id>
// and polls /messages/async/:id (or /intents/:id/summary) for the
// terminal result.
func (d *daemonState) handleMessagesAsyncStart(t *transcript) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		_, ok := d.requireAuthPolicy(w, r, authAny)
		if !ok {
			return
		}
		var req messageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "decode body: " + err.Error(),
			})
			return
		}
		if req.Prose == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "prose is required",
			})
			return
		}
		if req.SkillURI == "" {
			req.SkillURI = d.selectSkill(req.Prose, req.Verb)
		}
		if req.SkillURI == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "skill URI required (no daemon default configured)",
			})
			return
		}
		intentID := req.IntentID
		if intentID == "" {
			intentID = synthIntentID(req.Prose, req.Verb)
			req.IntentID = intentID
		}
		userID := userIDFromContext(r.Context())
		if d.asyncReg == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "async registry not enabled",
			})
			return
		}
		if _, err := d.asyncReg.CreateQueued(intentID, userID, req); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		go d.runAsyncMessage(intentID, req, t)
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"intent_id":   intentID,
			"status":      "queued",
			"events_url":  "/events?intent_id=" + intentID,
			"poll_url":    "/messages/async/" + intentID,
			"summary_url": "/intents/" + intentID + "/summary",
			"transcript":  "/intents/" + intentID + "/transcript",
		})
	}
}

// handleMessagesAsyncPoll serves GET /messages/async/:id.
func (d *daemonState) handleMessagesAsyncPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/messages/async/")
	id = strings.Trim(id, "/")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "intent id required"})
		return
	}
	if d.asyncReg == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "async registry not enabled"})
		return
	}
	job := d.asyncReg.Get(id)
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "async job not found"})
		return
	}
	resp := map[string]interface{}{
		"intent_id":  job.IntentID,
		"status":     job.Status,
		"created_at": job.CreatedAt,
	}
	if !job.StartedAt.IsZero() {
		resp["started_at"] = job.StartedAt
	}
	if !job.EndedAt.IsZero() {
		resp["ended_at"] = job.EndedAt
	}
	if job.Result != nil {
		resp["result"] = job.Result
	}
	if job.Error != "" {
		resp["error"] = job.Error
	}
	if job.clarify != nil {
		resp["clarify"] = job.clarify
	}
	writeJSON(w, http.StatusOK, resp)
}

// runAsyncMessage is the goroutine that runs one async job. It
// acquires d.busy (the cortex single-writer mutex), constructs an
// httpGateHandler keyed by intent_id, and threads the resulting
// goroutine context through runMessage.
func (d *daemonState) runAsyncMessage(intentID string, req messageRequest, t *transcript) {
	ctx, cancel := context.WithCancel(context.Background())
	d.asyncReg.MarkRunning(intentID, cancel)
	defer cancel()

	// Wait for the cortex single-writer mutex (queueing other async
	// jobs behind us). Concurrent /messages/async POSTs serialise
	// here so cortex doesn't see two writers.
	d.busy.Lock()
	defer d.busy.Unlock()

	// Re-check status post-acquire — Cancel may have fired while
	// we waited.
	if cur := d.asyncReg.Get(intentID); cur != nil && cur.Status == asyncCancelled {
		d.asyncReg.MarkResult(intentID, nil, fmt.Errorf("cancelled before start"), nil)
		return
	}

	d.asyncCurrentIntent.Store(intentID) // for httpGateHandler binding
	defer d.asyncCurrentIntent.Store("")

	// Gideon mode routes to the compiler-bypass runMessageDirect via
	// dispatchMessage; otherwise the legacy compile→walk runMessage runs.
	res, err := d.dispatchMessage(ctx, req)
	if err != nil {
		// clarify-required surfaces as error from runMessage; capture
		// it in the registry so the poll endpoint can return 422-ish
		// state.
		if cre, ok := err.(*clarifyRequiredError); ok {
			d.asyncReg.MarkResult(intentID, nil, nil, cre)
			return
		}
		d.asyncReg.MarkResult(intentID, nil, err, nil)
		return
	}
	d.asyncReg.MarkResult(intentID, res, nil, nil)
}

// ---- helpers ----

// envelopePair carries a parsed envelope with its sequence + kind for
// the handlers that need ordered iteration over the full chain.
type envelopePair struct {
	seq  int
	kind string
	body *rawEnvelopeJSON
}

// readSortedEnvelopes reads all envelope JSON files in an intent dir,
// sorted by seq ascending. Returns an error when the dir is missing.
func readSortedEnvelopes(dir string) ([]envelopePair, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]envelopePair, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		seq, kind := parseEnvelopeFilename(e.Name())
		body, rerr := readEnvelopeBody(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		out = append(out, envelopePair{
			seq:  seq,
			kind: kind,
			body: body,
		})
	}
	// Sort ascending by seq.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].seq > out[j].seq; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out, nil
}

func sortByEnvelopeSeq(in []intentEnvelopeFile) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1].Seq > in[j].Seq; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
}

func statusFromFsErr(err error) int {
	if os.IsNotExist(err) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// asyncQueueDeadline is a future safety hook so a queued async job
// never blocks more than this long waiting on busy. v1: 1h.
const asyncQueueDeadline = time.Hour

// Copyright © 2026 Paxlabs Inc. All rights reserved.
