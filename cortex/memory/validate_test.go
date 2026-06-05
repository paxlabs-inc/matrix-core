// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newPreferenceMemory(t *testing.T) (*Head, *Version, TypedData) {
	t.Helper()
	d := PreferenceData{SchemaVersion: 1, Topic: "tone", Polarity: PolarityPrefer, StrengthVal: 0.7}
	enc, err := EncodeData(d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	id := NewID()
	h := &Head{
		ID:             id,
		Type:           TypePreference,
		CurrentVersion: 1,
		ActorScope:     "andrew",
		Visibility:     VisPrivate,
		LastUpdatedAt:  time.Unix(1, 0).UTC(),
	}
	v := &Version{
		ID:         id,
		Version:    1,
		Type:       TypePreference,
		Data:       enc,
		CreatedAt:  time.Unix(1, 0).UTC(),
		CreatedBy:  "andrew",
		Confidence: 1.0,
		Provenance: Provenance{Source: SourceUserInput},
		Forms:      Forms{Short: "prefers tone=terse"},
	}
	return h, v, d
}

func TestValidateMemoryHappyPath(t *testing.T) {
	h, v, d := newPreferenceMemory(t)
	if err := ValidateMemory(h, v, d); err != nil {
		t.Fatalf("happy path failed: %v", err)
	}
}

func TestValidateRejectsHeadVersionTypeMismatch(t *testing.T) {
	h, v, d := newPreferenceMemory(t)
	v.Type = TypeBelief
	err := ValidateMemory(h, v, d)
	if err == nil || !strings.Contains(err.Error(), "head.Type") {
		t.Fatalf("expected head/version type mismatch, got %v", err)
	}
}

func TestValidateRejectsBadVisibility(t *testing.T) {
	h, v, d := newPreferenceMemory(t)
	h.Visibility = 0
	if err := ValidateMemory(h, v, d); !errors.Is(err, ErrInvalidVisibility) {
		t.Fatalf("expected ErrInvalidVisibility, got %v", err)
	}
}

func TestValidateRejectsBadConfidence(t *testing.T) {
	h, v, d := newPreferenceMemory(t)
	v.Confidence = 1.5
	if err := ValidateMemory(h, v, d); err == nil {
		t.Fatalf("expected confidence error")
	}
}

func TestValidateRejectsTooManyTags(t *testing.T) {
	h, v, d := newPreferenceMemory(t)
	for i := 0; i <= MaxTagsPerMemory; i++ {
		h.Tags = append(h.Tags, Tag("t"))
	}
	if err := ValidateMemory(h, v, d); !errors.Is(err, ErrTooManyTags) {
		t.Fatalf("expected ErrTooManyTags, got %v", err)
	}
}

func TestValidateRejectsLongTag(t *testing.T) {
	h, v, d := newPreferenceMemory(t)
	h.Tags = []Tag{Tag(strings.Repeat("x", MaxTagLen+1))}
	if err := ValidateMemory(h, v, d); !errors.Is(err, ErrTagTooLong) {
		t.Fatalf("expected ErrTagTooLong, got %v", err)
	}
}

func TestValidateRejectsLongShortForm(t *testing.T) {
	h, v, d := newPreferenceMemory(t)
	v.Forms.Short = strings.Repeat("token ", MaxShortTokens+1)
	err := ValidateMemory(h, v, d)
	if err == nil || !errors.Is(err, ErrFormTooLong) {
		t.Fatalf("expected ErrFormTooLong, got %v", err)
	}
}

func TestValidateRejectsMissingTypedField(t *testing.T) {
	d := PreferenceData{SchemaVersion: 1, Polarity: PolarityPrefer, StrengthVal: 0.5}
	enc, err := EncodeData(d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	h := &Head{ID: NewID(), Type: TypePreference, CurrentVersion: 1,
		ActorScope: "x", Visibility: VisPrivate, LastUpdatedAt: time.Unix(1, 0).UTC()}
	v := &Version{ID: h.ID, Version: 1, Type: TypePreference, Data: enc,
		CreatedAt: time.Unix(1, 0).UTC(), CreatedBy: "x", Confidence: 1,
		Provenance: Provenance{Source: SourceUserInput}}
	err = ValidateMemory(h, v, d)
	if err == nil || !strings.Contains(err.Error(), "Preference.topic required") {
		t.Fatalf("expected topic-required error, got %v", err)
	}
}

func TestValidateRejectsTypeDataMismatch(t *testing.T) {
	h, v, _ := newPreferenceMemory(t)
	other := IdentityData{SchemaVersion: 1, Name: "x"}
	if err := ValidateMemory(h, v, other); !errors.Is(err, ErrTypeDataMismatch) {
		t.Fatalf("expected ErrTypeDataMismatch, got %v", err)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
