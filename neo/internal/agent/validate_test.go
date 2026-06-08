// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"
)

func TestExtractHighEntropyTokens(t *testing.T) {
	transcript := strings.Join([]string{
		"deployed to 0x63b13bc2fAaE1Ad3a377fec3101B92902cA0Cf23",
		"tx 0x75e14a1effbd63fda2681474c4bfde14845023fdfaf61404f067bfc17305766e",
		"file /root/matrix/neo/internal/agent/validate.go",
		"intent 01KTK1DFJB2VB25A5T9PTZ5KZ9",
		"id 1d9a4dd-1111-2222-3333-444455556666",
		"amount 1500000000000000000 wei",
		"the cat sat on the mat",
	}, "\n")
	toks := extractHighEntropyTokens(transcript)
	mustContain := []string{
		"0x63b13bc2fAaE1Ad3a377fec3101B92902cA0Cf23",
		"0x75e14a1effbd63fda2681474c4bfde14845023fdfaf61404f067bfc17305766e",
		"/root/matrix/neo/internal/agent/validate.go",
		"01KTK1DFJB2VB25A5T9PTZ5KZ9",
		"1500000000000000000",
	}
	for _, want := range mustContain {
		found := false
		for _, g := range toks {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("token %q not extracted; got %v", want, toks)
		}
	}
	// plain prose words must not be treated as high-entropy.
	for _, g := range toks {
		if g == "the" || g == "cat" || g == "mat" {
			t.Errorf("prose word leaked into tokens: %q", g)
		}
	}
}

func TestValidateSummaryRepairsDroppedTokens(t *testing.T) {
	transcript := "deployed MRT at 0xDeAdBeEf00112233445566778899aabbccddeeff and saved /tmp/out/report.json"
	summary := "GOAL: ship the token\nNEXT: verify on chain" // omits the address + path
	repaired, clean := validateSummary(transcript, summary)
	if clean {
		t.Error("should not be clean — tokens were dropped")
	}
	if !strings.Contains(repaired, "0xDeAdBeEf00112233445566778899aabbccddeeff") {
		t.Errorf("dropped address not restored:\n%s", repaired)
	}
	if !strings.Contains(repaired, "/tmp/out/report.json") {
		t.Errorf("dropped path not restored:\n%s", repaired)
	}
	if !strings.Contains(repaired, "preserved verbatim") {
		t.Error("expected a preserved-identifiers marker")
	}
}

func TestValidateSummaryCleanWhenPreserved(t *testing.T) {
	addr := "0xDeAdBeEf00112233445566778899aabbccddeeff"
	transcript := "deployed at " + addr
	summary := "GOAL: deploy\nARTIFACTS: " + addr + "\nNEXT: done"
	repaired, clean := validateSummary(transcript, summary)
	if !clean {
		t.Error("schema present + tokens preserved should be clean")
	}
	if repaired != summary {
		t.Errorf("clean summary must be returned unchanged:\n%s", repaired)
	}
}

func TestSummaryHasSchema(t *testing.T) {
	if !summaryHasSchema("GOAL: x\nNEXT: y") {
		t.Error("should detect schema headers")
	}
	if summaryHasSchema("just some prose without headers") {
		t.Error("should not detect schema in plain prose")
	}
}
