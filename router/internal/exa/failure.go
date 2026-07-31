// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"errors"
	"fmt"
	"time"
)

type FailureKind string

const (
	FailureNotConfigured FailureKind = "not_configured"
	FailureRateLimited   FailureKind = "rate_limited"
	FailureUpstream      FailureKind = "upstream"
	FailureTimeout       FailureKind = "timeout"
	FailureNotFound      FailureKind = "not_found"
	FailureBadRequest    FailureKind = "bad_request"
	FailurePartial       FailureKind = "partial"
	FailureUngrounded    FailureKind = "ungrounded"
	FailureConflict      FailureKind = "conflict"
)

type Failure struct {
	Kind       FailureKind
	Endpoint   string
	Status     int
	Message    string
	Detail     string
	RetryAfter time.Duration
}

func (f *Failure) Error() string {
	if f == nil {
		return "<nil exa failure>"
	}
	if f.Detail != "" {
		return fmt.Sprintf("exa %s: %s (%s)", f.Endpoint, f.Message, f.Detail)
	}
	return fmt.Sprintf("exa %s: %s", f.Endpoint, f.Message)
}

func FailureOf(err error) *Failure {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure
	}
	return nil
}

func KindOf(err error) FailureKind {
	if err == nil {
		return ""
	}
	if failure := FailureOf(err); failure != nil {
		return failure.Kind
	}
	return FailureUpstream
}
