// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package consolidation

import (
	"fmt"
	"sort"
	"strings"
)

type EvidenceKind string

const (
	EvidenceUser      EvidenceKind = "user"
	EvidenceTool      EvidenceKind = "tool"
	EvidenceAssistant EvidenceKind = "assistant"
	EvidenceVerified  EvidenceKind = "verified_outcome"
)

type Evidence struct {
	Kind      EvidenceKind
	Content   string
	ToolName  string
	AttemptID string
}

type Message struct {
	Role    string
	Content string
}

type Job struct {
	IntentID  string
	Attempt   int
	Terminal  bool
	Objective string
	Context   []Message
	Evidence  []Evidence
	Outcome   string
	Conv      string
	HaveSeq   bool
	SeqLo     uint64
	SeqHi     uint64
}

func (j Job) Empty() bool {
	if strings.TrimSpace(j.Objective) != "" || strings.TrimSpace(j.Outcome) != "" {
		return false
	}
	for _, evidence := range j.Evidence {
		if strings.TrimSpace(evidence.Content) != "" {
			return false
		}
	}
	return true
}

func Merge(current, next Job) Job {
	if strings.TrimSpace(current.IntentID) == "" {
		current.IntentID = strings.TrimSpace(next.IntentID)
	}
	if next.Attempt > current.Attempt {
		current.Attempt = next.Attempt
	}
	current.Terminal = current.Terminal || next.Terminal
	if strings.TrimSpace(current.Objective) == "" {
		current.Objective = strings.TrimSpace(next.Objective)
	}
	if strings.TrimSpace(next.Outcome) != "" {
		current.Outcome = strings.TrimSpace(next.Outcome)
	}
	if strings.TrimSpace(current.Conv) == "" {
		current.Conv = strings.TrimSpace(next.Conv)
	}
	if next.HaveSeq {
		if !current.HaveSeq || next.SeqLo < current.SeqLo {
			current.SeqLo = next.SeqLo
		}
		if !current.HaveSeq || next.SeqHi > current.SeqHi {
			current.SeqHi = next.SeqHi
		}
		current.HaveSeq = true
	}
	current.Context = mergeMessages(current.Context, next.Context)
	current.Evidence = mergeEvidence(current.Evidence, next.Evidence)
	return current
}

func Render(j Job) (string, map[int]struct{}) {
	var b strings.Builder
	fmt.Fprintf(&b, "Intent: %s\nAttempt: %d\nTerminal: %t\n", strings.TrimSpace(j.IntentID), j.Attempt, j.Terminal)
	if objective := strings.TrimSpace(j.Objective); objective != "" {
		b.WriteString("Objective (context only; cite the user evidence below instead):\n")
		b.WriteString(objective)
		b.WriteString("\n")
	}
	b.WriteString("\nEVIDENCE (the only citable source):\n")
	valid := make(map[int]struct{}, len(j.Evidence))
	for i, evidence := range j.Evidence {
		content := strings.TrimSpace(evidence.Content)
		if content == "" {
			continue
		}
		id := i + 1
		valid[id] = struct{}{}
		fmt.Fprintf(&b, "[E%d] %s", id, evidence.Kind)
		if tool := strings.TrimSpace(evidence.ToolName); tool != "" {
			fmt.Fprintf(&b, " tool=%s", tool)
		}
		if attempt := strings.TrimSpace(evidence.AttemptID); attempt != "" {
			fmt.Fprintf(&b, " attempt=%s", attempt)
		}
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	if len(j.Context) > 0 {
		b.WriteString("\nCONTEXT (interpretation only; never cite this section):\n")
		for _, message := range j.Context {
			if content := strings.TrimSpace(message.Content); content != "" {
				fmt.Fprintf(&b, "%s: %s\n", strings.TrimSpace(message.Role), content)
			}
		}
	}
	if outcome := strings.TrimSpace(j.Outcome); outcome != "" {
		b.WriteString("\nCommitted outcome:\n")
		b.WriteString(outcome)
		b.WriteString("\n")
	}
	return b.String(), valid
}

func ValidCitations(ids []int, valid map[int]struct{}) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if _, ok := valid[id]; !ok {
			return false
		}
	}
	return true
}

func mergeEvidence(left, right []Evidence) []Evidence {
	out := append([]Evidence(nil), left...)
	seen := make(map[string]struct{}, len(out))
	for _, evidence := range out {
		seen[evidenceKey(evidence)] = struct{}{}
	}
	for _, evidence := range right {
		key := evidenceKey(evidence)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, evidence)
	}
	sort.SliceStable(out, func(i, k int) bool {
		return out[i].AttemptID < out[k].AttemptID
	})
	return out
}

func evidenceKey(e Evidence) string {
	return string(e.Kind) + "\x00" + strings.TrimSpace(e.ToolName) + "\x00" +
		strings.TrimSpace(e.AttemptID) + "\x00" + strings.TrimSpace(e.Content)
}

func mergeMessages(left, right []Message) []Message {
	out := append([]Message(nil), left...)
	seen := make(map[string]struct{}, len(out))
	for _, message := range out {
		seen[message.Role+"\x00"+strings.TrimSpace(message.Content)] = struct{}{}
	}
	for _, message := range right {
		key := message.Role + "\x00" + strings.TrimSpace(message.Content)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, message)
	}
	return out
}
