// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package address

import (
	"fmt"
	"regexp"
	"strings"
)

type Form string

const (
	SecondPerson  Form = "second_person"
	PreferredName Form = "preferred_name"
)

type Identity struct {
	PreferredPersonName string `json:"preferred_person_name,omitempty"`
	AgentName           string `json:"agent_name"`
	AddressForm         Form   `json:"address_form"`
}

func New(preferredPersonName, agentName string) Identity {
	identity := Identity{
		PreferredPersonName: strings.TrimSpace(preferredPersonName),
		AgentName:           strings.TrimSpace(agentName),
		AddressForm:         SecondPerson,
	}
	if identity.AgentName == "" {
		identity.AgentName = "Neo"
	}
	if identity.PreferredPersonName != "" {
		identity.AddressForm = PreferredName
	}
	return identity
}

func (identity Identity) Prompt() string {
	identity = New(identity.PreferredPersonName, identity.AgentName)
	var prompt strings.Builder
	prompt.WriteString("Address identity:\n")
	fmt.Fprintf(&prompt, "- Your agent name is %s.\n", identity.AgentName)
	prompt.WriteString("- Speak directly to the person as ‘you’; never refer to them as ‘the user’ in visible reasoning or answers.\n")
	if identity.AddressForm == PreferredName {
		fmt.Fprintf(&prompt, "- Their preferred name is %s, with exactly that capitalization. Use it sparingly when direct address genuinely helps; otherwise use second person. Never invent or infer another person-name.\n", identity.PreferredPersonName)
	} else {
		prompt.WriteString("- No preferred person-name is configured. Use second person and never invent or infer a name.\n")
	}
	return prompt.String()
}

var fabricatedGreeting = regexp.MustCompile(`(?:^|[.!?]\s+)(?:Hi|Hello|Hey|Dear)\s+([A-Z][A-Za-z'’-]{1,50})\b`)

func (identity Identity) ValidateVisible(reasoning, answer string) error {
	identity = New(identity.PreferredPersonName, identity.AgentName)
	for channel, content := range map[string]string{"reasoning": reasoning, "answer": answer} {
		lower := strings.ToLower(content)
		if strings.Contains(lower, "the user") {
			return fmt.Errorf("address identity: %s used prohibited third-person phrasing", channel)
		}
		for _, match := range fabricatedGreeting.FindAllStringSubmatch(content, -1) {
			if len(match) < 2 {
				continue
			}
			name := strings.TrimSpace(match[1])
			if identity.PreferredPersonName == "" || name != identity.PreferredPersonName {
				return fmt.Errorf("address identity: %s fabricated person-name %q", channel, name)
			}
		}
	}
	return nil
}

func RepairInstruction(identity Identity) string {
	identity = New(identity.PreferredPersonName, identity.AgentName)
	if identity.PreferredPersonName != "" {
		return fmt.Sprintf("Rewrite visible reasoning and the answer in direct second person. You may use the configured preferred name %q sparingly, with exact capitalization. Never say ‘the user’ and never invent another name.", identity.PreferredPersonName)
	}
	return "Rewrite visible reasoning and the answer in direct second person. Never say ‘the user’ and never invent a person-name."
}
