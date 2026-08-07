// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package cloudchannels

import (
	"context"
	"sync"
	"time"
)

type WeixinPoller struct {
	api       *WeixinAPI
	store     *Store
	config    func() WeixinConfig
	handle    func(context.Context, WeixinMessage) error
	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	connected bool
	lastError string
	updatedAt time.Time
}

func NewWeixinPoller(api *WeixinAPI, store *Store, config func() WeixinConfig, handle func(context.Context, WeixinMessage) error) *WeixinPoller {
	return &WeixinPoller{api: api, store: store, config: config, handle: handle}
}
func (p *WeixinPoller) Start(parent context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel, p.done, p.updatedAt = cancel, make(chan struct{}), time.Now().UTC()
	done := p.done
	p.mu.Unlock()
	go func() { defer close(done); p.run(ctx) }()
}
func (p *WeixinPoller) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
func (p *WeixinPoller) Status() RuntimeStatus {
	if p == nil {
		return RuntimeStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return RuntimeStatus{Running: p.cancel != nil, Connected: p.connected, LastError: p.lastError, UpdatedAt: p.updatedAt}
}
func (p *WeixinPoller) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		config := p.config()
		if !config.Enabled || !config.Configured() {
			p.setStatus(false, "")
			return
		}
		updates, err := p.api.GetUpdates(ctx, config)
		if err == nil {
			p.setStatus(true, "")
			for _, message := range updates.Messages {
				if p.handle != nil {
					err = p.handle(ctx, message)
					if err != nil {
						break
					}
				}
			}
			if err == nil && updates.Cursor != "" {
				err = p.store.UpdateWeixinProgress(updates.Cursor, "", "", 0)
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.setStatus(false, err.Error())
			if !waitContext(ctx, jitter(backoff)) {
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		} else {
			backoff = time.Second
		}
	}
}
func (p *WeixinPoller) setStatus(connected bool, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connected = connected
	p.lastError = message
	p.updatedAt = time.Now().UTC()
}
