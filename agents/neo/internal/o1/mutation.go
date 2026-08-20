// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// MutationKind describes the class of file/artifact mutation.
// Per O1 req.5 ac.1: bounded create, read-range, append, replace-range,
// structured patch, atomic finalize, and rollback.
type MutationKind string

const (
	MutateCreate    MutationKind = "create"
	MutateReadRange MutationKind = "read_range"
	MutateAppend    MutationKind = "append"
	MutateReplace   MutationKind = "replace_range"
	MutatePatch     MutationKind = "patch"
	MutateFinalize  MutationKind = "finalize"
	MutateRollback  MutationKind = "rollback"
)

// MutationRequest is a bounded, invariant-preserving description of a
// file or artifact change. Per O1 req.5: the model expresses intent to
// mutate; deterministic operations own the mechanics.
type MutationRequest struct {
	Kind           MutationKind `json:"kind"`
	Path           string       `json:"path"`
	Offset         int64        `json:"offset,omitempty"`
	Length         int64        `json:"length,omitempty"`
	Content        string       `json:"content,omitempty"`
	Hash           string       `json:"expected_hash,omitempty"`
	MaxBytes       int64        `json:"max_bytes,omitempty"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
}

// MutationResult is the deterministic outcome of a mutation.
type MutationResult struct {
	OK           bool   `json:"ok"`
	Path         string `json:"path"`
	BytesWritten int64  `json:"bytes_written"`
	NewHash      string `json:"new_hash,omitempty"`
	Err          string `json:"error,omitempty"`
	Checkpoint   string `json:"checkpoint,omitempty"`
}

// ValidateMutation checks a mutation request for structural validity before
// execution. Per O1 req.5 ac.1 and ac.2: reject oversized payloads and
// invalid ranges before model-visible execution.
func ValidateMutation(req MutationRequest, maxPayloadBytes int64) error {
	if strings.TrimSpace(req.Path) == "" {
		return fmt.Errorf("mutation missing path")
	}
	if !filepath.IsAbs(req.Path) || filepath.Clean(req.Path) != req.Path {
		return fmt.Errorf("mutation path must be absolute and clean")
	}
	if req.Offset < 0 || req.Length < 0 {
		return fmt.Errorf("%s mutation requires non-negative offset and length", req.Kind)
	}
	if req.MaxBytes < 0 {
		return fmt.Errorf("%s mutation max_bytes must be non-negative", req.Kind)
	}
	limit := maxPayloadBytes
	if req.MaxBytes > 0 && (limit <= 0 || req.MaxBytes < limit) {
		limit = req.MaxBytes
	}
	switch req.Kind {
	case MutateCreate, MutateAppend, MutateReplace, MutatePatch:
		if len(req.Content) == 0 {
			return fmt.Errorf("%s mutation requires content", req.Kind)
		}
		if limit <= 0 {
			return fmt.Errorf("%s mutation requires a positive payload limit", req.Kind)
		}
		if int64(len(req.Content)) > limit {
			return fmt.Errorf("%s mutation content is %d bytes; limit is %d",
				req.Kind, len(req.Content), limit)
		}
	case MutateReadRange:
		if req.Length <= 0 {
			return fmt.Errorf("read_range requires a positive length")
		}
	case MutateFinalize:
		if req.Hash == "" || req.IdempotencyKey == "" {
			return fmt.Errorf("finalize requires expected_hash and idempotency_key")
		}
	case MutateRollback:
		if req.IdempotencyKey == "" {
			return fmt.Errorf("rollback requires idempotency_key")
		}
	default:
		return fmt.Errorf("unknown mutation kind: %s", req.Kind)
	}
	if req.Kind == MutateReplace && req.Length <= 0 {
		return fmt.Errorf("replace_range requires a positive length")
	}
	return nil
}

// ContentHash computes the SHA-256 of content for integrity verification.
func ContentHash(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}

// MutationCheckpoint captures resumable state for a multi-step mutation.
type MutationCheckpoint struct {
	OperationID string `json:"operation_id"`
	Path        string `json:"path"`
	BytesDone   int64  `json:"bytes_done"`
	Hash        string `json:"hash"`
	NextOffset  int64  `json:"next_offset"`
}

// IdempotencyRecord tracks a mutation's idempotency key to prevent duplicate
// application. Per O1 req.5 ac.4: stable idempotency, expected prior state,
// authorization, effect class, and postcondition checks.
type IdempotencyRecord struct {
	Key       string `json:"key"`
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Hash      string `json:"result_hash"`
	Applied   bool   `json:"applied"`
}
