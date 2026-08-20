// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"testing"
)

func TestValidateMutationCreate(t *testing.T) {
	err := ValidateMutation(MutationRequest{
		Kind:    MutateCreate,
		Path:    "/tmp/test.txt",
		Content: "hello",
	}, 1024)
	if err != nil {
		t.Fatalf("valid create should pass: %v", err)
	}
}

func TestValidateMutationCreateMissingContent(t *testing.T) {
	err := ValidateMutation(MutationRequest{
		Kind: MutateCreate,
		Path: "/tmp/test.txt",
	}, 1024)
	if err == nil {
		t.Fatal("create without content should fail")
	}
}

func TestValidateMutationOversized(t *testing.T) {
	err := ValidateMutation(MutationRequest{
		Kind:    MutateAppend,
		Path:    "/tmp/test.txt",
		Content: "this is way too long for the budget",
	}, 10)
	if err == nil {
		t.Fatal("oversized mutation should fail")
	}
}

func TestValidateMutationMissingPath(t *testing.T) {
	err := ValidateMutation(MutationRequest{Kind: MutateCreate}, 1024)
	if err == nil {
		t.Fatal("missing path should fail")
	}
}

func TestValidateMutationReplaceRequiresOffset(t *testing.T) {
	err := ValidateMutation(MutationRequest{
		Kind:    MutateReplace,
		Path:    "/tmp/test.txt",
		Content: "new content",
		Offset:  -1,
	}, 1024)
	if err == nil {
		t.Fatal("negative offset should fail")
	}
}

func TestValidateMutationRejectsUnboundedWriteAndInvalidRanges(t *testing.T) {
	write := MutationRequest{Kind: MutateAppend, Path: "/tmp/test.txt", Content: "x"}
	if err := ValidateMutation(write, 0); err == nil {
		t.Fatal("write without a positive bound should fail")
	}
	read := MutationRequest{Kind: MutateReadRange, Path: "/tmp/test.txt"}
	if err := ValidateMutation(read, 1024); err == nil {
		t.Fatal("read_range without a positive length should fail")
	}
}

func TestValidateMutationFinalizeAndRollbackRequireStableIdentity(t *testing.T) {
	if err := ValidateMutation(MutationRequest{Kind: MutateFinalize, Path: "/tmp/test.txt"}, 1024); err == nil {
		t.Fatal("finalize without hash and idempotency key should fail")
	}
	if err := ValidateMutation(MutationRequest{Kind: MutateRollback, Path: "/tmp/test.txt"}, 1024); err == nil {
		t.Fatal("rollback without idempotency key should fail")
	}
}

func TestContentHashDeterministic(t *testing.T) {
	h1 := ContentHash([]byte("hello world"))
	h2 := ContentHash([]byte("hello world"))
	if h1 != h2 {
		t.Fatal("same content should produce same hash")
	}
	h3 := ContentHash([]byte("hello world!"))
	if h1 == h3 {
		t.Fatal("different content should produce different hash")
	}
}
