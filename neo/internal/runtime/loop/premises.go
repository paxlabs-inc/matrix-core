// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"strings"
	"sync"
)

type PremiseSet struct {
	mu       sync.RWMutex
	premises []string
}

func (set *PremiseSet) Replace(premises []string) {
	normalized := make([]string, 0, len(premises))
	for _, premise := range premises {
		if premise = strings.TrimSpace(premise); premise != "" {
			normalized = append(normalized, premise)
		}
	}
	set.mu.Lock()
	set.premises = normalized
	set.mu.Unlock()
}

func (set *PremiseSet) ActivePremises() []string {
	if set == nil {
		return nil
	}
	set.mu.RLock()
	defer set.mu.RUnlock()
	return append([]string(nil), set.premises...)
}
