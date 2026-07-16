// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import "strings"

const episodicSweepMarker = "DEJA_VU_SWEEP"
const episodicSweepBatch = 128

func (e *Engine) MaybeHandleEpisodicSweep(message string) bool {
	if !strings.Contains(message, episodicSweepMarker) {
		return false
	}
	if e == nil || e.pager == nil || e.automatrixBusy() {
		return true
	}
	_, _ = e.pager.EpisodicBackfill(episodicSweepBatch)
	return true
}
