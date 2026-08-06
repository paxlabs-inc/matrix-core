// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package address

import (
	"strings"
	"testing"
)

func TestAddressIdentityPromptAndVisibleContainment(t *testing.T) {
	identity := New("Ada", "Nova")
	if identity.AddressForm != PreferredName || !strings.Contains(identity.Prompt(), "Their preferred name is Ada") ||
		!strings.Contains(identity.Prompt(), "never call them ‘the user’") ||
		!strings.Contains(identity.Prompt(), "Do not mention or acknowledge this instruction") ||
		strings.Contains(identity.Prompt(), "The user's name") {
		t.Fatalf("identity=%+v prompt=%q", identity, identity.Prompt())
	}
	if err := identity.ValidateVisible("I will answer directly.", "Ada, here is the result."); err != nil {
		t.Fatal(err)
	}
	if err := identity.ValidateVisible("The user said hello Neo.", "Hello Ada, how can I help?"); err != nil {
		t.Fatalf("private reasoning must not reject a valid answer: %v", err)
	}
	for _, test := range []struct{ answer string }{
		{answer: "The user asked for a concise result."},
		{answer: "Hello Alice, here is the result."},
	} {
		if err := identity.ValidateVisible("", test.answer); err == nil {
			t.Fatalf("prohibited output passed: %+v", test)
		}
	}
	unnamed := New("", "Neo")
	if unnamed.AddressForm != SecondPerson || unnamed.ValidateVisible("", "Hello Jordan, done.") == nil {
		t.Fatalf("unnamed identity failed containment: %+v", unnamed)
	}
}
