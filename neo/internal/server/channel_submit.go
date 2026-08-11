// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"matrix/neo/internal/channelgateway"
	"matrix/neo/internal/runrecord"
)

// submitNormalizedMessage is the one channel-adapter submission seam. It
// deliberately delegates to the existing session registry and idempotent
// submit operation; it does not own a loop, context, completion, or delivery.
func (e *Engine) submitNormalizedMessage(ctx context.Context, envelope channelgateway.Envelope) (runID, conversationID string, duplicate bool, err error) {
	if e == nil || e.sessions == nil {
		return "", "", false, errors.New("Neo session service is unavailable")
	}
	if envelope.Direction != channelgateway.Inbound || envelope.Kind != channelgateway.KindMessage {
		return "", "", false, errors.New("normalized submission requires an inbound message")
	}
	if err := envelope.Validate(); err != nil {
		return "", "", false, err
	}
	conversationID = strings.TrimSpace(envelope.NeoConversation)
	if conversationID == "" {
		conversationID = channelConversationID(envelope.Address)
	}
	content := normalizedMessageText(envelope)
	if content == "" {
		return "", "", false, errors.New("normalized message content is empty")
	}
	e.beginTurnBoundary(ctx)
	defer e.endTurnBoundary()
	session := e.sessions.get(conversationID)
	observeCompletion, armObservation := e.improvementCompletionObserver(conversationID)
	var fresh bool
	runID, fresh, duplicate, err = session.submitIdempotent(content, envelope.IdempotencyKey, observeCompletion)
	if err != nil {
		return "", conversationID, false, err
	}
	armObservation(runID, fresh)
	if !duplicate {
		e.conv.AppendUser(conversationID, runID, content)
	}
	if e.channelGateway != nil {
		_ = e.channelGateway.Bind(ctx, envelope.Address, conversationID)
	}
	return runID, conversationID, duplicate, nil
}

func (e *Engine) acceptNormalizedMessage(ctx context.Context, envelope channelgateway.Envelope) (runID, conversationID string, duplicate bool, err error) {
	if e != nil && e.channelGateway != nil {
		claim, claimErr := e.channelGateway.ClaimInbound(ctx, envelope)
		if claimErr != nil {
			if errors.Is(claimErr, channelgateway.ErrIdempotencyConflict) {
				return "", "", false, claimErr
			}
		} else if claim.State == channelgateway.ClaimDuplicate && claim.Status == "completed" {
			return claim.RunID, claim.NeoConversation, true, nil
		}
	}
	runID, conversationID, duplicate, err = e.submitNormalizedMessage(ctx, envelope)
	if e != nil && e.channelGateway != nil {
		if err != nil {
			_ = e.channelGateway.FailInbound(ctx, envelope, err.Error())
		} else {
			_ = e.channelGateway.CompleteInbound(ctx, envelope, conversationID, runID)
		}
	}
	return runID, conversationID, duplicate, err
}

func normalizedMessageText(envelope channelgateway.Envelope) string {
	parts := []string{strings.TrimSpace(envelope.Text)}
	for _, media := range envelope.Media {
		parts = append(parts, "[attached "+string(media.Kind)+": "+strings.TrimSpace(media.Ref)+"]")
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func channelConversationID(address channelgateway.Address) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		string(address.Channel), address.AccountID, address.ConversationID,
	}, "\x00")))
	return "channel-" + string(address.Channel) + "-" + hex.EncodeToString(digest[:12])
}

func channelSubmitStatus(err error) int {
	if errors.Is(err, runrecord.ErrIdempotencyConflict) || errors.Is(err, channelgateway.ErrIdempotencyConflict) {
		return 409
	}
	return 500
}
