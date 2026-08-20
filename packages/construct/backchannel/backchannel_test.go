// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package backchannel

import (
	"strings"
	"testing"

	"centra/packages/construct/schema"
	"centra/packages/construct/schema/primitives"
)

func boolp(b bool) *bool { return &b }

func TestValidateResponse(t *testing.T) {
	cases := []struct {
		name string
		ask  *primitives.Ask
		resp *primitives.AskResponse
		ok   bool
	}{
		{"choose ok", &primitives.Ask{AskKind: primitives.AskChoose, Options: []primitives.AskOption{{ID: "a", Label: "A"}}}, &primitives.AskResponse{Choice: "a"}, true},
		{"choose empty", &primitives.Ask{AskKind: primitives.AskChoose}, &primitives.AskResponse{}, false},
		{"choose off-menu", &primitives.Ask{AskKind: primitives.AskChoose, Options: []primitives.AskOption{{ID: "a"}}}, &primitives.AskResponse{Choice: "z"}, false},
		{"choose free (no options)", &primitives.Ask{AskKind: primitives.AskChoose}, &primitives.AskResponse{Choice: "anything"}, true},
		{"input ok", &primitives.Ask{AskKind: primitives.AskInput}, &primitives.AskResponse{Value: "hello"}, true},
		{"input blank", &primitives.Ask{AskKind: primitives.AskInput}, &primitives.AskResponse{Value: "   "}, false},
		{"confirm true", &primitives.Ask{AskKind: primitives.AskConfirm}, &primitives.AskResponse{Confirmed: boolp(true)}, true},
		{"confirm false", &primitives.Ask{AskKind: primitives.AskConfirm}, &primitives.AskResponse{Confirmed: boolp(false)}, true},
		{"confirm missing", &primitives.Ask{AskKind: primitives.AskConfirm}, &primitives.AskResponse{}, false},
		{"sign via signature", &primitives.Ask{AskKind: primitives.AskSign}, &primitives.AskResponse{Signature: "0xdeadbeef"}, true},
		{"sign via confirm", &primitives.Ask{AskKind: primitives.AskSign}, &primitives.AskResponse{Confirmed: boolp(true)}, true},
		{"sign declined", &primitives.Ask{AskKind: primitives.AskSign}, &primitives.AskResponse{Confirmed: boolp(false)}, true},
		{"upload ok", &primitives.Ask{AskKind: primitives.AskUpload}, &primitives.AskResponse{UploadRef: "/media/x.png"}, true},
		{"upload blank", &primitives.Ask{AskKind: primitives.AskUpload}, &primitives.AskResponse{}, false},
		{"unknown kind", &primitives.Ask{AskKind: "bogus"}, &primitives.AskResponse{Value: "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateResponse(c.ask, c.resp)
			if c.ok && err != nil {
				t.Fatalf("expected ok, got error: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestValidateResponseNil(t *testing.T) {
	if err := ValidateResponse(nil, &primitives.AskResponse{}); err == nil {
		t.Fatal("nil ask should error")
	}
	if err := ValidateResponse(&primitives.Ask{AskKind: primitives.AskInput}, nil); err == nil {
		t.Fatal("nil response should error")
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		ask  *primitives.Ask
		resp *primitives.AskResponse
		want string
	}{
		{&primitives.Ask{AskKind: primitives.AskChoose, Options: []primitives.AskOption{{ID: "paxscan", Label: "Use Paxscan"}}}, &primitives.AskResponse{Choice: "paxscan"}, "Use Paxscan"},
		{&primitives.Ask{AskKind: primitives.AskInput}, &primitives.AskResponse{Value: "42"}, "42"},
		{&primitives.Ask{AskKind: primitives.AskConfirm}, &primitives.AskResponse{Confirmed: boolp(true)}, "confirmed"},
		{&primitives.Ask{AskKind: primitives.AskConfirm}, &primitives.AskResponse{Confirmed: boolp(false)}, "declined"},
		{&primitives.Ask{AskKind: primitives.AskUpload}, &primitives.AskResponse{UploadRef: "doc.pdf"}, "doc.pdf"},
	}
	for _, c := range cases {
		got := Summarize(c.ask, c.resp)
		if !strings.Contains(strings.ToLower(got), strings.ToLower(c.want)) {
			t.Fatalf("Summarize(%s) = %q; want it to contain %q", c.ask.AskKind, got, c.want)
		}
	}
}

func TestAnswered(t *testing.T) {
	orig := schema.NewAsk("ask:1", &primitives.Ask{
		AskKind: primitives.AskInput,
		Prompt:  "Your name?",
	})
	if err := orig.Validate(); err != nil {
		t.Fatalf("seed surface invalid: %v", err)
	}
	resp := &primitives.AskResponse{Value: "Neo"}
	patched, err := Answered(orig, resp)
	if err != nil {
		t.Fatalf("Answered: %v", err)
	}
	if err := patched.Validate(); err != nil {
		t.Fatalf("answered surface invalid: %v", err)
	}
	if patched.Ask.Response == nil || patched.Ask.Response.Value != "Neo" {
		t.Fatalf("response not folded onto the patch: %+v", patched.Ask.Response)
	}
	// The original must not have been mutated.
	if orig.Ask.Response != nil {
		t.Fatalf("Answered mutated the original surface")
	}
	if patched.ID != orig.ID {
		t.Fatalf("patch id %q != base id %q", patched.ID, orig.ID)
	}
}

func TestAnsweredRejectsNonAsk(t *testing.T) {
	notAsk := schema.NewNarration("n:1", &primitives.Narration{Text: "hi"})
	if _, err := Answered(notAsk, &primitives.AskResponse{Value: "x"}); err == nil {
		t.Fatal("Answered on a non-ask surface should error")
	}
}
