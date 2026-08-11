// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package chronos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type LoopbackTarget struct {
	URL        string
	Capability string
	Client     *http.Client
}

func (target LoopbackTarget) Deliver(ctx context.Context, delivery Delivery) error {
	endpoint, err := url.Parse(strings.TrimSpace(target.URL))
	if err != nil || endpoint.Scheme != "http" || endpoint.Host == "" || !isLoopbackHost(endpoint.Hostname()) {
		return fmt.Errorf("local chronos: delivery target must be a loopback HTTP URL")
	}
	if strings.TrimSpace(target.Capability) == "" {
		return fmt.Errorf("local chronos: delivery capability is required")
	}
	body, err := json.Marshal(struct {
		AlarmID        string          `json:"alarm_id"`
		ScheduledFor   time.Time       `json:"scheduled_for"`
		ConversationID string          `json:"conversation_id,omitempty"`
		Payload        json.RawMessage `json:"payload"`
		WakeMessage    string          `json:"wake_message"`
		Message        string          `json:"message"`
	}{delivery.Alarm.ID, delivery.ScheduledFor, delivery.Alarm.Body.ConversationID,
		delivery.Alarm.Body.Payload, delivery.Alarm.Body.WakeMessage, delivery.Alarm.Body.WakeMessage})
	if err != nil {
		return fmt.Errorf("local chronos: encode delivery: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+target.Capability)
	client := target.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("local chronos: deliver wake: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("local chronos: target returned HTTP %d", response.StatusCode)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
