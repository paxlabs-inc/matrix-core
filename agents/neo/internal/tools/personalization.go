// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"centra/agents/neo/internal/llm"
)

// The ORACLE personalization save tool (task 5.3, req 12.3/13.3) — the ONLY
// agent path that writes the personalization profile record. It is advertised
// EXCLUSIVELY to interview agents (agent Options.Interview appends
// PersonalizationSchema; it is deliberately absent from Schemas /
// SubagentSchemas / BriefSchemas), and the dispatch enforces the confirmation
// gate: nothing is persisted unless the model attests the user explicitly
// confirmed the plain-language summary.

// SavePersonalizationTool is the synthetic function an interview agent calls
// AFTER the user confirms the plain-language profile summary.
const SavePersonalizationTool = "save_personalization_profile"

// PersonalizationSaveFunc persists the confirmed profile JSON (the tool's
// arguments minus the confirmation flag) as the single versioned record and
// returns its URI. Wired by the engine onto the real pager.
type PersonalizationSaveFunc func(ctx context.Context, profileJSON []byte) (string, error)

// SetPersonalizationSave wires the confirmed-profile writer after construction.
// nil leaves the tool inert (an interview can run, but a save politely fails).
func (m *Manager) SetPersonalizationSave(f PersonalizationSaveFunc) { m.personalization = f }

// PersonalizationSchema advertises the save tool. It is NOT part of any of the
// Manager's schema sets — only an interview agent appends it (req 13.3: profile
// mutations originate solely from confirmed interview output, explicit Settings
// edits, or explicit recommendation feedback).
func PersonalizationSchema() llm.Tool {
	strList := func(desc string) map[string]interface{} {
		return map[string]interface{}{
			"type":        "array",
			"items":       map[string]interface{}{"type": "string"},
			"description": desc,
		}
	}
	taste := func(cat string) map[string]interface{} {
		return map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"liked":    strList("The " + cat + " the user said they like."),
				"disliked": strList("The " + cat + " the user said they dislike or want less of."),
			},
		}
	}
	return llm.NewFunctionTool(
		SavePersonalizationTool,
		"Save the user's confirmed personalization profile as the single versioned record. Call this ONLY after you presented the plain-language summary of everything you understood and the user explicitly confirmed it — never mid-interview, never with unconfirmed or inferred content. Include ONLY what the user actually said and confirmed; a skipped question group is simply omitted. Never record sensitive traits (health, religion, politics, sexuality, finances) — there are no fields for them.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"user_confirmed": map[string]interface{}{
					"type":        "boolean",
					"description": "Set true ONLY if the user explicitly confirmed the plain-language profile summary you presented. Nothing is saved otherwise.",
				},
				"interests":        strList("Topics and interests the user confirmed (e.g. \"AI research\", \"country music\")."),
				"day_to_day_goals": strList("Recurring day-to-day things the user wants help with."),
				"media": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"music":    taste("music artists/genres"),
						"films":    taste("films"),
						"shows":    taste("shows"),
						"books":    taste("books"),
						"games":    taste("games"),
						"creators": taste("creators"),
					},
				},
				"adventurousness": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"familiar", "balanced", "surprising"},
					"description": "How adventurous recommendations should be.",
				},
				"brief_preferences": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"length": map[string]interface{}{
							"type": "string",
							"enum": []string{"short", "standard", "deep"},
						},
						"sections": map[string]interface{}{
							"type":        "array",
							"items":       map[string]interface{}{"type": "string", "enum": []string{"news", "music", "movies_tv", "books", "games", "daily_assistance"}},
							"description": "The brief sections the user wants.",
						},
					},
				},
			},
			"required": []interface{}{"user_confirmed"},
		},
	)
}

// dispatchPersonalizationSave enforces the confirmation gate (req 12.3) and
// persists the confirmed profile through the wired writer. Refusals are
// returned in-band (isError=true, err=nil) so the model reads the steer —
// present the summary, get the explicit confirmation — rather than the harness
// retrying a doomed call.
func (m *Manager) dispatchPersonalizationSave(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if m.personalization == nil {
		return "saving the personalization profile isn't available in this session.", true, nil
	}
	confirmed, _ := args["user_confirmed"].(bool)
	if !confirmed {
		return "Nothing was saved. Present the user a plain-language summary of everything you understood and get their explicit confirmation first — only a confirmed summary may be saved.", true, nil
	}
	profile := make(map[string]interface{}, len(args))
	for k, v := range args {
		if k == "user_confirmed" {
			continue
		}
		profile[k] = v
	}
	payload, err := json.Marshal(profile)
	if err != nil {
		return "", true, fmt.Errorf("personalization: encode: %w", err)
	}
	uri, serr := m.personalization(ctx, payload)
	if serr != nil {
		return "", true, fmt.Errorf("personalization: save: %w", serr)
	}
	_ = uri
	return "Saved. The confirmed profile is now the user's single personalization record — they can view, edit, export, or delete it in Settings at any time.", false, nil
}
