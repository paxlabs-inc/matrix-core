// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"regexp"
	"strings"
)

// auditEventIdentityLeak is emitted on the audit side-channel when the model
// broke character and self-identified as its underlying LLM. It is a
// COMPLIANCE canary, not merely a brand check: Neo is an agent (a charter
// imposed on some foundation model), and a model that will not hold the
// simplest system rule — its own name — is signalling it may disregard the
// harder ones (money, safety, do-not-touch). Surfacing the breach keeps us from
// being blind to that; scrubbing (below) contains it; the guidance re-anchor
// makes the model self-correct in-loop.
const auditEventIdentityLeak = "identity.leak"

// underlyingModelNames enumerates the foundation-model and lab identities Neo
// must never present as its OWN identity. That a large model provides Neo's
// fluency is not a secret; the failure is the model speaking or reasoning AS
// one of these instead of as the agent. Ordered longest-first WITHIN a family
// (gpt-4o before gpt) because Go's regexp alternation is leftmost-first, so the
// most specific token must come earlier to win.
var underlyingModelNames = []string{
	"chatgpt", "gpt-5", "gpt-4o", "gpt-4.1", "gpt-4", "gpt-3.5", "gpt",
	"grok", "claude", "gemini", "bard", "llama", "qwen", "kimi",
	"deepseek", "mistral", "copilot",
	"openai", "anthropic", "google deepmind", "deepmind",
	"moonshot ai", "moonshot", "meta ai", "xai",
}

// modelAlternation builds the non-capturing regexp alternation of every banned
// self-identity token, each escaped so metacharacters (the "." in "gpt-3.5")
// are literal.
func modelAlternation() string {
	parts := make([]string, len(underlyingModelNames))
	for i, m := range underlyingModelNames {
		parts[i] = regexp.QuoteMeta(m)
	}
	return "(?:" + strings.Join(parts, "|") + ")"
}

// identityScrubs are the high-precision self-identification patterns. Each
// captures the leading cue in groups 1 and 2 (an optional article between the
// cue and the model name is consumed but dropped) so a replacement of
// "${1}${2}<name>" keeps the sentence intact while swapping only the identity
// token. They target FIRST-PERSON self-identification and self-role framing —
// never a third-person mention — so legitimately discussing these models (e.g.
// "GPT is OpenAI's model", "I'm using Claude for this") is left untouched.
var identityScrubs = func() []*regexp.Regexp {
	m := modelAlternation()
	filler := `(?:actually|really|truly|honestly|simply|just|basically)?`
	article := `(?:an? |the )?`
	return []*regexp.Regexp{
		// "I am / I'm / I was / I," [filler] [article] MODEL
		regexp.MustCompile(`(?i)(\bi am|\bi['’]?m|\bi was|\bi,)([\s,]*` + filler + `[\s,]*)` + article + m + `\b`),
		// "my name is / name's / named / called / known as / this is" [article] MODEL
		regexp.MustCompile(`(?i)(\bmy name is|\bname['’]?s|\bnamed|\bcalled|\bknown as|\bthis is)(\s+)` + article + m + `\b`),
		// self-role framing at a boundary: "[acting/posing/...] as" [article]
		// MODEL. Group 1 pins the phrase to a clause boundary — sentence start,
		// punctuation, an open paren/bracket, or a dash — so a parenthetical
		// self-label like "(as Grok)" is caught while a comparative preceded by
		// an ordinary word ("just as GPT does") is left alone (no lookbehind in
		// RE2, so the allowed left-context is enumerated explicitly).
		regexp.MustCompile(`(?i)([(\[{]\s*|[.;:,]\s+|[—–-]\s*|^)((?:acting |posing |speaking |responding |role[- ]?playing |role[- ]?play )?as )` + article + m + `\b`),
	}
}()

// agentName is the configured agent identity with the canonical "Neo"
// fallback — the single source the identity guardrails resolve the name from.
func (a *Agent) agentName() string {
	if n := strings.TrimSpace(a.cfg.AgentName); n != "" {
		return n
	}
	return "Neo"
}

// cleanContent applies the identity scrub to a user-facing content surface,
// discarding the leaked flag (the caller only wants the safe text). Used at the
// narration/answer surfaces where detection is handled separately.
func (a *Agent) cleanContent(text string) string {
	out, _ := scrubIdentity(a.agentName(), text)
	return out
}

// scrubIdentity rewrites any self-identification as an underlying model to the
// agent's own name and reports whether it changed anything (a detected leak).
// It is deterministic and display-safe: applied to reasoning glimpses, streamed
// deltas, and the delivered answer so a breach never reaches the user even when
// the prompt-level guardrail and the in-loop re-anchor both miss.
func scrubIdentity(name, text string) (string, bool) {
	if text == "" {
		return text, false
	}
	if name == "" {
		name = "Neo"
	}
	// Escape "$" so a name can never be read as a replacement template ref.
	safe := strings.ReplaceAll(name, "$", "$$")
	out := text
	for _, re := range identityScrubs {
		out = re.ReplaceAllString(out, "${1}${2}"+safe)
	}
	return out, out != text
}

// identityReanchorNudge is the private guidance-channel steer used to re-anchor
// a model that broke character. It corrects the CURRENT turn and reasserts the
// charter's authority rather than only hiding the symptom.
func identityReanchorNudge(name string) string {
	return "You just referred to yourself as the underlying language model. You are " + name +
		" — that is your only identity, in your reply and in your reasoning. The model beneath you is infrastructure, not who you are. Re-anchor and answer as " + name +
		", and hold every other system rule just as firmly."
}
