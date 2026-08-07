// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package channelgateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"matrix/executor/tool"
	"matrix/vault"
)

func gatewayVault(t *testing.T, root, user string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{Required: true, DataDir: root, UserDID: user, KEKHex: kek})
	if err != nil {
		t.Fatalf("boot vault: %v", err)
	}
	return session
}

func inboundEnvelope() Envelope {
	return Envelope{
		Direction: Inbound, Kind: KindMessage,
		Address:         Address{Channel: ChannelTelegram, AccountID: "bot-1", ConversationID: "chat-9", ParticipantID: "user-4", Scope: ScopeDirect},
		ExternalEventID: "update-77", IdempotencyKey: "telegram:update-77", Text: "restart-safe hello",
	}
}

func TestInboundClaimIsIdempotentAcrossRestart(t *testing.T) {
	ctx := context.Background()
	root, keyRoot := t.TempDir(), t.TempDir()
	user := "did:matrix:alice"
	store, err := Open(ctx, root, gatewayVault(t, keyRoot, user), user)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	envelope := inboundEnvelope()
	first, err := store.ClaimInbound(ctx, envelope)
	if err != nil || first.State != ClaimNew {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if err := store.CompleteInbound(ctx, envelope, "neo-conversation", "run-123"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(ctx, root, gatewayVault(t, keyRoot, user), user)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	duplicate, err := reopened.ClaimInbound(ctx, envelope)
	if err != nil {
		t.Fatalf("duplicate claim: %v", err)
	}
	if duplicate.State != ClaimDuplicate || duplicate.Status != "completed" || duplicate.RunID != "run-123" {
		t.Fatalf("duplicate = %+v", duplicate)
	}
	changed := envelope
	changed.Text = "different payload"
	if _, err := reopened.ClaimInbound(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed duplicate = %v, want conflict", err)
	}
	address := envelope.Address
	if err := reopened.SetPending(ctx, PendingAction{Address: address, Kind: KindApproval, RunID: "run-gate", NodeID: "node-1"}); err != nil {
		t.Fatalf("set pending: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = Open(ctx, root, gatewayVault(t, keyRoot, user), user)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	pending, ok, err := reopened.Pending(ctx, address)
	if err != nil || !ok || pending.RunID != "run-gate" || pending.NodeID != "node-1" {
		t.Fatalf("pending after restart = %+v, ok=%v err=%v", pending, ok, err)
	}
}

type httpProtocolSender struct {
	url    string
	client *http.Client
}

func (s httpProtocolSender) Send(ctx context.Context, envelope Envelope) (SendReceipt, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, strings.NewReader(envelope.Text))
	if err != nil {
		return SendReceipt{}, err
	}
	request.Header.Set("Idempotency-Key", envelope.IdempotencyKey)
	response, err := s.client.Do(request)
	if err != nil {
		return SendReceipt{}, &DeliveryError{Code: "transport", Message: err.Error()}
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode >= 500 {
		return SendReceipt{}, &DeliveryError{Code: "service", Message: response.Status, RetryAfter: time.Millisecond}
	}
	return SendReceipt{ExternalMessageID: response.Header.Get("X-Message-ID"), Code: "accepted"}, nil
}

func TestEncryptedDeliveryRetriesThroughRealHTTPAfterRestart(t *testing.T) {
	var calls atomic.Int32
	received := make(chan string, 1)
	protocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			http.Error(w, "temporary upstream outage", http.StatusServiceUnavailable)
			return
		}
		received <- r.Header.Get("Idempotency-Key") + ":" + string(body)
		w.Header().Set("X-Message-ID", "external-42")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer protocol.Close()

	ctx := context.Background()
	root, keyRoot := t.TempDir(), t.TempDir()
	user := "did:matrix:alice"
	store, err := Open(ctx, root, gatewayVault(t, keyRoot, user), user)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	policy := RetryPolicy{BaseBackoff: time.Millisecond, MaximumBackoff: time.Millisecond, MaximumAttempts: 3}
	dispatcher := NewDispatcher(store, policy)
	envelope := Envelope{
		Direction: Outbound, Kind: KindMessage,
		Address:        Address{Channel: ChannelTelegram, AccountID: "bot-1", ConversationID: "chat-9", Scope: ScopeDirect},
		IdempotencyKey: "delivery-key-9", Text: "private delivery body", SideEffectClass: tool.SideEffectNetwork,
	}
	first, err := dispatcher.Dispatch(ctx, envelope, httpProtocolSender{url: protocol.URL, client: protocol.Client()})
	var deliveryErr *DeliveryError
	if !errors.As(err, &deliveryErr) || first.State != DeliveryRetrying {
		t.Fatalf("first dispatch = %+v, %v", first, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, name := range []string{"channel-gateway.db", "channel-gateway.db-wal"} {
		raw, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr == nil && bytes.Contains(raw, []byte("private delivery body")) {
			t.Fatalf("plaintext delivery leaked into %s", name)
		}
	}

	time.Sleep(2 * time.Millisecond)
	reopened, err := Open(ctx, root, gatewayVault(t, keyRoot, user), user)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	drained, err := NewDispatcher(reopened, policy).Drain(ctx, ChannelTelegram, "bot-1", httpProtocolSender{url: protocol.URL, client: protocol.Client()}, 10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(drained) != 1 || drained[0].State != DeliveryDelivered || drained[0].ExternalMessageID != "external-42" {
		t.Fatalf("drained = %+v", drained)
	}
	select {
	case got := <-received:
		if got != "delivery-key-9:private delivery body" {
			t.Fatalf("protocol received %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("real protocol server did not receive retry")
	}
}

func TestConvertImageEnforcesRealChannelDimensionsAndBytes(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			source.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x + y), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatalf("encode source: %v", err)
	}
	converted, err := ConvertImage(bytes.NewReader(encoded.Bytes()), ImagePolicy{
		MaximumInputBytes: int64(encoded.Len() + 1), MaximumOutputBytes: 20 << 10, MaximumWidth: 96, MaximumHeight: 96,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if converted.MIMEType != "image/jpeg" || converted.Width != 96 || converted.Height != 60 || int64(len(converted.Data)) > 20<<10 || !converted.Changed {
		t.Fatalf("converted = mime %s, %dx%d, bytes %d, changed %v", converted.MIMEType, converted.Width, converted.Height, len(converted.Data), converted.Changed)
	}
	decoded, _, err := image.Decode(bytes.NewReader(converted.Data))
	if err != nil || decoded.Bounds().Dx() != 96 || decoded.Bounds().Dy() != 60 {
		t.Fatalf("decode output: bounds %v err %v", decoded.Bounds(), err)
	}
}
