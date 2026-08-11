// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package primitives

// AskKind is the type of blocking request-for-human (frozen [vocabulary.ask]:
// "kind in {choose | input | confirm | sign | upload}"). It is the ONE
// inherently bidirectional primitive — the back-channel anchor (invariant i5).
type AskKind string

const (
	AskChoose  AskKind = "choose"
	AskInput   AskKind = "input"
	AskConfirm AskKind = "confirm"
	AskSign    AskKind = "sign"
	AskUpload  AskKind = "upload"
)

// AskKindValues is the frozen, ordered set of ask kinds.
var AskKindValues = []AskKind{AskChoose, AskInput, AskConfirm, AskSign, AskUpload}

// ValidAskKind reports whether k is a known ask kind.
func ValidAskKind(k AskKind) bool {
	switch k {
	case AskChoose, AskInput, AskConfirm, AskSign, AskUpload:
		return true
	default:
		return false
	}
}

// AskOption is one selectable choice for an AskChoose request.
type AskOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// AskResponse is the typed human reply to an Ask. It is filled in when the
// human answers and flows back to the agent as an INPUT on the same footing as
// a user message (replayable, deterministic) — NEVER a mutation of plan/walk/
// cortex. The relevant field is set per AskKind: Choice (choose), Value
// (input), Confirmed (confirm), Signature (sign), UploadRef (upload).
type AskResponse struct {
	Choice     string `json:"choice,omitempty"`
	Value      string `json:"value,omitempty"`
	Confirmed  *bool  `json:"confirmed,omitempty"`
	Signature  string `json:"signature,omitempty"`
	UploadRef  string `json:"upload_ref,omitempty"`
	AnsweredAt string `json:"answered_at,omitempty"`
}

// Ask is the blocking, typed request-for-human (axis: action ->
// request-for-human). It is the half the industry has zero answer for beyond a
// text reply (frozen [vocabulary.ask].role, load_bearing).
type Ask struct {
	// AskKind is the request type (choose|input|confirm|sign|upload). Named
	// AskKind rather than Kind because Kind is the surface envelope
	// discriminator.
	AskKind AskKind `json:"ask_kind"`
	// Prompt is the human-facing question/instruction.
	Prompt string `json:"prompt"`
	// Options are the selectable choices for a choose request.
	Options []AskOption `json:"options,omitempty"`
	// Expected describes the expected response (e.g. input type "text"|
	// "number"; for sign, a reference to the entity/tx being signed).
	Expected string `json:"expected,omitempty"`
	// Required marks the ask as mandatory (the run blocks until answered).
	Required bool `json:"required,omitempty"`
	// Response is the typed human answer, set once the ask is answered.
	Response *AskResponse `json:"response,omitempty"`
}
