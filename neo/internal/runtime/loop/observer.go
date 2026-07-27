// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"sync"
)

type Reporter interface {
	Delta(turn int, channel, text string)
}

type ReporterObserver struct {
	mu       sync.Mutex
	reporter Reporter
	attempt  int
}

func NewReporterObserver(reporter Reporter, turn int) *ReporterObserver {
	return &ReporterObserver{reporter: reporter, attempt: turn}
}

func (observer *ReporterObserver) ContentDelta(
	_ context.Context,
	content string,
) error {
	observer.emit("content", content)
	return nil
}

func (observer *ReporterObserver) ReasoningDelta(
	_ context.Context,
	content string,
) error {
	observer.emit("reasoning", content)
	return nil
}

func (observer *ReporterObserver) Reset(context.Context) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.reporter != nil {
		observer.reporter.Delta(observer.attempt, "retraction", "")
	}
	observer.attempt++
	return nil
}

func (observer *ReporterObserver) CommitAttempt(context.Context) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.reporter != nil {
		observer.reporter.Delta(observer.attempt, "commit", "")
	}
	observer.attempt++
	return nil
}

func (observer *ReporterObserver) emit(channel, content string) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.reporter != nil && content != "" {
		observer.reporter.Delta(observer.attempt, channel, content)
	}
}
