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
// Gate handling is poll-based (no SSE dependency): each tick we answer any
// pending gates, then check terminal status.
package delegate

import (
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
	}
}

// Run submits the intent and blocks until it terminates, servicing approval
// gates inline. Returns the deliverable answer, or an error describing the
// failure / clarification needed / timeout (which the model relays).
func (c *Client) Run(ctx context.Context, prose string) (string, error) {
	c.notify("routing this through the secure execution path — I'll ask before anything spends.")

	id, err := c.submit(ctx, prose)
	if err != nil {
		return "", fmt.Errorf("could not reach the secure execution path: %w", err)
	}

	answered := map[string]bool{}
	deadline := time.Now().Add(c.maxWait)
	tick := time.NewTicker(c.pollEvery)
	defer tick.Stop()

	for {
		c.handleGates(ctx, id, answered)

		st, serr := c.status(ctx, id)
		if serr == nil {
			switch st.Status {
			case "completed":
				ans := ""
				if st.Result != nil {
					ans = strings.TrimSpace(st.Result.Answer)
				}
				if ans == "" {
					ans = "Done (the secure pipeline completed but returned no text)."
				}
				return ans, nil
			case "failed":
				if strings.TrimSpace(st.Error) != "" {
					return "", fmt.Errorf("the secure pipeline failed: %s", st.Error)
				}
				return "", fmt.Errorf("the secure pipeline failed")
			case "cancelled":
				return "", fmt.Errorf("the delegated task was cancelled")
			}
			if len(st.Clarify) > 0 {
				return "", fmt.Errorf("the task needs more detail before it can run: %s", clarifyText(st.Clarify))
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("the delegated task did not finish within %s", c.maxWait)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
		}
	}
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
