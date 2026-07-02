// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package delegate

import "errors"

// markedError carries an explicit FailureClass through a wrap chain. Cody's
// worker seam is in-process (no daemon HTTP), so failures that are known to
// be deterministic (a malformed sheet, a policy refusal) or conflicting are
// marked at the source instead of inferred from an HTTP status.
type markedError struct {
	class FailureClass
	err   error
}

func (e *markedError) Error() string { return e.err.Error() }
func (e *markedError) Unwrap() error { return e.err }

// Mark wraps err with an explicit failure class recoverable via ClassOf.
func Mark(class FailureClass, err error) error {
	if err == nil {
		return nil
	}
	return &markedError{class: class, err: err}
}

// classOfMarked recovers an explicit mark from the wrap chain; ok is false
// when err carries no mark.
func classOfMarked(err error) (FailureClass, bool) {
	var me *markedError
	if errors.As(err, &me) {
		return me.class, true
	}
	return ClassNone, false
}
