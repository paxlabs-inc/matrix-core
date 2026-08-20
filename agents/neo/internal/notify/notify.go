// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package notify is Automatrix's pluggable out-of-app completion ping.
//
// When Neo finishes a proactive (Automatrix) task it writes a durable in-app
// record (the canonical, replayable surface) and ALSO tries to ping the user
// out-of-app through a Notifier. The Notifier is the one genuinely new outbound
// seam; everything else rides existing substrate.
//
// Backends:
//
//   - ntfy (default): POST ${NTFY_SERVER}/${topic} with Title/Click headers.
//     The topic is derived deterministically from the agent DID (per-user and
//     unguessable) or set explicitly via config (NTFY_TOPIC). No key management,
//     works out-of-app, has first-party mobile/desktop apps.
//   - Apprise (optional fan-out): an Apprise API server (APPRISE_URL, http(s))
//     or the local `apprise` CLI (any non-http target, e.g. tgram://…), letting
//     the user route the ping to Telegram/Discord/email/Pushover/etc.
//   - noop: the fallback when nothing is configured. The durable in-app record
//     still happens, so a result is never lost.
//
// Honesty + hot-path discipline: external delivery is BEST-EFFORT and NEVER
// blocks the agent loop (use Deliver, which fires on a background goroutine). A
// failed external send is logged honestly and dropped — it is never reported to
// the user as delivered, while the in-app record remains the canonical surface.
// No secrets, tokens, or keys are stored or logged by this package.
package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"centra/executor/tool"
)

// Notification is a single outbound ping. It carries the RESULT, not the
// protocol — no Chronos/alarm/marker/Neocortex jargon (the consumer-product rule).
type Notification struct {
	Title string // short headline, e.g. the result summary
	Body  string // the human-readable result
	URL   string // optional deep link the ping opens (ntfy Click / Apprise)
}

// Notifier delivers a Notification to an external channel. A backend returns an
// error on a failed send; callers that must not block (the agent loop) should
// route through Deliver rather than calling Notify directly.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Config selects and wires the external backend(s). All fields are optional;
// an empty Config yields a noop Notifier.
type Config struct {
	// NtfyServer is the ntfy base URL (default https://ntfy.sh when blank but a
	// topic is available). NtfyTopic is the explicit topic; when blank it is
	// derived deterministically from AgentDID. ntfy is disabled (skipped) when
	// neither an explicit topic nor a DID is available.
	NtfyServer string
	NtfyTopic  string
	AgentDID   string

	// AppriseURL enables the optional Apprise fan-out: an http(s) value targets
	// an Apprise API server; any other value (e.g. tgram://…, comma-separated
	// apprise service URLs) is delivered via the local `apprise` CLI.
	AppriseURL string

	// HTTPClient overrides the default client (tests inject an httptest client).
	HTTPClient *http.Client
}

// DefaultNtfyServer is the public ntfy server used when none is configured.
const DefaultNtfyServer = "https://ntfy.sh"

// deliverTimeout bounds a single best-effort external send.
const deliverTimeout = 10 * time.Second

// DeriveTopic derives a deterministic, per-user, unguessable ntfy topic from
// the agent DID. Returns "" for a blank DID (ntfy then has no topic and is
// skipped). The DID itself is never exposed: only an opaque hash prefix.
func DeriveTopic(did string) string {
	did = strings.TrimSpace(did)
	if did == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(did))
	return "neo-" + hex.EncodeToString(sum[:8])
}

// New builds a Notifier from cfg. ntfy (default) is added whenever a topic can
// be determined; Apprise (optional) is added when AppriseURL is set. With both,
// the returned Notifier fans out to each. With neither, it is a noop.
func New(cfg Config) Notifier {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: deliverTimeout}
	}

	var backends []Notifier

	server := strings.TrimSpace(cfg.NtfyServer)
	if server == "" {
		server = DefaultNtfyServer
	}
	topic := strings.TrimSpace(cfg.NtfyTopic)
	if topic == "" {
		topic = DeriveTopic(cfg.AgentDID)
	}
	if topic != "" {
		backends = append(backends, &ntfyBackend{
			server: strings.TrimRight(server, "/"),
			topic:  topic,
			client: client,
		})
	}

	if target := strings.TrimSpace(cfg.AppriseURL); target != "" {
		backends = append(backends, &appriseBackend{
			target: target,
			client: client,
		})
	}

	switch len(backends) {
	case 0:
		return noopBackend{}
	case 1:
		return backends[0]
	default:
		return multiBackend(backends)
	}
}

// Deliver sends the notification BEST-EFFORT on a background goroutine. It never
// blocks the caller and never returns an error: a failed external send is logged
// honestly and dropped (the durable in-app record remains the canonical
// surface). A nil notifier is a safe no-op.
func Deliver(n Notifier, note Notification) {
	if n == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
		defer cancel()
		if err := n.Notify(ctx, note); err != nil {
			fmt.Fprintf(os.Stderr, "neo/notify: external send failed (in-app record retained): %v\n", err)
		}
	}()
}

// noopBackend is the fallback when nothing is configured. The durable in-app
// record still happens upstream, so the result is never lost.
type noopBackend struct{}

func (noopBackend) Notify(context.Context, Notification) error { return nil }

// ntfyBackend posts to ${server}/${topic} with Title/Click headers — the
// documented ntfy publish contract.
type ntfyBackend struct {
	server string
	topic  string
	client *http.Client
}

func (b *ntfyBackend) Notify(ctx context.Context, n Notification) error {
	url := b.server + "/" + b.topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(n.Body))
	if err != nil {
		return err
	}
	if n.Title != "" {
		req.Header.Set("Title", n.Title)
	}
	if n.URL != "" {
		req.Header.Set("Click", n.URL)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// appriseBackend routes to an Apprise API server (http(s) target) or the local
// `apprise` CLI (any other target) for fan-out to many services.
type appriseBackend struct {
	target string
	client *http.Client
}

func (b *appriseBackend) Notify(ctx context.Context, n Notification) error {
	if isHTTPURL(b.target) {
		return b.notifyHTTP(ctx, n)
	}
	return b.notifyCLI(ctx, n)
}

// notifyHTTP posts {title, body} JSON to an Apprise API server endpoint. The
// service URLs themselves live server-side (a stored config key), so no secret
// is sent or logged from here.
func (b *appriseBackend) notifyHTTP(ctx context.Context, n Notification) error {
	payload, err := json.Marshal(struct {
		Title string `json:"title,omitempty"`
		Body  string `json:"body"`
	}{Title: n.Title, Body: n.Body})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.target, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("apprise: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// notifyCLI fans out through the local `apprise` binary. The target may be a
// comma-separated list of apprise service URLs. When the binary is absent the
// send fails honestly (logged by Deliver, never reported as delivered).
func (b *appriseBackend) notifyCLI(ctx context.Context, n Notification) error {
	args := []string{"-t", n.Title, "-b", n.Body}
	for _, u := range strings.Split(b.target, ",") {
		if u = strings.TrimSpace(u); u != "" {
			args = append(args, u)
		}
	}
	cmd := exec.CommandContext(ctx, "apprise", args...)
	cmd.Env = tool.AgentEnvironment(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apprise cli: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// multiBackend fans a notification out to every backend, attempting all even
// when one fails and joining their errors (best-effort fan-out).
type multiBackend []Notifier

func (m multiBackend) Notify(ctx context.Context, n Notification) error {
	var errs []error
	for _, b := range m {
		if err := b.Notify(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// isHTTPURL reports whether target is an http(s) URL (the Apprise API server
// path) rather than an apprise service URL handled by the CLI.
func isHTTPURL(target string) bool {
	t := strings.ToLower(strings.TrimSpace(target))
	return strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://")
}
