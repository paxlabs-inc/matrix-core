// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package identity

import (
	"regexp"
	"strings"
)

var underlyingModelNames = []string{
	"chatgpt", "gpt-5", "gpt-4o", "gpt-4.1", "gpt-4", "gpt-3.5", "gpt",
	"grok", "claude", "gemini", "bard", "llama", "qwen", "kimi",
	"deepseek", "mistral", "copilot",
	"openai", "anthropic", "google deepmind", "deepmind",
	"moonshot ai", "moonshot", "meta ai", "xai",
}

var scrubs = func() []*regexp.Regexp {
	parts := make([]string, len(underlyingModelNames))
	for index, name := range underlyingModelNames {
		parts[index] = regexp.QuoteMeta(name)
	}
	model := "(?:" + strings.Join(parts, "|") + ")"
	filler := `(?:actually|really|truly|honestly|simply|just|basically)?`
	article := `(?:an? |the )?`
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)(\bi am|\bi['’]?m|\bi was|\bi,)([\s,]*` + filler + `[\s,]*)` + article + model + `\b`),
		regexp.MustCompile(`(?i)(\bmy name is|\bname['’]?s|\bnamed|\bcalled|\bknown as|\bthis is)(\s+)` + article + model + `\b`),
		regexp.MustCompile(`(?i)([(\[{]\s*|[.;:,]\s+|[—–-]\s*|^)((?:acting |posing |speaking |responding |role[- ]?playing |role[- ]?play )?as )` + article + model + `\b`),
	}
}()

func Scrub(name, text string) (string, bool) {
	if text == "" {
		return text, false
	}
	if name == "" {
		name = "Neo"
	}
	safe := strings.ReplaceAll(name, "$", "$$")
	result := text
	for _, scrub := range scrubs {
		result = scrub.ReplaceAllString(result, "${1}${2}"+safe)
	}
	return result, result != text
}
