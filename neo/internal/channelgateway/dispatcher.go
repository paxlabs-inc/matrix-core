// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package channelgateway

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type RetryPolicy struct {
	MinimumInterval time.Duration
	BaseBackoff     time.Duration
	MaximumBackoff  time.Duration
	MaximumAttempts int
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.MinimumInterval < 0 {
		p.MinimumInterval = 0
	}
	if p.BaseBackoff <= 0 {
		p.BaseBackoff = time.Second
	}
	if p.MaximumBackoff < p.BaseBackoff {
		p.MaximumBackoff = 2 * time.Minute
	}
	if p.MaximumAttempts <= 0 || p.MaximumAttempts > 20 {
		p.MaximumAttempts = 8
	}
	return p
}

type Dispatcher struct {
	store  *Store
	policy RetryPolicy
}

func NewDispatcher(store *Store, policy RetryPolicy) *Dispatcher {
	return &Dispatcher{store: store, policy: policy.normalized()}
}

// Dispatch persists before sending. A failed send remains durable and can be
// drained after restart by the adapter that owns the external credentials.
func (d *Dispatcher) Dispatch(ctx context.Context, envelope Envelope, sender Sender) (Delivery, error) {
	if d == nil || d.store == nil {
		return Delivery{}, errors.New("channel dispatcher is disabled")
	}
	item, _, err := d.store.QueueOutbound(ctx, envelope)
	if err != nil {
		return Delivery{}, err
	}
	if item.State == DeliveryDelivered || item.State == DeliveryFailed {
		return item, nil
	}
	return d.attempt(ctx, item.ID, sender)
}

func (d *Dispatcher) Drain(ctx context.Context, channel Channel, accountID string, sender Sender, limit int) ([]Delivery, error) {
	if d == nil || d.store == nil {
		return nil, nil
	}
	items, err := d.store.Due(ctx, channel, accountID, limit)
	if err != nil {
		return nil, err
	}
	results := make([]Delivery, 0, len(items))
	for _, item := range items {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		result, attemptErr := d.attempt(ctx, item.ID, sender)
		results = append(results, result)
		if attemptErr != nil {
			var deliveryErr *DeliveryError
			if !errors.As(attemptErr, &deliveryErr) {
				return results, attemptErr
			}
		}
	}
	return results, nil
}

func (d *Dispatcher) attempt(ctx context.Context, id string, sender Sender) (Delivery, error) {
	if sender == nil {
		return Delivery{}, errors.New("channel sender is required")
	}
	item, wait, err := d.store.BeginAttempt(ctx, id, d.policy.MinimumInterval)
	if err != nil {
		return Delivery{}, err
	}
	if item.State == DeliveryDelivered || item.State == DeliveryFailed {
		return item, nil
	}
	if wait > 0 {
		next := time.Now().UTC().Add(wait)
		if err := d.store.MarkRetry(ctx, id, "channel rate limit", "rate_limited", next); err != nil {
			return Delivery{}, err
		}
		updated, readErr := d.store.Delivery(ctx, id)
		return updated, readErr
	}
	receipt, sendErr := sender.Send(ctx, item.Envelope)
	if sendErr == nil {
		if err := d.store.MarkDelivered(ctx, id, receipt); err != nil {
			return Delivery{}, err
		}
		return d.store.Delivery(ctx, id)
	}
	var classified *DeliveryError
	if !errors.As(sendErr, &classified) {
		classified = &DeliveryError{Code: "transport", Message: sendErr.Error()}
	}
	if classified.Permanent || item.Attempts >= d.policy.MaximumAttempts {
		if err := d.store.MarkFailed(ctx, id, classified.Error(), classified.Code); err != nil {
			return Delivery{}, err
		}
		updated, readErr := d.store.Delivery(ctx, id)
		if readErr != nil {
			return Delivery{}, readErr
		}
		return updated, classified
	}
	backoff := classified.RetryAfter
	if backoff <= 0 {
		backoff = d.policy.BaseBackoff << max(item.Attempts-1, 0)
	}
	if backoff > d.policy.MaximumBackoff {
		backoff = d.policy.MaximumBackoff
	}
	if err := d.store.MarkRetry(ctx, id, classified.Error(), classified.Code, time.Now().UTC().Add(backoff)); err != nil {
		return Delivery{}, err
	}
	updated, readErr := d.store.Delivery(ctx, id)
	if readErr != nil {
		return Delivery{}, readErr
	}
	return updated, classified
}

func (d *Dispatcher) String() string {
	if d == nil || d.store == nil {
		return "channel dispatcher disabled"
	}
	return fmt.Sprintf("channel dispatcher retries=%d", d.policy.MaximumAttempts)
}
