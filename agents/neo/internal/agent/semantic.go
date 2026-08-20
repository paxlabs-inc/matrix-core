// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"centra/agents/neo/internal/llm"
)

// semanticRepeat reports whether the current tool-call batch is a no-progress
// repeat of the previous one at the SAME operation, even when the surface
// wording of the arguments differs — so a cosmetic reword cannot defeat the
// no-progress guard (NE-4). Two batches are a semantic repeat when they share:
//   - the same tool SHAPE (the sorted tool names + the keys/values of any
//     NON-text arguments), so a different structured target (a different pool
//     id, a different numeric arg) is a distinct operation, not a repeat; AND
//   - the same TARGET identifiers (symbols / addresses / hashes / numbers
//     extracted from the text arguments), so reading two different pools does
//     NOT count as a repeat (the false-trip guard, req 4.3); AND
//   - a word overlap (the remaining free-text tokens) at or above the
//     configured threshold, so a genuinely different operation on the same
//     target (e.g. "read balance of X" vs "transfer from X") does not trip.
//
// It returns false when semantic detection is disabled (threshold <= 0) or
// either batch is empty. The exact byte-signature path (batchSignature) remains
// the primary trip; this catches the reworded case the exact path misses.
func (a *Agent) semanticRepeat(prev, cur []llm.ToolCall) bool {
	if a.cfg.SemanticStallSimilarityPct <= 0 || len(prev) == 0 || len(cur) == 0 {
		return false
	}
	pShape, pTargets, pWords := opProfile(prev)
	cShape, cTargets, cWords := opProfile(cur)
	if pShape != cShape {
		return false // different tool shape / structured target: distinct ops
	}
	if !sameSet(pTargets, cTargets) {
		return false // different target identifiers (e.g. two different pools)
	}
	return overlapPct(pWords, cWords) >= a.cfg.SemanticStallSimilarityPct
}

// opProfile reduces a tool-call batch to (shape, targetTokens, wordTokens):
//   - shape: a stable key of the sorted tool names plus each non-text argument
//     rendered as key=value (text args contribute only key=str, since their
//     value is normalized into the token sets below).
//   - targetTokens: the high-signal identifiers in the text args (all-caps
//     symbols like SPARK, hex/addresses like 0x…, numbers, long ids).
//   - wordTokens: the remaining significant lowercase words (stopwords and
//     short tokens dropped) — the fuzzy, reword-tolerant part.
func opProfile(calls []llm.ToolCall) (string, map[string]struct{}, map[string]struct{}) {
	names := make([]string, 0, len(calls))
	structParts := make([]string, 0)
	targets := map[string]struct{}{}
	words := map[string]struct{}{}
	for _, c := range calls {
		names = append(names, c.Function.Name)
		args, err := c.ParseArgs()
		if err != nil || len(args) == 0 {
			// Unparseable / no structured args: tokenize the raw argument blob.
			classifyTokens(c.Function.Arguments, targets, words)
			continue
		}
		keys := make([]string, 0, len(args))
		for k := range args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch v := args[k].(type) {
			case string:
				structParts = append(structParts, k+"=str")
				classifyTokens(v, targets, words)
			case nil:
				structParts = append(structParts, k+"=null")
			default:
				structParts = append(structParts, k+"="+fmt.Sprintf("%v", v))
			}
		}
	}
	sort.Strings(names)
	sort.Strings(structParts)
	return strings.Join(names, ",") + "|" + strings.Join(structParts, ","), targets, words
}

// contentChurnSimilarityPct is the word-overlap bar at which two consecutive
// bare-answer prose turns count as the same answer re-derived. It is HIGHER than
// the tool-argument reword bar (SemanticStallSimilarityPct) because prose turns
// naturally share far more common words than terse tool arguments, so the
// threshold for "this is the same answer again" must be stricter to avoid
// flagging two genuinely different answers on the same topic.
const contentChurnSimilarityPct = 80

// contentChurn reports whether two consecutive non-dispatching (bare-answer)
// turns repeat substantially the same prose — the reasoning-churn read the
// tool-batch repeat detectors miss (they compare only tool calls). It reuses the
// significant-word extraction and overlap coefficient of the semantic-repeat
// path. Gated on the same semantic-detection switch so disabling one disables
// both; both texts must carry real content (short answers are too collision-prone
// to judge by word overlap).
func (a *Agent) contentChurn(prev, cur string) bool {
	if a.cfg.SemanticStallSimilarityPct <= 0 {
		return false
	}
	if len(prev) < 40 || len(cur) < 40 {
		return false
	}
	discard := map[string]struct{}{}
	pWords := map[string]struct{}{}
	cWords := map[string]struct{}{}
	classifyTokens(prev, discard, pWords)
	classifyTokens(cur, discard, cWords)
	return overlapPct(pWords, cWords) >= contentChurnSimilarityPct
}

// classifyTokens splits s on non-alphanumeric boundaries and routes each token
// into targets (identifier-like) or words (significant free text).
func classifyTokens(s string, targets, words map[string]struct{}) {
	for _, raw := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if looksLikeTarget(raw) {
			targets[strings.ToLower(raw)] = struct{}{}
			continue
		}
		w := strings.ToLower(raw)
		if len(w) <= 2 || stallStopwords[w] {
			continue
		}
		words[w] = struct{}{}
	}
}

// looksLikeTarget reports whether a token is a high-signal target identifier
// (rather than ordinary prose): it contains a digit (addresses/hashes/amounts),
// is an all-caps symbol (e.g. SPARK, USDC), or is a long opaque identifier.
func looksLikeTarget(tok string) bool {
	hasUpper, hasLower, hasDigit := false, false, false
	for _, r := range tok {
		switch {
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		}
	}
	if hasDigit {
		return true
	}
	if hasUpper && !hasLower && len(tok) >= 2 { // all-caps symbol
		return true
	}
	return len(tok) >= 12 // long opaque identifier / hash
}

// overlapPct is the overlap coefficient (|a∩b| / min(|a|,|b|)) as a percentage.
// Two empty word sets count as fully overlapping (identical word-free op); a
// non-empty vs empty pair counts as no overlap.
func overlapPct(a, b map[string]struct{}) int {
	if len(a) == 0 && len(b) == 0 {
		return 100
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	min := len(a)
	if len(b) < min {
		min = len(b)
	}
	return inter * 100 / min
}

// sameSet reports whether two string sets are equal.
func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// stallStopwords are common low-signal words dropped before the word-overlap
// comparison so they neither inflate nor deflate the similarity score.
var stallStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "please": true,
	"now": true, "then": true, "this": true, "that": true, "you": true,
	"your": true, "can": true, "could": true, "would": true, "will": true,
	"should": true, "just": true, "let": true, "get": true, "also": true,
	"try": true, "again": true, "want": true, "need": true, "make": true,
	"into": true, "from": true, "out": true, "use": true, "via": true,
}
