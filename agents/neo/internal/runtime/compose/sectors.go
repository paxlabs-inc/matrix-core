// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package compose

import (
	"fmt"
	"sort"

	"centra/agents/neo/internal/runtime/records"
)

type Sector uint8

const (
	SectorStableIdentity Sector = iota + 1
	SectorLatestMessage
	SectorRecentTranscript
	SectorWorkingState
	SectorUnconsumedToolBatch
	SectorWarmCapsules
	SectorLongTermMemory
	SectorToolSchemas
	SectorResponseReserve
)

type SectorPolicy struct {
	TotalTokens     int
	ResponseReserve int
	Budgets         map[Sector]int
}

type TrimResult struct {
	Items       []Item
	Suppressed  map[string]string
	Diagnostics []string
	UsedTokens  int
}

func DefaultSectorPolicy(total, responseReserve int) SectorPolicy {
	if total <= 0 || total > 160_000 {
		total = 160_000
	}
	if responseReserve <= 0 {
		responseReserve = 8_192
	}
	usable := max(total-responseReserve, 1)
	return SectorPolicy{
		TotalTokens: total, ResponseReserve: responseReserve,
		Budgets: map[Sector]int{
			SectorStableIdentity:      usable * 20 / 100,
			SectorLatestMessage:       usable * 10 / 100,
			SectorRecentTranscript:    usable * 25 / 100,
			SectorWorkingState:        usable * 10 / 100,
			SectorUnconsumedToolBatch: usable * 15 / 100,
			SectorWarmCapsules:        usable * 8 / 100,
			SectorLongTermMemory:      usable * 7 / 100,
			SectorToolSchemas:         usable * 5 / 100,
		},
	}
}

func itemKey(item Item) string { return item.SourceNamespace + "\x00" + item.SourceID }

func itemTokens(item Item) int { return max((len(item.Content)+3)/4, 1) }

// ApplySectorBudgets enforces independent sector ceilings, then the frozen
// global trim order. Never-trim evidence survives even when diagnostics must
// report that mandatory material alone exceeds the preferred request size.
func ApplySectorBudgets(items []Item, policy SectorPolicy) TrimResult {
	if policy.TotalTokens <= 0 {
		policy = DefaultSectorPolicy(0, policy.ResponseReserve)
	}
	keep := make([]bool, len(items))
	sectorUsed := make(map[Sector]int)
	used := policy.ResponseReserve
	for index, item := range items {
		keep[index] = true
		cost := itemTokens(item)
		sectorUsed[item.Sector] += cost
		used += cost
	}
	result := TrimResult{Suppressed: make(map[string]string)}
	trim := func(index int, reason string) {
		if !keep[index] || items[index].NeverTrim {
			return
		}
		keep[index] = false
		cost := itemTokens(items[index])
		sectorUsed[items[index].Sector] -= cost
		used -= cost
		result.Suppressed[itemKey(items[index])] = reason
	}
	for sector := SectorStableIdentity; sector <= SectorToolSchemas; sector++ {
		budget, limited := policy.Budgets[sector]
		if !limited {
			continue
		}
		for index := range items {
			if sectorUsed[sector] <= budget {
				break
			}
			if items[index].Sector == sector {
				trim(index, "sector_budget_trim")
			}
		}
	}
	trimOrder := []Sector{SectorLongTermMemory, SectorWarmCapsules, SectorRecentTranscript, SectorWorkingState, SectorToolSchemas}
	for _, sector := range trimOrder {
		for index := range items {
			if used <= policy.TotalTokens {
				break
			}
			if items[index].Sector == sector {
				trim(index, "global_trim_order")
			}
		}
	}
	if used > policy.TotalTokens {
		result.Diagnostics = append(result.Diagnostics, fmt.Sprintf(
			"mandatory context uses %d tokens over the %d-token preferred request; never-trim evidence was preserved",
			used, policy.TotalTokens,
		))
	}
	for index, item := range items {
		if keep[index] {
			result.Items = append(result.Items, item)
		}
	}
	result.UsedTokens = used
	sort.Strings(result.Diagnostics)
	return result
}

func Compose(items []Item, policy SectorPolicy) ([]Item, records.ContextManifest, []string) {
	deduplicated, manifest := Deduplicate(items)
	trimmed := ApplySectorBudgets(deduplicated, policy)
	for index := range manifest.Entries {
		entry := &manifest.Entries[index]
		if reason, exists := trimmed.Suppressed[entry.SourceNamespace+"\x00"+entry.SourceID]; exists && entry.Included {
			entry.Included = false
			entry.Reason = reason
		}
	}
	return trimmed.Items, manifest, trimmed.Diagnostics
}
