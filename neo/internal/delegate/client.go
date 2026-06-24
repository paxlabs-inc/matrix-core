// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package delegate is the core_execute bridge: it hands a prose intent to the
// MCL pipeline (the rigorous, replayable "smart computer") over the daemon's
// async HTTP API, services in-walk approval gates inline (so the user approves
// any spend in conversation), and returns the verifiable outcome.
//
// Contract (executor/cmd/mcl-execute):
//
//	POST /messages/async {prose}                  -> {intent_id}
//	GET  /intents/{id}/gates                       -> {pending:[{node_id,question,options}]}
//	POST /intents/{id}/gates/{nid}/answer {approved,answer}
//	GET  /messages/async/{id}                       -> {status, result:{answer}, error, clarify}
//
// Gate handling rides the daemon's SSE event stream (GET /events?intent_id=):
// gate.invoked → answer pending gates, intent.attest/fail/cancel → terminal.
// A poller is retained as an explicit safety net + fallback: it covers the
// dispatch→subscribe race and silent stream drops, and becomes the sole driver
// once bounded SSE reconnect attempts exhaust (stream unavailable).
package delegate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Approver is asked to approve an in-walk gate (typically a spend). nodeID is
// the gate's node identifier (so a UI can route the user's answer back to the
// exact gate). It returns whether to approve and an optional free-text answer.
// A nil Approver denies every gate (safe default — no unattended spends).
type Approver func(ctx context.Context, nodeID, question string, options []string) (approved bool, answer string)

// Options configures a Client.
type Options struct {
	BaseURL      string // daemon base URL, e.g. http://127.0.0.1:8080
	Token        string // bearer for the daemon (dev: any; prod: caller JWT)
	CallerDID    string // X-Caller-DID
	CallerWallet string // X-Caller-Wallet
	Skill        string // optional skill URI to pin; empty = daemon selects
	Approver     Approver
	Notify       func(string) // optional status sink (transparency)
	Timeout      time.Duration
	PollInterval time.Duration
	MaxWait      time.Duration
}

// Client delegates prose intents to the MCL daemon.
type Client struct {
	base    string
	http    *http.Client
	token   string
	did     string
	wallet  string
	skill   string
	approve Approver
	notify  func(string)

	pollEvery time.Duration
	maxWait   time.Duration

	// streamHTTP is an http.Client with no overall Timeout for the long-lived
	// SSE subscription (c.http.Timeout would kill the stream). It shares
	// c.http's transport; cancellation is via the request context.
	streamHTTP *http.Client
	// sseMaxReconnect bounds reconnection attempts after a stream drop before
	// the poller becomes the sole driver (fallback).
	sseMaxReconnect int
	// sseBackoff is the delay between reconnect attempts.
	sseBackoff time.Duration
}

// New builds a delegation client.
func New(o Options) *Client {
	to := o.Timeout
	if to == 0 {
		to = 30 * time.Second
	}
	poll := o.PollInterval
	if poll == 0 {
		poll = 1500 * time.Millisecond
	}
	mw := o.MaxWait
	if mw == 0 {
		mw = 30 * time.Minute
	}
	notify := o.Notify
	if notify == nil {
		notify = func(string) {}
	}
	return &Client{
		base:      strings.TrimRight(o.BaseURL, "/"),
		http:      &http.Client{Timeout: to},
		token:     o.Token,
		did:       o.CallerDID,
		wallet:    o.CallerWallet,
		skill:     o.Skill,
		approve:   o.Approver,
		notify:    notify,
		pollEvery: poll,
		maxWait:   mw,

		streamHTTP:      &http.Client{Transport: nil}, // DefaultTransport; no Timeout for SSE
		sseMaxReconnect: 3,
		sseBackoff:      100 * time.Millisecond,
	}
}

// Run submits the intent and blocks until it terminates, servicing approval
// gates inline. It subscribes to the daemon's SSE event stream for the intent
// and drives gate/terminal transitions from events (sub-ms latency). A poller
// is retained as an explicit safety net + fallback: it covers the
// dispatch→subscribe race and silent stream drops, and becomes the sole driver
// once the SSE stream's bounded reconnect attempts exhaust. The shared
// answered set (touched only from this goroutine) guarantees a gate is never
// answered twice, even when an event and a poll tick both observe it.
// Returns the deliverable answer, or an error describing the failure /
// clarification needed / timeout (which the model relays).
func (c *Client) Run(ctx context.Context, prose string) (string, error) {
	c.notify("routing this through the secure execution path — I'll ask before anything spends.")

	id, err := c.submit(ctx, prose)
	if err != nil {
		return "", fmt.Errorf("could not reach the secure execution path: %w", err)
	}

	answered := map[string]bool{}
	deadline := time.Now().Add(c.maxWait)

	// SSE subscription is the primary driver. It is cancelled when Run returns
	// so the stream goroutine never outlives the call.
	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()
	evCh, failCh := c.subscribeEvents(sseCtx, id)

	// The poller is the explicit fallback: it keeps ticking (covering races and
	// the SSE-unavailable case) regardless of the stream's health.
	tick := time.NewTicker(c.pollEvery)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-failCh:
			// Bounded SSE reconnect exhausted; the poller is now the sole driver.
			failCh = nil // don't select again
			c.notify("(live event stream unavailable — falling back to polling)")
		case ev, ok := <-evCh:
			if !ok {
				// Stream ended (run closed or reconnects exhausted); poller
				// carries on. Nil the channel so we never busy-loop on it.
				evCh = nil
				continue
			}
			switch {
			case isTerminalEvent(ev.Type):
				// An event signalled a terminal transition; confirm via the
				// authoritative status endpoint. If status hasn't caught up
				// (event raced ahead), keep going — a later event or poll tick
				// will close it out.
				if ans, rerr, done := c.checkTerminal(ctx, id); done {
					return ans, rerr
				}
			case isGateEvent(ev.Type):
				c.handleGates(ctx, id, answered)
			}
		case <-tick.C:
			c.handleGates(ctx, id, answered)
			if ans, rerr, done := c.checkTerminal(ctx, id); done {
				return ans, rerr
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("the delegated task did not finish within %s", c.maxWait)
		}
	}
}

// sseEvent mirrors the daemon's wire-format event (executor sseEvent / Neo
// server.Event): {seq,ts,phase,type,fields}. Only Type is used for routing.
type sseEvent struct {
	Seq    uint64                 `json:"seq"`
	TS     string                 `json:"ts"`
	Phase  string                 `json:"phase"`
	Type   string                 `json:"type"`
	Fields map[string]interface{} `json:"fields,omitempty"`
}

// isTerminalEvent reports whether an event type signals a terminal lifecycle
// transition (completed via attest, failed, or cancelled). These mirror the
// daemon's lifecycleAdvance terminal kinds.
func isTerminalEvent(t string) bool {
	switch t {
	case "intent.attest", "intent.fail", "intent.cancel":
		return true
	}
	return false
}

// isGateEvent reports whether an event type relates to an approval gate (the
// daemon emits gate.invoked when a gate becomes pending).
func isGateEvent(t string) bool {
	return strings.HasPrefix(t, "gate.")
}

// classifyTerminal maps an authoritative status response to a result/error when
// the status is terminal (or a clarification is required). done is false when
// the run is still in flight.
func classifyTerminal(st *statusResp) (string, error, bool) {
	switch st.Status {
	case "completed":
		ans := ""
		if st.Result != nil {
			ans = strings.TrimSpace(st.Result.Answer)
		}
		if ans == "" {
			ans = "Done (the secure pipeline completed but returned no text)."
		}
		return ans, nil, true
	case "failed":
		if strings.TrimSpace(st.Error) != "" {
			return "", fmt.Errorf("the secure pipeline failed: %s", st.Error), true
		}
		return "", fmt.Errorf("the secure pipeline failed"), true
	case "cancelled":
		return "", fmt.Errorf("the delegated task was cancelled"), true
	}
	if len(st.Clarify) > 0 {
		return "", fmt.Errorf("the task needs more detail before it can run: %s", clarifyText(st.Clarify)), true
	}
	return "", nil, false
}

// checkTerminal fetches the authoritative status and classifies it.
func (c *Client) checkTerminal(ctx context.Context, intentID string) (string, error, bool) {
	st, err := c.status(ctx, intentID)
	if err != nil {
		return "", nil, false
	}
	return classifyTerminal(st)
}

// subscribeEvents opens a goroutine that subscribes to the daemon's
// GET /events?intent_id=<id> SSE stream and delivers parsed events on evCh.
// On a stream error it attempts a bounded number of reconnects (sseBackoff
// between each); once exhausted it signals failCh once and closes evCh. evCh
// is also closed on a clean stream end or context cancellation. The poller is
// expected to keep driving regardless, so a closed evCh is never fatal.
func (c *Client) subscribeEvents(ctx context.Context, intentID string) (<-chan sseEvent, <-chan struct{}) {
	evCh := make(chan sseEvent, 16)
	failCh := make(chan struct{}, 1)
	go func() {
		defer close(evCh)
		for attempt := 0; ; attempt++ {
			err := c.readEventStream(ctx, intentID, evCh)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				// Clean end (the daemon closed the run's stream). The poller
				// will confirm the terminal status.
				return
			}
			// Stream error: bounded reconnect, then hand off to the poller.
			if attempt >= c.sseMaxReconnect {
				select {
				case failCh <- struct{}{}:
				default:
				}
				return
			}
			select {
			case <-time.After(c.sseBackoff):
			case <-ctx.Done():
				return
			}
		}
	}()
	return evCh, failCh
}

// readEventStream performs one SSE subscription, forwarding parsed events to
// evCh until the stream ends, errors, or ctx is cancelled.
func (c *Client) readEventStream(ctx context.Context, intentID string, evCh chan<- sseEvent) error {
	u := c.base + "/events?intent_id=" + url.QueryEscape(intentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.did != "" {
		req.Header.Set("X-Caller-DID", c.did)
	}
	if c.wallet != "" {
		req.Header.Set("X-Caller-Wallet", c.wallet)
	}
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("events http %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20) // 1 MiB max line
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] == ':' {
			// Blank separator or comment (heartbeat / ": connected").
			continue
		}
		var payload []byte
		switch {
		case bytes.HasPrefix(line, []byte("data: ")):
			payload = bytes.TrimSpace(line[len("data: "):])
		case bytes.HasPrefix(line, []byte("data:")):
			payload = bytes.TrimSpace(line[len("data:"):])
		default:
			continue
		}
		if len(payload) == 0 {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue // skip malformed frame rather than abort the stream
		}
		select {
		case evCh <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return scanner.Err()
}

func (c *Client) submit(ctx context.Context, prose string) (string, error) {
	body := map[string]string{"prose": prose}
	if c.skill != "" {
		body["skill"] = c.skill
	}
	var out struct {
		IntentID string `json:"intent_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/messages/async", body, &out); err != nil {
		return "", err
	}
	if out.IntentID == "" {
		return "", fmt.Errorf("daemon did not return an intent_id")
	}
	return out.IntentID, nil
}

type pendingGateDTO struct {
	NodeID   string   `json:"node_id"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

func (c *Client) handleGates(ctx context.Context, intentID string, answered map[string]bool) {
	var out struct {
		Pending []pendingGateDTO `json:"pending"`
	}
	if err := c.do(ctx, http.MethodGet, "/intents/"+url.PathEscape(intentID)+"/gates", nil, &out); err != nil {
		return
	}
	for _, g := range out.Pending {
		if g.NodeID == "" || answered[g.NodeID] {
			continue
		}
		approved, answer := false, ""
		if c.approve != nil {
			approved, answer = c.approve(ctx, g.NodeID, g.Question, g.Options)
		}
		_ = c.answerGate(ctx, intentID, g.NodeID, approved, answer)
		answered[g.NodeID] = true
	}
}

func (c *Client) answerGate(ctx context.Context, intentID, nodeID string, approved bool, answer string) error {
	body := map[string]interface{}{"approved": approved}
	if answer != "" {
		body["answer"] = answer
	}
	path := "/intents/" + url.PathEscape(intentID) + "/gates/" + url.PathEscape(nodeID) + "/answer"
	return c.do(ctx, http.MethodPost, path, body, nil)
}

type statusResp struct {
	Status  string          `json:"status"`
	Error   string          `json:"error"`
	Result  *resultBody     `json:"result"`
	Clarify json.RawMessage `json:"clarify"`
}

type resultBody struct {
	Answer string `json:"answer"`
	Status string `json:"status"`
}

func (c *Client) status(ctx context.Context, intentID string) (*statusResp, error) {
	var out statusResp
	if err := c.do(ctx, http.MethodGet, "/messages/async/"+url.PathEscape(intentID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out interface{}) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.did != "" {
		req.Header.Set("X-Caller-DID", c.did)
	}
	if c.wallet != "" {
		req.Header.Set("X-Caller-Wallet", c.wallet)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func clarifyText(raw json.RawMessage) string {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err == nil {
		if q, ok := m["question"].(string); ok && q != "" {
			return q
		}
	}
	return truncate(string(raw), 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
