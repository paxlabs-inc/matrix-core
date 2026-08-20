// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

// Package channelgateway owns the durable, channel-neutral edge around Neo's
// existing conversational server. It intentionally does not own an agent loop,
// session state, context assembly, or answer completion.
package channelgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"centra/executor/tool"
)

type Channel string

const (
	ChannelWeb         Channel = "web"
	ChannelTelegram    Channel = "telegram"
	ChannelMachineMail Channel = "machinemail"
	ChannelSlack       Channel = "slack"
	ChannelDiscord     Channel = "discord"
	ChannelLark        Channel = "lark"
	ChannelDingTalk    Channel = "dingtalk"
	ChannelWeComBot    Channel = "wecom_bot"
	ChannelWeComApp    Channel = "wecom_app"
	ChannelQQ          Channel = "qq"
	ChannelWeixin      Channel = "weixin"
	ChannelWeChatMP    Channel = "wechat_official"
	ChannelWeChatKF    Channel = "wechat_customer_service"
)

type Scope string

const (
	ScopeDirect Scope = "direct"
	ScopeGroup  Scope = "group"
)

type Direction string

const (
	Inbound  Direction = "inbound"
	Outbound Direction = "outbound"
)

type Kind string

const (
	KindMessage   Kind = "message"
	KindTyping    Kind = "typing"
	KindProgress  Kind = "progress"
	KindApproval  Kind = "approval"
	KindInterrupt Kind = "interrupt"
	KindSteer     Kind = "steer"
	KindReceipt   Kind = "receipt"
)

type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaAudio MediaKind = "audio"
	MediaVideo MediaKind = "video"
	MediaFile  MediaKind = "file"
)

type Address struct {
	Channel        Channel `json:"channel"`
	AccountID      string  `json:"account_id"`
	ConversationID string  `json:"conversation_id"`
	ParticipantID  string  `json:"participant_id,omitempty"`
	Scope          Scope   `json:"scope"`
}

type Media struct {
	Kind     MediaKind `json:"kind"`
	Ref      string    `json:"ref"`
	Name     string    `json:"name,omitempty"`
	MIMEType string    `json:"mime_type,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Width    int       `json:"width,omitempty"`
	Height   int       `json:"height,omitempty"`
}

type Quote struct {
	ExternalMessageID string `json:"external_message_id,omitempty"`
	Text              string `json:"text,omitempty"`
}

type Approval struct {
	ID       string   `json:"id"`
	Prompt   string   `json:"prompt"`
	Options  []string `json:"options,omitempty"`
	Decision string   `json:"decision,omitempty"`
}

type Progress struct {
	Stage   string `json:"stage"`
	Detail  string `json:"detail,omitempty"`
	Current int64  `json:"current,omitempty"`
	Total   int64  `json:"total,omitempty"`
}

type Envelope struct {
	ID                string            `json:"id"`
	Direction         Direction         `json:"direction"`
	Kind              Kind              `json:"kind"`
	Address           Address           `json:"address"`
	NeoConversation   string            `json:"neo_conversation_id,omitempty"`
	RunID             string            `json:"run_id,omitempty"`
	ExternalEventID   string            `json:"external_event_id,omitempty"`
	ExternalMessageID string            `json:"external_message_id,omitempty"`
	IdempotencyKey    string            `json:"idempotency_key"`
	Text              string            `json:"text,omitempty"`
	Media             []Media           `json:"media,omitempty"`
	Quote             *Quote            `json:"quote,omitempty"`
	Approval          *Approval         `json:"approval,omitempty"`
	Progress          *Progress         `json:"progress,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	SideEffectClass   string            `json:"side_effect_class,omitempty"`
	OccurredAt        time.Time         `json:"occurred_at"`
}

func (e Envelope) Validate() error {
	if strings.TrimSpace(string(e.Address.Channel)) == "" {
		return errors.New("channel is required")
	}
	if strings.TrimSpace(e.Address.AccountID) == "" {
		return errors.New("account_id is required")
	}
	if strings.TrimSpace(e.Address.ConversationID) == "" {
		return errors.New("conversation_id is required")
	}
	if e.Address.Scope != ScopeDirect && e.Address.Scope != ScopeGroup {
		return errors.New("scope must be direct or group")
	}
	if e.Direction != Inbound && e.Direction != Outbound {
		return errors.New("direction must be inbound or outbound")
	}
	switch e.Kind {
	case KindMessage, KindTyping, KindProgress, KindApproval, KindInterrupt, KindSteer, KindReceipt:
	default:
		return fmt.Errorf("unsupported envelope kind %q", e.Kind)
	}
	if strings.TrimSpace(e.IdempotencyKey) == "" {
		return errors.New("idempotency_key is required")
	}
	if e.Kind == KindMessage && strings.TrimSpace(e.Text) == "" && len(e.Media) == 0 {
		return errors.New("message requires text or media")
	}
	if e.Kind == KindApproval && e.Approval == nil {
		return errors.New("approval envelope requires approval")
	}
	if e.Kind == KindProgress && e.Progress == nil {
		return errors.New("progress envelope requires progress")
	}
	if e.Direction == Outbound && !tool.ValidSideEffectClasses[strings.TrimSpace(e.SideEffectClass)] {
		return errors.New("outbound envelope requires a valid side_effect_class")
	}
	for _, media := range e.Media {
		if media.Kind != MediaImage && media.Kind != MediaAudio && media.Kind != MediaVideo && media.Kind != MediaFile {
			return fmt.Errorf("unsupported media kind %q", media.Kind)
		}
		if strings.TrimSpace(media.Ref) == "" {
			return errors.New("media ref is required")
		}
		if media.Size < 0 {
			return errors.New("media size cannot be negative")
		}
	}
	return nil
}

type ClaimState string

const (
	ClaimNew       ClaimState = "new"
	ClaimDuplicate ClaimState = "duplicate"
)

type InboundClaim struct {
	State           ClaimState `json:"state"`
	EnvelopeID      string     `json:"envelope_id"`
	NeoConversation string     `json:"neo_conversation_id,omitempty"`
	RunID           string     `json:"run_id,omitempty"`
	Status          string     `json:"status"`
}

type DeliveryState string

const (
	DeliveryQueued    DeliveryState = "queued"
	DeliverySending   DeliveryState = "sending"
	DeliveryDelivered DeliveryState = "delivered"
	DeliveryRetrying  DeliveryState = "retrying"
	DeliveryFailed    DeliveryState = "failed"
)

type Delivery struct {
	ID                string        `json:"id"`
	Envelope          Envelope      `json:"envelope"`
	State             DeliveryState `json:"state"`
	Attempts          int           `json:"attempts"`
	NextAttemptAt     time.Time     `json:"next_attempt_at,omitempty"`
	ExternalMessageID string        `json:"external_message_id,omitempty"`
	ReceiptCode       string        `json:"receipt_code,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

type SendReceipt struct {
	ExternalMessageID string
	Code              string
}

type PendingAction struct {
	Address   Address   `json:"address"`
	Kind      Kind      `json:"kind"`
	RunID     string    `json:"run_id"`
	NodeID    string    `json:"node_id"`
	CreatedAt time.Time `json:"created_at"`
}

type DeliveryError struct {
	Code       string
	Message    string
	Permanent  bool
	RetryAfter time.Duration
}

func (e *DeliveryError) Error() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.Message)
}

type Sender interface {
	Send(ctx context.Context, envelope Envelope) (SendReceipt, error)
}
