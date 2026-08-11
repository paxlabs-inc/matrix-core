// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// FailureKind classifies why a finance call could not be answered. The kind is
// what the UI and the agent branch on; Message is the plain-language line a user
// may read verbatim.
type FailureKind string

const (
	// FailureNotConfigured — the vendor key is absent from the router env. The
	// lane still boots and still answers; it says what is missing.
	FailureNotConfigured FailureKind = "not_configured"
	// FailureThrottled — the vendor refused for quota/rate reasons. Alpha
	// Vantage signals this with HTTP 200 and a Note/Information body, which is
	// exactly why it is detected before any parse.
	FailureThrottled FailureKind = "throttled"
	// FailureRateLimited — OUR per-user limit, not the vendor's.
	FailureRateLimited FailureKind = "rate_limited"
	// FailureUpstream — the vendor answered with an error status or an
	// unreadable body.
	FailureUpstream FailureKind = "upstream"
	// FailureTimeout — the vendor did not answer in time.
	FailureTimeout FailureKind = "timeout"
	// FailureNotFound — the vendor has nothing for this symbol/series.
	FailureNotFound FailureKind = "not_found"
	// FailureBadRequest — the caller asked for something malformed.
	FailureBadRequest FailureKind = "bad_request"
)

// Failure is the typed error every provider call returns. It never carries a
// URL or a key: Detail holds vendor text for logs, Message holds the line the
// user sees.
type Failure struct {
	Kind       FailureKind
	Provider   Provider
	Endpoint   string
	Status     int
	Message    string
	Detail     string
	RetryAfter time.Duration
}

func (f *Failure) Error() string {
	if f == nil {
		return "<nil finance failure>"
	}
	if f.Detail != "" {
		return fmt.Sprintf("finance %s %s: %s (%s)", f.Provider, f.Endpoint, f.Message, f.Detail)
	}
	return fmt.Sprintf("finance %s %s: %s", f.Provider, f.Endpoint, f.Message)
}

// FailureOf unwraps err to its *Failure, or nil when err is not one.
func FailureOf(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	return nil
}

// KindOf reports err's FailureKind, or FailureUpstream for a plain error.
func KindOf(err error) FailureKind {
	if f := FailureOf(err); f != nil {
		return f.Kind
	}
	if err == nil {
		return ""
	}
	return FailureUpstream
}

// Retryable reports whether trying the OTHER provider (or the same one later)
// could plausibly help. A malformed request or a genuine "no such symbol" is
// not worth a second call.
func (f *Failure) Retryable() bool {
	if f == nil {
		return false
	}
	switch f.Kind {
	case FailureThrottled, FailureUpstream, FailureTimeout, FailureNotConfigured:
		return true
	}
	return false
}

// defaultTimeout bounds every upstream call. A market panel that has not
// answered in this long is better reported as unavailable than left spinning.
const defaultTimeout = 12 * time.Second

// maxResponseBytes caps a vendor body. Full history responses are large but
// bounded; anything past this is a vendor malfunction, not data.
const maxResponseBytes = 24 << 20

// transport is the shared upstream caller: one bounded, redaction-safe GET.
type transport struct {
	client   *http.Client
	maxBytes int64
	// now is the clock seam so cache tiers and metering are testable without
	// sleeping.
	now func() time.Time
}

func newTransport(client *http.Client) *transport {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &transport{client: client, maxBytes: maxResponseBytes, now: time.Now}
}

// get performs one upstream GET and returns the body bytes. Every non-success
// path becomes a *Failure carrying a plain-language message; the request URL —
// which holds the API key — never reaches the message, the detail, or the logs.
func (t *transport) get(ctx context.Context, provider Provider, endpoint, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, &Failure{
			Kind: FailureBadRequest, Provider: provider, Endpoint: endpoint,
			Message: "That market request could not be built.",
			Detail:  redactKeys(err.Error()),
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "matrix-router-finance/1")

	res, err := t.client.Do(req)
	if err != nil {
		kind := FailureUpstream
		msg := "The market data provider could not be reached."
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			kind = FailureTimeout
			msg = "The market data provider did not answer in time."
		}
		return nil, &Failure{
			Kind: kind, Provider: provider, Endpoint: endpoint,
			Message: msg, Detail: redactKeys(err.Error()),
		}
	}
	defer res.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(res.Body, t.maxBytes))
	if readErr != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: provider, Endpoint: endpoint, Status: res.StatusCode,
			Message: "The market data provider's response could not be read.",
			Detail:  redactKeys(readErr.Error()),
		}
	}

	switch {
	case res.StatusCode == http.StatusTooManyRequests:
		return nil, &Failure{
			Kind: FailureThrottled, Provider: provider, Endpoint: endpoint, Status: res.StatusCode,
			Message:    "The market data provider is rate limiting requests right now.",
			Detail:     snippet(body),
			RetryAfter: retryAfter(res.Header.Get("Retry-After")),
		}
	case res.StatusCode == http.StatusUnauthorized, res.StatusCode == http.StatusForbidden:
		return nil, &Failure{
			Kind: FailureNotConfigured, Provider: provider, Endpoint: endpoint, Status: res.StatusCode,
			Message: "The market data provider rejected this deployment's credentials.",
			Detail:  snippet(body),
		}
	case res.StatusCode == http.StatusNotFound:
		return nil, &Failure{
			Kind: FailureNotFound, Provider: provider, Endpoint: endpoint, Status: res.StatusCode,
			Message: "The market data provider has nothing for that request.",
			Detail:  snippet(body),
		}
	case res.StatusCode < 200 || res.StatusCode >= 300:
		return nil, &Failure{
			Kind: FailureUpstream, Provider: provider, Endpoint: endpoint, Status: res.StatusCode,
			Message: "The market data provider returned an error.",
			Detail:  snippet(body),
		}
	}
	return body, nil
}

func isTimeout(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// snippet bounds vendor text for a log line and strips anything key-shaped.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300]
	}
	return redactKeys(s)
}

// redactKeys removes apikey query values from any string headed for a log or an
// error. Both vendors carry the key in the query string, so a naive error wrap
// would leak it into logs.
//
// The scan advances past each replacement rather than restarting: the
// substitution re-introduces the parameter name it just matched, so a
// search-from-zero loop would rewrite the same span forever.
func redactKeys(s string) string {
	for _, param := range []string{"apikey=", "apiKey=", "api_key="} {
		var b strings.Builder
		rest := s
		for {
			i := strings.Index(rest, param)
			if i < 0 {
				b.WriteString(rest)
				break
			}
			b.WriteString(rest[:i+len(param)])
			b.WriteString("REDACTED")
			j := i + len(param)
			for j < len(rest) && !isValueDelimiter(rest[j]) {
				j++
			}
			rest = rest[j:]
		}
		s = b.String()
	}
	return s
}

// isValueDelimiter marks the end of a query-parameter value inside free text.
func isValueDelimiter(c byte) bool {
	switch c {
	case '&', ' ', '"', '\n', '\t', '\'', ',', ')', '}':
		return true
	}
	return false
}

// buildURL joins a base, a path and query params. Empty param values are
// dropped so a vendor never sees `&from=`.
func buildURL(base, path string, params map[string]string) string {
	u := strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
	q := url.Values{}
	for k, v := range params {
		if strings.TrimSpace(v) == "" {
			continue
		}
		q.Set(k, v)
	}
	if len(q) == 0 {
		return u
	}
	return u + "?" + q.Encode()
}

// notConfigured is the typed answer when a vendor key is absent. The lane always
// boots; it degrades honestly at call time, naming the missing variable so an
// operator knows exactly what to set.
func notConfigured(provider Provider, endpoint, envVar string) *Failure {
	return &Failure{
		Kind: FailureNotConfigured, Provider: provider, Endpoint: endpoint,
		Message: "Market data is not configured for this deployment.",
		Detail:  "missing " + envVar,
	}
}
