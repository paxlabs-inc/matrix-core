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
		strings.Contains(identity.Prompt(), "The user's name") {
		t.Fatalf("identity=%+v prompt=%q", identity, identity.Prompt())
	}
	if err := identity.ValidateVisible("I will answer directly.", "Ada, here is the result."); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ reasoning, answer string }{
		{reasoning: "The user wants a concise result."},
		{answer: "Hello Alice, here is the result."},
	} {
		if err := identity.ValidateVisible(test.reasoning, test.answer); err == nil {
			t.Fatalf("prohibited output passed: %+v", test)
		}
	}
	unnamed := New("", "Neo")
	if unnamed.AddressForm != SecondPerson || unnamed.ValidateVisible("", "Hello Jordan, done.") == nil {
		t.Fatalf("unnamed identity failed containment: %+v", unnamed)
	}
}
