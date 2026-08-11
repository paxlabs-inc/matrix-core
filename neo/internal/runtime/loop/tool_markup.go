// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"errors"
	"fmt"
	"strings"
)

var ErrTextualToolMarkup = errors.New(
	"runtime loop: provider leaked textual tool markup",
)

func rejectTextualToolMarkup(content string) error {
	if !containsToolMarkup(content) || markupOnlyInMarkdownCode(content) {
		return nil
	}
	return fmt.Errorf(
		"%w: normalization must occur at the MiMo provider boundary",
		ErrTextualToolMarkup,
	)
}

func containsToolMarkup(content string) bool {
	for _, marker := range []string{
		"<tool_call>", "</tool_call>", "<function=", "<parameter=",
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

func markupOnlyInMarkdownCode(content string) bool {
	var outside strings.Builder
	inFence := false
	inInline := false
	for index := 0; index < len(content); {
		if strings.HasPrefix(content[index:], "```") {
			inFence = !inFence
			index += 3
			continue
		}
		if !inFence && content[index] == '`' {
			inInline = !inInline
			index++
			continue
		}
		if !inFence && !inInline {
			outside.WriteByte(content[index])
		}
		index++
	}
	return !containsToolMarkup(outside.String())
}
