// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package worker

import (
	"crypto/sha256"
	"encoding/hex"
)

// Loop-detection bounds: a signature repeating loopMaxRepeats times inside the
// last loopWindow calls marks the worker stuck.
const (
	loopWindow     = 10
	loopMaxRepeats = 5
)

// loopGap is the honest gap recorded when the guard fires.
const loopGap = "worker stopped: the same tool call repeated with identical input and output " +
	"(loop detected) — the approach is not converging"

// loopDetector is the stuck-worker guard: it hashes every (tool, input,
// output) triple and stops the run when the identical call keeps producing
// the identical result. A repeated call whose OUTPUT changes (e.g. polling a
// background job as it makes progress) never trips it.
type loopDetector struct {
	sigs []string
}

// observe folds one tool call into the window and reports whether the worker
// is looping.
func (l *loopDetector) observe(tool, input, output string) bool {
	sum := sha256.Sum256([]byte(tool + "\x00" + input + "\x00" + output))
	sig := hex.EncodeToString(sum[:])
	l.sigs = append(l.sigs, sig)
	if len(l.sigs) > loopWindow {
		l.sigs = l.sigs[len(l.sigs)-loopWindow:]
	}
	n := 0
	for _, s := range l.sigs {
		if s == sig {
			n++
		}
	}
	return n >= loopMaxRepeats
}
