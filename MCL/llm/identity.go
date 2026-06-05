// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package llm — identity preamble injection (Session 34 / Forge Phase 1).
//
// Forge is the local self-maintenance spin-off of Matrix (matrix.kvx sess#34).
// The recursion thesis — Matrix maintains Matrix — requires every frontier-model
// call to carry a small persistent reminder that the speaker IS Matrix and its
// sole purpose right now is to optimize the Matrix codebase itself.
//
// Implementation:
//
//   - IdentityPreamble is the canonical string injected as a system message.
//   - IdentityVersion identifies the preamble revision; bumping it cleanly
//     invalidates the sess#31d compile-cache because the cache key incorporates
//     model_digest, which downstream callers compose with IdentityVersion.
//   - InjectIdentity is a pure helper that prepends the preamble as the FIRST
//     system message, leaving any caller-supplied system prompt intact at index 1.
//   - Config.InjectIdentity gates the injection per client instance so legacy
//     paths (ambient daemon, mcl-e2e harness) preserve their pre-Phase-1 wire
//     bytes byte-identically when the flag is left false.
//
// All three API shapes (chat-completions / messages / responses) call
// InjectIdentity from their Decode + Stream entry points so identity reaches
// every routed frontier-model invocation uniformly regardless of provider.

package llm

import (
	"matrix/mcl/mtx/interpreter"
)

// IdentityVersion identifies the IdentityPreamble revision. Bumped whenever
// the preamble text changes; mixed into model_digest by callers so the
// compile-cache (sess#31d) invalidates cleanly across preamble migrations.
// Replay byte-identity (cortex §13.4) is preserved across the bump because
// cached entries with stale version simply miss + recompute.
const IdentityVersion = "matrix-identity-v1"

// IdentityPreamble is the system-message text injected at every Decode +
// Stream call when Config.InjectIdentity is true. Locked verbatim by Andrew
// 2026-05-27 (matrix.kvx sess#34). Edits MUST also bump IdentityVersion.
const IdentityPreamble = "You are Matrix — a cognition+UX layer running on the Paxeer Network. Your codebase lives at /root/matrix. This conversation is part of your ongoing self-maintenance. Every action you take should serve improving Matrix itself."

// InjectIdentity prepends IdentityPreamble as the FIRST system message of
// the returned slice. If messages already begins with a system role, the
// preamble is prepended as a SECOND system message ahead of it — both
// providers (Anthropic Messages, OpenAI Responses, OpenAI Chat Completions)
// accept multiple system messages and concatenate them in declaration order.
//
// Pure: the input slice is not mutated; the returned slice is freshly
// allocated with len(messages)+1 elements.
//
// Callers MUST honour Config.InjectIdentity — passing this helper through
// unconditionally would break legacy paths (e.g. mcl-e2e harness assertions
// on exact wire bytes pre-sess#34).
func InjectIdentity(messages []interpreter.Message) []interpreter.Message {
	out := make([]interpreter.Message, 0, len(messages)+1)
	out = append(out, interpreter.Message{
		Role:    "system",
		Content: IdentityPreamble,
	})
	out = append(out, messages...)
	return out
}

// maybeInjectIdentity applies InjectIdentity iff cfg.InjectIdentity is set.
// Internal helper called from each client (chat-completions / messages /
// responses) at the top of Decode + Stream so the gate is enforced uniformly
// across all three API shapes.
func maybeInjectIdentity(cfg Config, messages []interpreter.Message) []interpreter.Message {
	if !cfg.InjectIdentity {
		return messages
	}
	return InjectIdentity(messages)
}

// IdentityModelDigestSuffix returns the suffix to append to a model's
// digest string when computing compile-cache keys for Identity-injected
// routes. Use:
//
//	digest := "claude-opus-4-7@opencode" + llm.IdentityModelDigestSuffix(cfg)
//
// Returns "" when cfg.InjectIdentity is false (legacy callers unaffected).
// Returns "+identity=<IdentityVersion>" when true. Caller composes this
// into the digest string passed to compilecache.Key per sess#31d S31dQ2.
func IdentityModelDigestSuffix(cfg Config) string {
	if !cfg.InjectIdentity {
		return ""
	}
	return "+identity=" + IdentityVersion
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
