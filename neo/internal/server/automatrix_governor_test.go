// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import "testing"

// The chronos MCP bridge (tools/chronos/chronos.mjs envelopeResult) returns the
// chronosd envelope {"ok":true,"data":{...CreateAlarmResponse}} as the tool
// text — the alarm id lives at data.id, NOT at the top level. parseAlarmID must
// unwrap it; failing to do so made every enable return "no parseable id" and
// the Settings toggle roll back (the "toggle doesn't work" bug).
func TestParseAlarmID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "chronos bridge envelope (the production shape)",
			in:   `{"ok":true,"data":{"id":"alm_9f2c1d","owner":"did:matrix:x","next_fire_at":"2026-07-11T12:00:00Z","status":"active"}}`,
			want: "alm_9f2c1d",
		},
		{
			name: "bare CreateAlarmResponse object",
			in:   `{"id":"alm_top","next_fire_at":"2026-07-11T12:00:00Z","status":"active"}`,
			want: "alm_top",
		},
		{
			name: "alarm_id variant",
			in:   `{"alarm_id":"alm_alt"}`,
			want: "alm_alt",
		},
		{
			name: "nested alarm wrapper",
			in:   `{"ok":true,"data":{"alarm":{"id":"alm_deep"}}}`,
			want: "alm_deep",
		},
		{
			name: "error envelope has no id",
			in:   `{"ok":false,"error":{"code":"unreachable","message":"chronosd down","retryable":true}}`,
			want: "",
		},
		{name: "empty", in: "", want: ""},
		{name: "not json", in: "alarm created", want: ""},
		{name: "id not a string", in: `{"ok":true,"data":{"id":42}}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAlarmID(tc.in); got != tc.want {
				t.Fatalf("parseAlarmID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
