// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"centra/packages/construct/backchannel"
	"centra/packages/construct/schema/primitives"
	constructtransport "centra/packages/construct/transport"
	executortool "centra/executor/tool"
	"centra/agents/neo/internal/channelgateway"
	"centra/agents/neo/internal/telegramsettings"
)

var telegramTokenPattern = regexp.MustCompile(`^[0-9]{5,20}:[A-Za-z0-9_-]{20,200}$`)

const telegramOutboundMediaMaxBytes = 49 << 20

type telegramStatus struct {
	Available         bool   `json:"available"`
	Configured        bool   `json:"configured"`
	Connected         bool   `json:"connected"`
	Paired            bool   `json:"paired"`
	BotUsername       string `json:"bot_username,omitempty"`
	TelegramUsername  string `json:"telegram_username,omitempty"`
	PairingURL        string `json:"pairing_url,omitempty"`
	LastError         string `json:"last_error,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type telegramPending struct {
	kind   string
	runID  string
	nodeID string
	askID  string
	ask    *primitives.Ask
}

type telegramBridge struct {
	engine *Engine
	store  *telegramsettings.Store
	api    *telegramAPI

	mu           sync.Mutex
	rootCtx      context.Context
	workerCancel context.CancelFunc
	workerDone   chan struct{}
	generation   uint64
	connected    bool
	lastError    string
	watching     map[string]bool
	pending      map[int64]telegramPending
}

func newTelegramBridge(engine *Engine, store *telegramsettings.Store) *telegramBridge {
	return &telegramBridge{
		engine:   engine,
		store:    store,
		api:      newTelegramAPI(),
		watching: map[string]bool{},
		pending:  map[int64]telegramPending{},
	}
}

func (b *telegramBridge) Start(ctx context.Context) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.rootCtx = ctx
	b.mu.Unlock()
	b.startWorker()
}

func (b *telegramBridge) Stop() {
	if b == nil {
		return
	}
	b.stopWorker()
}

func (b *telegramBridge) Status() telegramStatus {
	if b == nil || b.store == nil {
		return telegramStatus{Available: false, UnavailableReason: "Telegram is not configured on this Neo daemon"}
	}
	state := b.store.View()
	b.mu.Lock()
	connected, lastError := b.connected, b.lastError
	b.mu.Unlock()
	lastError = redactTelegramToken(lastError, state.Token)
	status := telegramStatus{
		Available:        b.store.Secure(),
		Configured:       state.Configured(),
		Connected:        state.Configured() && connected,
		Paired:           state.Paired(),
		BotUsername:      state.BotUsername,
		TelegramUsername: state.TelegramUsername,
		LastError:        lastError,
	}
	if !status.Available {
		status.UnavailableReason = "Neo's encrypted user vault is not available; the bot token was not saved"
	}
	if state.Configured() && !state.Paired() && state.PairSecret != "" {
		status.PairingURL = "https://t.me/" + state.BotUsername + "?start=" + state.PairSecret
	}
	return status
}

func (b *telegramBridge) Configure(ctx context.Context, token string) (telegramStatus, error) {
	if b == nil || b.store == nil {
		return telegramStatus{}, errors.New("Telegram is not available on this Neo daemon")
	}
	if !b.store.Secure() {
		return b.Status(), telegramsettings.ErrEncryptionRequired
	}
	token = strings.TrimSpace(token)
	if !telegramTokenPattern.MatchString(token) {
		return b.Status(), errors.New("the bot token format is invalid; copy the complete token from BotFather")
	}
	validateCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	bot, err := b.api.getMe(validateCtx, token)
	if err != nil {
		return b.Status(), err
	}
	if !bot.IsBot || bot.ID == 0 || strings.TrimSpace(bot.Username) == "" {
		return b.Status(), errors.New("Telegram accepted the token but it does not identify a usable bot account")
	}
	if err := b.api.deleteWebhook(validateCtx, token); err != nil {
		return b.Status(), fmt.Errorf("could not switch the bot to Centra AI message delivery: %w", err)
	}
	pairSecret, err := randomTelegramSecret()
	if err != nil {
		return b.Status(), fmt.Errorf("create pairing link: %w", err)
	}

	previous := b.store.View()
	b.stopWorker()
	if err := b.store.Replace(telegramsettings.State{
		Token:       token,
		BotID:       bot.ID,
		BotUsername: strings.TrimPrefix(bot.Username, "@"),
		PairSecret:  pairSecret,
	}); err != nil {
		if previous.Configured() {
			b.startWorker()
		}
		return b.Status(), err
	}
	b.mu.Lock()
	b.connected = true
	b.lastError = ""
	b.pending = map[int64]telegramPending{}
	b.mu.Unlock()
	b.startWorker()
	return b.Status(), nil
}

func (b *telegramBridge) Disconnect() error {
	if b == nil || b.store == nil {
		return nil
	}
	previous := b.store.View()
	b.stopWorker()
	if err := b.store.Clear(); err != nil {
		if previous.Configured() {
			b.startWorker()
		}
		return err
	}
	b.mu.Lock()
	b.connected = false
	b.lastError = ""
	b.pending = map[int64]telegramPending{}
	b.mu.Unlock()
	return nil
}

func (b *telegramBridge) startWorker() {
	if b == nil || b.store == nil {
		return
	}
	state := b.store.View()
	if !state.Configured() {
		return
	}
	b.mu.Lock()
	if b.workerCancel != nil {
		b.mu.Unlock()
		return
	}
	parent := b.rootCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	b.generation++
	generation := b.generation
	b.workerCancel = cancel
	b.workerDone = done
	b.mu.Unlock()
	go b.poll(ctx, done, generation, state.Token, state.LastUpdateID+1)
}

func (b *telegramBridge) stopWorker() {
	b.mu.Lock()
	cancel, done := b.workerCancel, b.workerDone
	b.workerCancel = nil
	b.workerDone = nil
	b.generation++
	b.connected = false
	b.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

func (b *telegramBridge) poll(ctx context.Context, done chan struct{}, generation uint64, token string, offset int64) {
	defer close(done)
	backoff := time.Second
	for {
		updates, err := b.api.getUpdates(ctx, token, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.setWorkerState(generation, false, err.Error())
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		b.setWorkerState(generation, true, "")
		backoff = time.Second
		b.drainOutbound(ctx, generation, token)
		for _, update := range updates {
			if !b.workerCurrent(generation) {
				return
			}
			if err := b.processUpdate(ctx, generation, token, update); err != nil && ctx.Err() == nil {
				b.setWorkerState(generation, true, err.Error())
				break
			}
			current, err := b.setLastUpdateID(generation, update.UpdateID)
			if !current {
				return
			}
			if err != nil {
				b.setWorkerState(generation, false, err.Error())
				break
			}
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
		}
	}
}

func (b *telegramBridge) workerCurrent(generation uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return generation == b.generation
}

func (b *telegramBridge) currentState(generation uint64) (telegramsettings.State, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return telegramsettings.State{}, false
	}
	return b.store.View(), true
}

func (b *telegramBridge) setLastUpdateID(generation uint64, updateID int64) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return false, nil
	}
	return true, b.store.SetLastUpdateID(updateID)
}

func (b *telegramBridge) setWorkerState(generation uint64, connected bool, lastError string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return
	}
	b.connected = connected
	b.lastError = strings.TrimSpace(lastError)
}

func (b *telegramBridge) processUpdate(ctx context.Context, generation uint64, token string, update telegramUpdate) error {
	state, current := b.currentState(generation)
	if !current {
		return nil
	}
	envelope, normalized := normalizeTelegramUpdate(state, update)
	if normalized && b.engine != nil && b.engine.channelGateway != nil {
		if state.Paired() && state.ConversationID != "" {
			_ = b.engine.channelGateway.Bind(ctx, envelope.Address, state.ConversationID)
		}
		claim, err := b.engine.channelGateway.ClaimInbound(ctx, envelope)
		if errors.Is(err, channelgateway.ErrIdempotencyConflict) {
			return err
		}
		if err == nil && claim.State == channelgateway.ClaimDuplicate && claim.Status == "completed" {
			return nil
		}
	}
	var err error
	if update.CallbackQuery != nil {
		err = b.processCallback(ctx, generation, token, update.CallbackQuery)
	} else if update.Message != nil {
		err = b.processMessage(ctx, generation, token, update.Message, fmt.Sprintf("telegram:%d", update.UpdateID))
	}
	if normalized && b.engine != nil && b.engine.channelGateway != nil {
		currentState, _ := b.currentState(generation)
		if err != nil {
			_ = b.engine.channelGateway.FailInbound(ctx, envelope, err.Error())
		} else {
			if currentState.Paired() && currentState.ConversationID != "" {
				_ = b.engine.channelGateway.Bind(ctx, envelope.Address, currentState.ConversationID)
			}
			_ = b.engine.channelGateway.CompleteInbound(ctx, envelope, currentState.ConversationID, "")
		}
	}
	return err
}

func (b *telegramBridge) processMessage(ctx context.Context, generation uint64, token string, message *telegramMessage, idempotencyKey string) error {
	if message == nil || message.From == nil || message.Chat.Type != "private" {
		return nil
	}
	state, current := b.currentState(generation)
	if !current {
		return nil
	}
	command, argument := parseTelegramCommand(message.Text)
	if !state.Paired() {
		if command != "start" {
			return nil
		}
		paired, current, err := b.pair(generation, argument, message, telegramConversationID(state.BotID, message.Chat.ID))
		if !current {
			return nil
		}
		if err != nil {
			return b.sendText(ctx, token, message.Chat.ID, "Centra AI could not complete pairing: "+err.Error(), nil)
		}
		if !paired {
			return b.sendText(ctx, token, message.Chat.ID, "This pairing link is invalid or has expired. Create a new link in Centra AI Settings.", nil)
		}
		return b.sendText(ctx, token, message.Chat.ID, "Connected to Neo. Send a message here whenever you want to talk. Use /new for a fresh conversation or /stop to stop the current task.", nil)
	}
	if state.ChatID != message.Chat.ID || state.TelegramUserID != message.From.ID {
		return nil
	}

	content, uploadRef, err := b.telegramMessageContent(ctx, token, message)
	if err != nil {
		return b.sendText(ctx, token, message.Chat.ID, "I could not receive that attachment: "+err.Error(), nil)
	}
	switch command {
	case "start", "help":
		return b.sendText(ctx, token, message.Chat.ID, "Send any message to talk to Neo. /new starts a fresh conversation, /stop stops the current task, and /status checks the connection.", nil)
	case "status":
		return b.sendText(ctx, token, message.Chat.ID, "Neo is connected and ready.", nil)
	case "new":
		conversationID, err := newTelegramConversationID(state.BotID, state.ChatID)
		if err != nil {
			return b.sendText(ctx, token, message.Chat.ID, "I could not start a new conversation: "+err.Error(), nil)
		}
		current, err := b.rotateConversation(generation, conversationID)
		if !current {
			return nil
		}
		if err != nil {
			return b.sendText(ctx, token, message.Chat.ID, "I could not start a new conversation: "+err.Error(), nil)
		}
		b.clearPendingChat(generation, state.ChatID)
		return b.sendText(ctx, token, message.Chat.ID, "Started a fresh Neo conversation.", nil)
	case "stop":
		if state.ConversationID == "" {
			return b.sendText(ctx, token, message.Chat.ID, "There is no active Neo task to stop.", nil)
		}
		runID := b.interruptConversation(generation, state.ConversationID)
		if runID == "" {
			return b.sendText(ctx, token, message.Chat.ID, "There is no active Neo task to stop.", nil)
		}
		b.clearPendingChat(generation, state.ChatID)
		return b.sendText(ctx, token, message.Chat.ID, "Stopping the current Neo task.", nil)
	}
	if handled, err := b.answerPendingText(ctx, generation, token, state, content, uploadRef); handled {
		return err
	}

	content = strings.TrimSpace(content)
	if content == "" {
		return b.sendText(ctx, token, message.Chat.ID, "Send text, a photo, or a file and I will pass it to Neo.", nil)
	}
	runID, conversationID, current, err := b.submitMessage(ctx, generation, state.ChatID, state.TelegramUserID, content, idempotencyKey)
	if !current {
		return nil
	}
	if err != nil {
		return err
	}
	b.watchRun(ctx, generation, token, state.ChatID, runID, conversationID)
	return nil
}

func (b *telegramBridge) pair(generation uint64, secret string, message *telegramMessage, conversationID string) (bool, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return false, false, nil
	}
	paired, err := b.store.Pair(secret, message.Chat.ID, message.From.ID, message.From.Username, conversationID)
	if err == nil && paired {
		b.lastError = ""
	}
	return paired, true, err
}

func (b *telegramBridge) rotateConversation(generation uint64, conversationID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return false, nil
	}
	return true, b.store.RotateConversation(conversationID)
}

func (b *telegramBridge) interruptConversation(generation uint64, conversationID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return ""
	}
	return b.engine.sessions.get(conversationID).interruptActive()
}

func (b *telegramBridge) submitMessage(ctx context.Context, generation uint64, chatID, userID int64, content, idempotencyKey string) (string, string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return "", "", false, nil
	}
	state := b.store.View()
	if !state.Paired() || state.ChatID != chatID || state.TelegramUserID != userID {
		return "", "", false, nil
	}
	conversationID := state.ConversationID
	if conversationID == "" {
		conversationID = telegramConversationID(state.BotID, state.ChatID)
		if err := b.store.RotateConversation(conversationID); err != nil {
			return "", "", true, err
		}
	}
	runID, _, _, err := b.engine.submitNormalizedMessage(ctx, channelgateway.Envelope{
		Direction: channelgateway.Inbound,
		Kind:      channelgateway.KindMessage,
		Address: channelgateway.Address{
			Channel: channelgateway.ChannelTelegram, AccountID: fmt.Sprintf("%d", state.BotID),
			ConversationID: fmt.Sprintf("%d", chatID), ParticipantID: fmt.Sprintf("%d", userID), Scope: channelgateway.ScopeDirect,
		},
		NeoConversation: conversationID, IdempotencyKey: idempotencyKey, Text: content, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		return "", "", true, err
	}
	return runID, conversationID, true, nil
}

func (b *telegramBridge) processCallback(ctx context.Context, generation uint64, token string, callback *telegramCallbackQuery) error {
	if callback == nil || callback.Message == nil {
		return nil
	}
	state, current := b.currentState(generation)
	if !current {
		return nil
	}
	if !state.Paired() || callback.Message.Chat.ID != state.ChatID || callback.From.ID != state.TelegramUserID {
		return nil
	}
	b.mu.Lock()
	pending, ok := b.pending[state.ChatID]
	b.mu.Unlock()
	if !ok {
		return b.api.answerCallback(ctx, token, callback.ID, "That request is no longer active.")
	}
	var answerText string
	switch {
	case pending.kind == "gate" && callback.Data == "mx:approve":
		if !b.answerGate(generation, pending, true) {
			return b.api.answerCallback(ctx, token, callback.ID, "That approval has expired.")
		}
		answerText = "Approved"
	case pending.kind == "gate" && callback.Data == "mx:deny":
		if !b.answerGate(generation, pending, false) {
			return b.api.answerCallback(ctx, token, callback.ID, "That approval has expired.")
		}
		answerText = "Denied"
	case pending.kind == "ask" && strings.HasPrefix(callback.Data, "mx:ask:"):
		index := -1
		_, _ = fmt.Sscanf(strings.TrimPrefix(callback.Data, "mx:ask:"), "%d", &index)
		response, label, err := telegramAskCallbackResponse(pending.ask, index)
		if err != nil {
			return b.api.answerCallback(ctx, token, callback.ID, err.Error())
		}
		if err := b.deliverAskResponse(generation, pending, response); err != nil {
			return b.api.answerCallback(ctx, token, callback.ID, err.Error())
		}
		answerText = label
	default:
		return b.api.answerCallback(ctx, token, callback.ID, "That action is no longer active.")
	}
	b.clearPending(generation, state.ChatID, pending)
	if err := b.api.answerCallback(ctx, token, callback.ID, answerText); err != nil {
		return err
	}
	return b.sendText(ctx, token, state.ChatID, answerText+". Neo is continuing.", nil)
}

func (b *telegramBridge) answerGate(generation uint64, pending telegramPending, approved bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return false
	}
	run := b.engine.lookupRun(pending.runID)
	return run != nil && run.sess.answerGate(pending.nodeID, gateAnswer{approved: approved})
}

func (b *telegramBridge) watchRun(ctx context.Context, generation uint64, token string, chatID int64, runID, conversationID string) {
	b.mu.Lock()
	if generation != b.generation || b.watching[runID] {
		b.mu.Unlock()
		return
	}
	b.watching[runID] = true
	b.mu.Unlock()
	go b.followRun(ctx, generation, token, chatID, runID, conversationID)
}

func (b *telegramBridge) followRun(ctx context.Context, generation uint64, token string, chatID int64, runID, conversationID string) {
	defer func() {
		b.mu.Lock()
		delete(b.watching, runID)
		b.mu.Unlock()
		for attempt := 0; attempt < 6; attempt++ {
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if next := b.engine.activeRunForConv(conversationID); next != "" && next != runID {
				b.watchRun(ctx, generation, token, chatID, next, conversationID)
				return
			}
		}
	}()
	replay, live, cancel := b.engine.broker.subscribe(runID, 0)
	defer cancel()
	for _, event := range replay {
		b.deliverEvent(ctx, generation, token, chatID, runID, event)
	}
	if live == nil {
		return
	}
	typing := time.NewTicker(4 * time.Second)
	defer typing.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-typing.C:
			_ = b.api.sendChatAction(ctx, token, chatID)
		case event, ok := <-live:
			if !ok {
				return
			}
			b.deliverEvent(ctx, generation, token, chatID, runID, event)
		}
	}
}

func (b *telegramBridge) deliverEvent(ctx context.Context, generation uint64, token string, chatID int64, runID string, event Event) {
	if !b.workerCurrent(generation) {
		return
	}
	switch event.Type {
	case "chat.assistant":
		text, _ := event.Fields["text"].(string)
		if strings.TrimSpace(text) != "" {
			if err := b.sendTextKey(ctx, token, chatID, text, nil, fmt.Sprintf("telegram:%s:%d:text", runID, event.Seq)); err != nil {
				b.setWorkerState(generation, true, err.Error())
			}
		}
	case "tool.media":
		if err := b.sendRunMedia(ctx, token, chatID, runID, event); err != nil {
			b.setWorkerState(generation, true, err.Error())
		}
	case "gate.invoked":
		nodeID, _ := event.Fields["node_id"].(string)
		question, _ := event.Fields["question"].(string)
		pending := telegramPending{kind: "gate", runID: runID, nodeID: nodeID}
		if !b.setPending(generation, chatID, pending) {
			return
		}
		keyboard := &telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{
			{Text: "Approve", CallbackData: "mx:approve"},
			{Text: "Deny", CallbackData: "mx:deny"},
		}}}
		_ = b.sendTextKey(ctx, token, chatID, strings.TrimSpace(question), keyboard, fmt.Sprintf("telegram:%s:%d:gate", runID, event.Seq))
	case "construct.surface":
		surface, err := constructtransport.SurfaceFromEvent(event.Fields)
		if err != nil || surface.Ask == nil {
			return
		}
		pending := telegramPending{kind: "ask", runID: runID, askID: surface.ID, ask: surface.Ask}
		if !b.setPending(generation, chatID, pending) {
			return
		}
		_ = b.sendTextKey(ctx, token, chatID, surface.Ask.Prompt, telegramAskKeyboard(surface.Ask), fmt.Sprintf("telegram:%s:%d:ask", runID, event.Seq))
	}
}

func (b *telegramBridge) answerPendingText(ctx context.Context, generation uint64, token string, state telegramsettings.State, text, uploadRef string) (bool, error) {
	b.mu.Lock()
	pending, ok := b.pending[state.ChatID]
	b.mu.Unlock()
	if !ok {
		return false, nil
	}
	trimmed := strings.TrimSpace(text)
	if pending.kind == "gate" {
		switch strings.ToLower(trimmed) {
		case "approve", "approved", "yes", "y":
			if !b.answerGate(generation, pending, true) {
				return true, b.sendText(ctx, token, state.ChatID, "That approval is no longer active.", nil)
			}
			b.clearPending(generation, state.ChatID, pending)
			return true, b.sendText(ctx, token, state.ChatID, "Approved. Neo is continuing.", nil)
		case "deny", "denied", "no", "n":
			if !b.answerGate(generation, pending, false) {
				return true, b.sendText(ctx, token, state.ChatID, "That approval is no longer active.", nil)
			}
			b.clearPending(generation, state.ChatID, pending)
			return true, b.sendText(ctx, token, state.ChatID, "Denied. Neo is continuing.", nil)
		default:
			return true, b.sendText(ctx, token, state.ChatID, "Reply Approve or Deny, or use the buttons above.", nil)
		}
	}
	if pending.ask == nil {
		return false, nil
	}
	response, err := telegramAskTextResponse(pending.ask, trimmed, uploadRef)
	if err != nil {
		return true, b.sendText(ctx, token, state.ChatID, err.Error(), nil)
	}
	if err := b.deliverAskResponse(generation, pending, response); err != nil {
		return true, b.sendText(ctx, token, state.ChatID, "That answer was not accepted: "+err.Error(), nil)
	}
	b.clearPending(generation, state.ChatID, pending)
	return true, b.sendText(ctx, token, state.ChatID, "Answer received. Neo is continuing.", nil)
}

func telegramAskTextResponse(ask *primitives.Ask, text, uploadRef string) (*primitives.AskResponse, error) {
	if ask == nil {
		return nil, errors.New("that question is no longer active")
	}
	response := &primitives.AskResponse{}
	switch ask.AskKind {
	case primitives.AskInput:
		response.Value = text
	case primitives.AskSign:
		switch strings.ToLower(text) {
		case "yes", "y", "confirm", "confirmed", "approve", "approved":
			value := true
			response.Confirmed = &value
		case "no", "n", "deny", "denied", "decline", "declined":
			value := false
			response.Confirmed = &value
		default:
			response.Signature = text
		}
	case primitives.AskUpload:
		response.UploadRef = uploadRef
	case primitives.AskChoose:
		for _, option := range ask.Options {
			if strings.EqualFold(text, option.ID) || strings.EqualFold(text, option.Label) {
				response.Choice = option.ID
				break
			}
		}
	case primitives.AskConfirm:
		var value bool
		switch strings.ToLower(text) {
		case "yes", "y", "confirm", "confirmed", "approve", "approved":
			value = true
		case "no", "n", "deny", "denied", "decline", "declined":
			value = false
		default:
			return nil, errors.New("Reply Yes or No, or use the buttons above.")
		}
		response.Confirmed = &value
	}
	return response, nil
}

func (b *telegramBridge) deliverAskResponse(generation uint64, pending telegramPending, response *primitives.AskResponse) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return errors.New("that question is no longer active")
	}
	run := b.engine.lookupRun(pending.runID)
	if run == nil {
		return errors.New("that question is no longer active")
	}
	ask, ok := run.sess.pendingAsk(pending.askID)
	if !ok {
		return errors.New("that question is no longer active")
	}
	if err := backchannel.ValidateResponse(ask, response); err != nil {
		return err
	}
	if !run.sess.answerAsk(pending.askID, response) {
		return errors.New("that question was already answered or expired")
	}
	return nil
}

func (b *telegramBridge) setPending(generation uint64, chatID int64, pending telegramPending) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return false
	}
	b.pending[chatID] = pending
	return true
}

func (b *telegramBridge) clearPending(generation uint64, chatID int64, expected telegramPending) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return
	}
	current, ok := b.pending[chatID]
	if ok && current.kind == expected.kind && current.runID == expected.runID && current.nodeID == expected.nodeID && current.askID == expected.askID {
		delete(b.pending, chatID)
	}
}

func (b *telegramBridge) clearPendingChat(generation uint64, chatID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation == b.generation {
		delete(b.pending, chatID)
	}
}

func (b *telegramBridge) telegramMessageContent(ctx context.Context, token string, message *telegramMessage) (string, string, error) {
	text := strings.TrimSpace(message.Text)
	if text == "" {
		text = strings.TrimSpace(message.Caption)
	}
	attachment, ok := selectTelegramAttachment(message)
	if !ok {
		return text, "", nil
	}
	ref, kind, err := b.storeTelegramAttachment(ctx, token, attachment)
	if err != nil {
		return "", "", err
	}
	marker := fmt.Sprintf("[attached %s: %s]", kind, ref)
	return strings.TrimSpace(strings.Join([]string{text, marker}, "\n\n")), ref, nil
}

type selectedTelegramAttachment struct {
	fileID   string
	fileName string
	mimeType string
	fileSize int64
}

func selectTelegramAttachment(message *telegramMessage) (selectedTelegramAttachment, bool) {
	if message == nil {
		return selectedTelegramAttachment{}, false
	}
	if n := len(message.Photo); n > 0 {
		photo := message.Photo[n-1]
		return selectedTelegramAttachment{fileID: photo.FileID, fileName: "telegram-photo.jpg", mimeType: "image/jpeg", fileSize: photo.FileSize}, true
	}
	for _, file := range []*telegramFileAttachment{message.Document, message.Video, message.Audio, message.Voice, message.Sticker} {
		if file != nil && file.FileID != "" {
			name := file.FileName
			if name == "" {
				name = "telegram-upload"
			}
			return selectedTelegramAttachment{fileID: file.FileID, fileName: name, mimeType: file.MimeType, fileSize: file.FileSize}, true
		}
	}
	return selectedTelegramAttachment{}, false
}

func (b *telegramBridge) storeTelegramAttachment(ctx context.Context, token string, attachment selectedTelegramAttachment) (string, string, error) {
	if b.engine == nil || b.engine.mediaDir == "" {
		return "", "", errors.New("media storage is not configured on this Neo daemon")
	}
	if attachment.fileSize > uploadMaxBytes {
		return "", "", fmt.Errorf("the file is larger than the %d MB upload limit", uploadMaxBytes>>20)
	}
	file, err := b.api.getFile(ctx, token, attachment.fileID)
	if err != nil {
		return "", "", err
	}
	if file.FilePath == "" {
		return "", "", errors.New("Telegram did not provide a downloadable file")
	}
	if file.FileSize > uploadMaxBytes {
		return "", "", fmt.Errorf("the file is larger than the %d MB upload limit", uploadMaxBytes>>20)
	}
	body, contentLength, err := b.api.downloadFile(ctx, token, file.FilePath)
	if err != nil {
		return "", "", err
	}
	defer body.Close()
	if contentLength > uploadMaxBytes {
		return "", "", fmt.Errorf("the file is larger than the %d MB upload limit", uploadMaxBytes>>20)
	}
	if err := os.MkdirAll(b.engine.mediaDir, 0o700); err != nil {
		return "", "", errors.New("could not prepare Neo's media storage")
	}
	ext := extForUpload(attachment.fileName, attachment.mimeType)
	name := mintMediaID() + ext
	destination, err := os.OpenFile(filepath.Join(b.engine.mediaDir, name), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", errors.New("could not store the Telegram attachment")
	}
	limited := io.LimitReader(body, uploadMaxBytes+1)
	var written int64
	if b.engine.mediaSealEnabled() {
		written, err = sealMediaCopy(b.engine, name, destination, limited)
	} else {
		written, err = io.Copy(destination, limited)
	}
	closeErr := destination.Close()
	if err != nil || closeErr != nil || written > uploadMaxBytes {
		_ = os.Remove(filepath.Join(b.engine.mediaDir, name))
		if written > uploadMaxBytes {
			return "", "", fmt.Errorf("the file is larger than the %d MB upload limit", uploadMaxBytes>>20)
		}
		return "", "", errors.New("could not finish storing the Telegram attachment")
	}
	mimeType := mimeForName("x" + ext)
	return "/media/" + name, kindForMIME(mimeType), nil
}

func (b *telegramBridge) sendRunMedia(ctx context.Context, token string, chatID int64, runID string, event Event) error {
	if b.engine == nil || b.engine.channelDispatch == nil {
		return b.sendRunMediaDirect(ctx, token, chatID, event)
	}
	ref, _ := event.Fields["url"].(string)
	kind, _ := event.Fields["kind"].(string)
	envelope := b.outboundEnvelope(chatID, channelgateway.KindMessage, fmt.Sprintf("telegram:%s:%d:media", runID, event.Seq))
	envelope.Media = []channelgateway.Media{{Kind: mediaKindForMIME(mimeForName(ref)), Ref: ref}}
	envelope.Metadata = map[string]string{"media_ref": ref, "media_kind": kind}
	_, err := b.engine.channelDispatch.Dispatch(ctx, envelope, telegramOutboundSender{bridge: b, token: token})
	var deliveryErr *channelgateway.DeliveryError
	if errors.As(err, &deliveryErr) {
		return nil
	}
	if err != nil {
		return b.sendRunMediaDirect(ctx, token, chatID, event)
	}
	return nil
}

func (b *telegramBridge) sendRunMediaDirect(ctx context.Context, token string, chatID int64, event Event) error {
	ref, _ := event.Fields["url"].(string)
	kind, _ := event.Fields["kind"].(string)
	name, data, err := b.readRunMedia(ref)
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "image":
		return b.api.sendPhoto(ctx, token, chatID, name, data)
	case "video":
		return b.api.sendVideo(ctx, token, chatID, name, data)
	default:
		return errors.New("Telegram cannot deliver this generated media type")
	}
}

func (b *telegramBridge) readRunMedia(ref string) (string, []byte, error) {
	if b.engine == nil || b.engine.mediaDir == "" {
		return "", nil, errors.New("media storage is not configured on this Neo daemon")
	}
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "/media/") {
		return "", nil, errors.New("generated media is not stored on this Neo daemon")
	}
	name := safeMediaName(strings.TrimPrefix(ref, "/media/"))
	if name == "" || ref != "/media/"+name {
		return "", nil, errors.New("generated media reference is invalid")
	}
	file, err := os.Open(filepath.Join(b.engine.mediaDir, name))
	if err != nil {
		return "", nil, errors.New("generated media is no longer available")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return "", nil, errors.New("generated media is no longer available")
	}
	var reader io.Reader = file
	sealed, err := sniffVaultFile(file)
	if err != nil {
		return "", nil, errors.New("generated media could not be read")
	}
	if sealed {
		if b.engine.vault == nil {
			return "", nil, errors.New("generated media requires the encrypted user vault")
		}
		userVault := b.engine.vault.UserVault()
		if userVault == nil {
			return "", nil, errors.New("generated media requires the encrypted user vault")
		}
		stream, err := userVault.StreamReader(file, b.engine.mediaAD(name))
		if err != nil {
			return "", nil, errors.New("generated media could not be decrypted")
		}
		reader = stream
	} else if info.Size() > telegramOutboundMediaMaxBytes {
		return "", nil, fmt.Errorf("generated media is larger than Telegram's %d MB upload limit", telegramOutboundMediaMaxBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(reader, telegramOutboundMediaMaxBytes+1))
	if err != nil {
		return "", nil, errors.New("generated media could not be read")
	}
	if len(data) > telegramOutboundMediaMaxBytes {
		return "", nil, fmt.Errorf("generated media is larger than Telegram's %d MB upload limit", telegramOutboundMediaMaxBytes>>20)
	}
	return name, data, nil
}

func (b *telegramBridge) sendText(ctx context.Context, token string, chatID int64, text string, keyboard *telegramInlineKeyboard) error {
	return b.sendTextKey(ctx, token, chatID, text, keyboard, "telegram:out:"+mintMediaID())
}

func (b *telegramBridge) sendTextKey(ctx context.Context, token string, chatID int64, text string, keyboard *telegramInlineKeyboard, key string) error {
	if b.engine == nil || b.engine.channelDispatch == nil {
		return b.sendTextDirect(ctx, token, chatID, text, keyboard)
	}
	envelope := b.outboundEnvelope(chatID, channelgateway.KindMessage, key)
	envelope.Text = strings.TrimSpace(text)
	if keyboard != nil {
		encoded, err := json.Marshal(keyboard)
		if err != nil {
			return err
		}
		envelope.Metadata = map[string]string{"telegram_keyboard": string(encoded)}
	}
	_, err := b.engine.channelDispatch.Dispatch(ctx, envelope, telegramOutboundSender{bridge: b, token: token})
	var deliveryErr *channelgateway.DeliveryError
	if errors.As(err, &deliveryErr) {
		return nil
	}
	if err != nil {
		return b.sendTextDirect(ctx, token, chatID, text, keyboard)
	}
	return nil
}

func (b *telegramBridge) outboundEnvelope(chatID int64, kind channelgateway.Kind, key string) channelgateway.Envelope {
	state := b.store.View()
	return channelgateway.Envelope{
		Direction: channelgateway.Outbound,
		Kind:      kind,
		Address: channelgateway.Address{
			Channel: channelgateway.ChannelTelegram, AccountID: fmt.Sprintf("%d", state.BotID),
			ConversationID: fmt.Sprintf("%d", chatID), ParticipantID: fmt.Sprintf("%d", state.TelegramUserID), Scope: channelgateway.ScopeDirect,
		},
		NeoConversation: state.ConversationID,
		IdempotencyKey:  key,
		SideEffectClass: executortool.SideEffectNetwork,
		OccurredAt:      time.Now().UTC(),
	}
}

func (b *telegramBridge) sendTextDirect(ctx context.Context, token string, chatID int64, text string, keyboard *telegramInlineKeyboard) error {
	parts := splitTelegramText(text, 4000)
	if len(parts) == 0 {
		return nil
	}
	for index, part := range parts {
		var buttons *telegramInlineKeyboard
		if index == len(parts)-1 {
			buttons = keyboard
		}
		if err := b.api.sendMessage(ctx, token, chatID, part, buttons); err != nil {
			return err
		}
	}
	return nil
}

type telegramOutboundSender struct {
	bridge *telegramBridge
	token  string
}

func (s telegramOutboundSender) Send(ctx context.Context, envelope channelgateway.Envelope) (channelgateway.SendReceipt, error) {
	chatID, err := strconv.ParseInt(envelope.Address.ConversationID, 10, 64)
	if err != nil {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "address", Message: "Telegram chat identity is invalid", Permanent: true}
	}
	if ref := envelope.Metadata["media_ref"]; ref != "" {
		event := Event{Fields: map[string]interface{}{"url": ref, "kind": envelope.Metadata["media_kind"]}}
		if err := s.bridge.sendRunMediaDirect(ctx, s.token, chatID, event); err != nil {
			return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "telegram", Message: err.Error()}
		}
		return channelgateway.SendReceipt{Code: "accepted"}, nil
	}
	var keyboard *telegramInlineKeyboard
	if encoded := envelope.Metadata["telegram_keyboard"]; encoded != "" {
		keyboard = &telegramInlineKeyboard{}
		if err := json.Unmarshal([]byte(encoded), keyboard); err != nil {
			return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "payload", Message: "Telegram approval controls are invalid", Permanent: true}
		}
	}
	if err := s.bridge.sendTextDirect(ctx, s.token, chatID, envelope.Text, keyboard); err != nil {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "telegram", Message: err.Error()}
	}
	return channelgateway.SendReceipt{Code: "accepted"}, nil
}

func (b *telegramBridge) drainOutbound(ctx context.Context, generation uint64, token string) {
	if b.engine == nil || b.engine.channelDispatch == nil {
		return
	}
	state, current := b.currentState(generation)
	if !current || !state.Configured() {
		return
	}
	_, err := b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelTelegram, fmt.Sprintf("%d", state.BotID), telegramOutboundSender{bridge: b, token: token}, 20)
	if err != nil && ctx.Err() == nil {
		b.setWorkerState(generation, true, err.Error())
	}
}

func telegramAskKeyboard(ask *primitives.Ask) *telegramInlineKeyboard {
	if ask == nil {
		return nil
	}
	var labels []string
	switch ask.AskKind {
	case primitives.AskChoose:
		for _, option := range ask.Options {
			labels = append(labels, option.Label)
		}
	case primitives.AskConfirm, primitives.AskSign:
		labels = []string{"Yes", "No"}
	default:
		return nil
	}
	rows := make([][]telegramInlineButton, 0, len(labels))
	for index, label := range labels {
		rows = append(rows, []telegramInlineButton{{Text: label, CallbackData: fmt.Sprintf("mx:ask:%d", index)}})
	}
	return &telegramInlineKeyboard{InlineKeyboard: rows}
}

func telegramAskCallbackResponse(ask *primitives.Ask, index int) (*primitives.AskResponse, string, error) {
	if ask == nil {
		return nil, "", errors.New("that question is no longer active")
	}
	response := &primitives.AskResponse{}
	switch ask.AskKind {
	case primitives.AskChoose:
		if index < 0 || index >= len(ask.Options) {
			return nil, "", errors.New("that option is not available")
		}
		response.Choice = ask.Options[index].ID
		return response, ask.Options[index].Label, nil
	case primitives.AskConfirm:
		value := index == 0
		response.Confirmed = &value
		if value {
			return response, "Confirmed", nil
		}
		return response, "Declined", nil
	case primitives.AskSign:
		value := index == 0
		response.Confirmed = &value
		if value {
			return response, "Signing approved", nil
		}
		return response, "Signing declined", nil
	default:
		return nil, "", errors.New("reply to the message instead")
	}
}

func normalizeTelegramUpdate(state telegramsettings.State, update telegramUpdate) (channelgateway.Envelope, bool) {
	var message *telegramMessage
	kind := channelgateway.KindMessage
	if update.CallbackQuery != nil {
		message = update.CallbackQuery.Message
		kind = channelgateway.KindApproval
	} else {
		message = update.Message
	}
	if message == nil {
		return channelgateway.Envelope{}, false
	}
	scope := channelgateway.ScopeGroup
	if message.Chat.Type == "private" {
		scope = channelgateway.ScopeDirect
	}
	participantID := ""
	if message.From != nil {
		participantID = fmt.Sprintf("%d", message.From.ID)
	}
	text := strings.TrimSpace(message.Text)
	if text == "" {
		text = strings.TrimSpace(message.Caption)
	}
	command, _ := parseTelegramCommand(text)
	switch command {
	case "stop":
		kind = channelgateway.KindInterrupt
	case "new":
		kind = channelgateway.KindSteer
	}
	envelope := channelgateway.Envelope{
		Direction: channelgateway.Inbound,
		Kind:      kind,
		Address: channelgateway.Address{
			Channel:        channelgateway.ChannelTelegram,
			AccountID:      fmt.Sprintf("%d", state.BotID),
			ConversationID: fmt.Sprintf("%d", message.Chat.ID),
			ParticipantID:  participantID,
			Scope:          scope,
		},
		NeoConversation:   state.ConversationID,
		ExternalEventID:   fmt.Sprintf("%d", update.UpdateID),
		ExternalMessageID: fmt.Sprintf("%d", message.MessageID),
		IdempotencyKey:    fmt.Sprintf("telegram:%d", update.UpdateID),
		Text:              text,
		OccurredAt:        time.Now().UTC(),
	}
	if update.CallbackQuery != nil {
		envelope.Approval = &channelgateway.Approval{
			ID: update.CallbackQuery.ID, Prompt: "Telegram approval response", Decision: update.CallbackQuery.Data,
		}
		envelope.Text = update.CallbackQuery.Data
		return envelope, true
	}
	if attachment, ok := selectTelegramAttachment(message); ok {
		envelope.Media = append(envelope.Media, channelgateway.Media{
			Kind: mediaKindForMIME(attachment.mimeType), Ref: "telegram:" + attachment.fileID,
			Name: attachment.fileName, MIMEType: attachment.mimeType, Size: attachment.fileSize,
		})
	}
	return envelope, true
}

func mediaKindForMIME(mimeType string) channelgateway.MediaKind {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return channelgateway.MediaImage
	case strings.HasPrefix(mimeType, "audio/"):
		return channelgateway.MediaAudio
	case strings.HasPrefix(mimeType, "video/"):
		return channelgateway.MediaVideo
	default:
		return channelgateway.MediaFile
	}
}

func parseTelegramCommand(text string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", ""
	}
	command := strings.TrimPrefix(strings.ToLower(fields[0]), "/")
	if at := strings.Index(command, "@"); at >= 0 {
		command = command[:at]
	}
	argument := ""
	if len(fields) > 1 {
		argument = fields[1]
	}
	return command, argument
}

func randomTelegramSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func telegramConversationID(botID, chatID int64) string {
	return fmt.Sprintf("tg-%d-%d", botID, chatID)
}

func newTelegramConversationID(botID, chatID int64) (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return fmt.Sprintf("tg-%d-%d-%s", botID, chatID, hex.EncodeToString(raw)), nil
}

func splitTelegramText(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" || limit <= 0 {
		return nil
	}
	runes := []rune(text)
	parts := make([]string, 0, len(runes)/limit+1)
	for len(runes) > limit {
		cut := limit
		for index := limit; index > limit/2; index-- {
			if runes[index-1] == '\n' || runes[index-1] == ' ' {
				cut = index
				break
			}
		}
		parts = append(parts, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
	}
	if tail := strings.TrimSpace(string(runes)); tail != "" {
		parts = append(parts, tail)
	}
	return parts
}

func validUTF8TelegramText(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	return strings.ToValidUTF8(text, "�")
}
