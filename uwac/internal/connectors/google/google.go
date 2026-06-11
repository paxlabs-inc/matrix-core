// Package google implements the uwac/google-workspace connector: the OAuth
// scope catalog plus Gmail + Calendar tool handlers. This is the first-slice
// connector that proves the whole UWAC spine end-to-end (connections.frozen.kvx
// [slice]).
package google

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/paxlabs-inc/uwac/internal/connectors"
	"github.com/paxlabs-inc/uwac/internal/httpx"
	"github.com/paxlabs-inc/uwac/internal/vault"
	"github.com/paxlabs-inc/uwac/pkg/types"
)

const (
	ID       = "uwac/google-workspace"
	Provider = "google"

	gmailBase    = "https://gmail.googleapis.com/gmail/v1/users/me"
	calendarBase = "https://www.googleapis.com/calendar/v3"

	scopeGmailReadonly  = "https://www.googleapis.com/auth/gmail.readonly"
	scopeGmailSend      = "https://www.googleapis.com/auth/gmail.send"
	scopeCalReadonly    = "https://www.googleapis.com/auth/calendar.readonly"
	scopeCalEvents      = "https://www.googleapis.com/auth/calendar.events"
)

var client = httpx.New(30 * time.Second)

// Connector builds the google-workspace connector (spec + handlers).
func Connector() *connectors.Connector {
	spec := types.ConnectorSpec{
		ID:       ID,
		Provider: Provider,
		Display:  "Google Workspace",
		OAuth: types.OAuthSpec{
			Provider: Provider,
			Scopes:   []string{scopeGmailReadonly, scopeGmailSend, scopeCalReadonly, scopeCalEvents},
			Refresh:  "rotating",
			// Google only returns a refresh token with these two params.
			QueryParams: map[string]string{"access_type": "offline", "prompt": "consent"},
		},
		Tools: []types.ToolSpec{
			{
				Name:            "gmail_search",
				Description:     "Search the owner's Gmail. Returns matching message ids + thread ids. Read-only. args: query (Gmail search syntax, e.g. \"is:unread from:boss\"), max_results?.",
				SideEffectClass: "network",
				Consequence:     types.ConseqNatural,
				Scopes:          []string{scopeGmailReadonly},
				InputSchema: obj(map[string]any{
					"query":       strProp("Gmail search query (e.g. \"is:unread newer_than:2d\")."),
					"max_results": numProp("Max messages to return (default 10)."),
				}, "query"),
			},
			{
				Name:            "gmail_get_message",
				Description:     "Fetch one Gmail message (headers + snippet + body) by id. Read-only. args: id.",
				SideEffectClass: "network",
				Consequence:     types.ConseqNatural,
				Scopes:          []string{scopeGmailReadonly},
				InputSchema: obj(map[string]any{
					"id":     strProp("Gmail message id (from gmail_search)."),
					"format": strProp("metadata | full (default full)."),
				}, "id"),
			},
			{
				Name:            "gmail_send",
				Description:     "Send an email from the owner's Gmail. IRREVERSIBLE: requires user confirmation. args: to, subject, body.",
				SideEffectClass: "write",
				Consequence:     types.ConseqConfirm,
				Scopes:          []string{scopeGmailSend},
				InputSchema: obj(map[string]any{
					"to":      strProp("Recipient email address."),
					"subject": strProp("Subject line."),
					"body":    strProp("Plain-text body."),
				}, "to", "subject", "body"),
			},
			{
				Name:            "calendar_list_events",
				Description:     "List upcoming events on the owner's primary calendar. Read-only. args: max_results?, time_min? (RFC3339, default now).",
				SideEffectClass: "network",
				Consequence:     types.ConseqNatural,
				Scopes:          []string{scopeCalReadonly},
				InputSchema: obj(map[string]any{
					"max_results": numProp("Max events (default 10)."),
					"time_min":    strProp("RFC3339 lower bound (default now)."),
				}),
			},
			{
				Name:            "calendar_create_event",
				Description:     "Create an event on the owner's primary calendar. IRREVERSIBLE: requires user confirmation. args: summary, start (RFC3339), end (RFC3339), description?.",
				SideEffectClass: "write",
				Consequence:     types.ConseqConfirm,
				Scopes:          []string{scopeCalEvents},
				InputSchema: obj(map[string]any{
					"summary":     strProp("Event title."),
					"start":       strProp("Start time, RFC3339 (e.g. 2026-06-12T15:00:00Z)."),
					"end":         strProp("End time, RFC3339."),
					"description": strProp("Optional event description."),
				}, "summary", "start", "end"),
			},
		},
		EventSources: []types.EventSource{
			{Key: "gmail.new_message", Kind: "poll", Description: "Fires when a new message matches a watch query."},
			{Key: "calendar.upcoming_event", Kind: "poll", Description: "Fires ahead of an upcoming calendar event."},
		},
	}

	return &connectors.Connector{
		Spec: spec,
		Handlers: map[string]connectors.Handler{
			"gmail_search":           gmailSearch,
			"gmail_get_message":      gmailGetMessage,
			"gmail_send":             gmailSend,
			"calendar_list_events":   calendarListEvents,
			"calendar_create_event":  calendarCreateEvent,
		},
	}
}

func gmailSearch(ctx context.Context, rec *vault.Record, args map[string]any) (any, error) {
	q := argStr(args, "query")
	if q == "" {
		return nil, connectors.Bad("gmail_search: query is required")
	}
	v := url.Values{}
	v.Set("q", q)
	v.Set("maxResults", strconv.Itoa(argInt(args, "max_results", 10)))
	var out any
	if err := client.JSON(ctx, "GET", gmailBase+"/messages?"+v.Encode(), rec.AccessToken, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func gmailGetMessage(ctx context.Context, rec *vault.Record, args map[string]any) (any, error) {
	id := argStr(args, "id")
	if id == "" {
		return nil, connectors.Bad("gmail_get_message: id is required")
	}
	format := argStr(args, "format")
	if format == "" {
		format = "full"
	}
	var out any
	if err := client.JSON(ctx, "GET", gmailBase+"/messages/"+url.PathEscape(id)+"?format="+url.QueryEscape(format), rec.AccessToken, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func gmailSend(ctx context.Context, rec *vault.Record, args map[string]any) (any, error) {
	to, subject, body := argStr(args, "to"), argStr(args, "subject"), argStr(args, "body")
	if to == "" || subject == "" || body == "" {
		return nil, connectors.Bad("gmail_send: to, subject, and body are required")
	}
	mime := fmt.Sprintf("To: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s", to, subject, body)
	raw := base64.RawURLEncoding.EncodeToString([]byte(mime))
	var out any
	if err := client.JSON(ctx, "POST", gmailBase+"/messages/send", rec.AccessToken, map[string]any{"raw": raw}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func calendarListEvents(ctx context.Context, rec *vault.Record, args map[string]any) (any, error) {
	v := url.Values{}
	v.Set("maxResults", strconv.Itoa(argInt(args, "max_results", 10)))
	v.Set("singleEvents", "true")
	v.Set("orderBy", "startTime")
	tmin := argStr(args, "time_min")
	if tmin == "" {
		tmin = time.Now().UTC().Format(time.RFC3339)
	}
	v.Set("timeMin", tmin)
	var out any
	if err := client.JSON(ctx, "GET", calendarBase+"/calendars/primary/events?"+v.Encode(), rec.AccessToken, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func calendarCreateEvent(ctx context.Context, rec *vault.Record, args map[string]any) (any, error) {
	summary, start, end := argStr(args, "summary"), argStr(args, "start"), argStr(args, "end")
	if summary == "" || start == "" || end == "" {
		return nil, connectors.Bad("calendar_create_event: summary, start, and end are required")
	}
	event := map[string]any{
		"summary": summary,
		"start":   map[string]any{"dateTime": start},
		"end":     map[string]any{"dateTime": end},
	}
	if d := argStr(args, "description"); d != "" {
		event["description"] = d
	}
	var out any
	if err := client.JSON(ctx, "POST", calendarBase+"/calendars/primary/events", rec.AccessToken, event, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ── arg + schema helpers ─────────────────────────────────────────────────────

func argStr(args map[string]any, k string) string {
	if v, ok := args[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	return ""
}

func argInt(args map[string]any, k string, def int) int {
	v, ok := args[k]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if p, err := strconv.Atoi(n); err == nil {
			return p
		}
	}
	return def
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "additionalProperties": true, "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strProp(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func numProp(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
