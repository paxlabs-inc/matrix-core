// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package cortexclient is the Go client for the neocortex cortexd engine: a
// versioned FlatBuffers protocol over a Unix domain socket with capability
// handshake, idempotent append keyed by client sequence, bounded-queue crash
// isolation, and reconnect that loses no acknowledged write and never
// double-applies an unacknowledged one.
package cortexclient

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	flatbuffers "matrix/cortexclient/internal/flatbuffers"
	"matrix/cortexclient/wire/neocortex/protocol"
)

const (
	ProtocolVersion   = 2
	frameHeaderBytes  = 8
	maximumFrameBytes = 17 * 1024 * 1024
)

// EventKind mirrors the frozen engine taxonomy (schematics.md section 4).
type EventKind uint8

const (
	KindUserMsg       EventKind = 1
	KindDeliveredMsg  EventKind = 2
	KindToolCall      EventKind = 3
	KindToolResult    EventKind = 4
	KindReasoning     EventKind = 5
	KindProviderFrame EventKind = 6
	KindMediaRef      EventKind = 7
	KindEffect        EventKind = 8
	KindApproval      EventKind = 9
	KindOutcome       EventKind = 10
	KindCheckpoint    EventKind = 11
	KindSupervisor    EventKind = 12
	KindRecovery      EventKind = 13
	KindIntentSet     EventKind = 14
	KindLoopOpened    EventKind = 15
	KindLoopClosed    EventKind = 16
	KindAssertion     EventKind = 17
	KindConsolidation EventKind = 18
	KindEmbedding     EventKind = 19
	KindRetract       EventKind = 20
	KindAttestation   EventKind = 21
)

var (
	ErrClosed           = errors.New("cortexclient: client closed")
	ErrUnavailable      = errors.New("cortexclient: cortexd unavailable")
	ErrQueueFull        = errors.New("cortexclient: pending queue full")
	ErrWritePending     = errors.New("cortexclient: write pending reconnect")
	ErrOutcomeUnknown   = errors.New("cortexclient: write outcome cannot be resolved")
	ErrSequenceRecovery = errors.New("cortexclient: sequence recovery limit reached")
	ErrProtocol         = errors.New("cortexclient: protocol violation")
	ErrCapability       = errors.New("cortexclient: capability denied")
	ErrAbsent           = errors.New("cortexclient: record absent")
)

// PendingWriteError reports that a synchronous write has an unknown immediate
// outcome and is retained for same-sequence resolution on reconnect. The
// caller MUST NOT retry the logical write. A later client operation drives the
// reconnect and either applies the write once or surfaces its permanent
// rejection.
type PendingWriteError struct {
	ClientSeq uint64
	Cause     error
}

func (e *PendingWriteError) Error() string {
	return fmt.Sprintf("%v: client_seq=%d: %v", ErrWritePending, e.ClientSeq, e.Cause)
}

func (e *PendingWriteError) Unwrap() error { return e.Cause }

func (e *PendingWriteError) Is(target error) bool {
	return target == ErrWritePending || errors.Is(e.Cause, target)
}

// EngineError is a typed error returned by cortexd.
type EngineError struct {
	Code        uint8
	SystemError int32
	Lsn         uint64
	Offset      uint64
}

func (e *EngineError) Error() string {
	return fmt.Sprintf("cortexd error code=%d system=%d lsn=%d offset=%d",
		e.Code, e.SystemError, e.Lsn, e.Offset)
}

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// Config describes one client connection identity. ConnectionID is the
// idempotency identity: it MUST stay stable across reconnects so cortexd can
// deduplicate resent appends by (connection id, client sequence).
type Config struct {
	SocketPath      string
	CapabilityToken [32]byte
	ConnectionID    [16]byte
	DialTimeout     time.Duration
	RequestTimeout  time.Duration
	PendingLimit    int
	// SequenceRecoveryLimit bounds the number of occupied historical sequence
	// slots one Client may recover across its lifetime. cortexd reports the next
	// sequence during the handshake, so recovery never probes by submitting a
	// logical write at historical slots.
	SequenceRecoveryLimit uint64
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.DialTimeout <= 0 {
		out.DialTimeout = 5 * time.Second
	}
	if out.RequestTimeout <= 0 {
		out.RequestTimeout = 30 * time.Second
	}
	if out.PendingLimit <= 0 {
		out.PendingLimit = 256
	}
	if out.SequenceRecoveryLimit == 0 {
		out.SequenceRecoveryLimit = 65_536
	}
	return out
}

// Welcome carries the handshake result.
type Welcome struct {
	ActorNamespace uint16
	Admin          bool
	MaxFrameBytes  uint32
	MaxBatchEvents uint32
	NextClientSeq  uint64
}

// AppendEvent is one typed event to append. Build emits the typed payload
// union member into the batch envelope builder; the client wraps it with the
// batch idempotency identity (connection id, client seq, index, count).
type AppendEvent struct {
	Kind         EventKind
	Conversation [16]byte
	Build        func(builder *flatbuffers.Builder) (byte, flatbuffers.UOffsetT)
}

// AppendAck is the engine acknowledgement for one append batch, carrying the
// engine's tamper-evidence truth for the committed prefix.
type AppendAck struct {
	ClientSeq    uint64
	FirstLsn     uint64
	LastLsn      uint64
	Duplicate    bool
	LeafCount    uint64
	LastLeafHash [32]byte
	Root         [32]byte
}

type pendingWrite struct {
	clientSeq uint64
	send      func(clientSeq uint64) error
	unknown   bool
	onSuccess func()
	onFailure func(error)
}

type rejectedWriteError struct{ cause error }

func (e *rejectedWriteError) Error() string { return e.cause.Error() }
func (e *rejectedWriteError) Unwrap() error { return e.cause }

// Client is a connection to cortexd. All operations serialize on one socket;
// cortexd processes a connection's requests in order.
type Client struct {
	cfg Config

	mu        sync.Mutex
	conn      net.Conn
	reader    *frameReader
	welcome   Welcome
	requestID uint64
	clientSeq uint64
	recovered uint64
	pending   []pendingWrite
	closed    bool
}

// Dial connects and performs the capability handshake.
func Dial(cfg Config) (*Client, error) {
	client := &Client{cfg: cfg.withDefaults()}
	client.mu.Lock()
	defer client.mu.Unlock()
	if err := client.connectLocked(); err != nil {
		return nil, err
	}
	if err := client.initializeSequenceLocked(); err != nil {
		client.dropConnLocked()
		return nil, err
	}
	return client, nil
}

// Welcome returns the handshake result of the current connection.
func (c *Client) Welcome() Welcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.welcome
}

// Close releases the connection. Pending queued appends are dropped; callers
// that need durability must have observed acknowledgements.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// PendingLen reports the number of queued unacknowledged append batches.
func (c *Client) PendingLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func (c *Client) connectLocked() error {
	if c.closed {
		return ErrClosed
	}
	// Retry refused dials inside the timeout window: a supervisor restart
	// leaves a stale socket file briefly, and the crash-isolation contract
	// wants the client to ride through that gap.
	deadline := time.Now().Add(c.cfg.DialTimeout)
	var conn net.Conn
	var err error
	for {
		conn, err = net.DialTimeout("unix", c.cfg.SocketPath,
			time.Until(deadline))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("%w: dial %s: %v", ErrUnavailable, c.cfg.SocketPath, err)
	}
	reader := newFrameReader(conn)
	builder := flatbuffers.NewBuilder(256)
	connectionID := builder.CreateByteVector(c.cfg.ConnectionID[:])
	token := builder.CreateByteVector(c.cfg.CapabilityToken[:])
	protocol.HelloStart(builder)
	protocol.HelloAddProtoVersion(builder, ProtocolVersion)
	protocol.HelloAddConnectionId(builder, connectionID)
	protocol.HelloAddCapabilityToken(builder, token)
	hello := protocol.HelloEnd(builder)
	finishEnvelope(builder, protocol.WirePayloadHello, hello)
	if err := writeFrame(conn, builder.FinishedBytes(), c.cfg.RequestTimeout); err != nil {
		conn.Close()
		return fmt.Errorf("%w: hello: %v", ErrUnavailable, err)
	}
	payload, err := reader.next(c.cfg.RequestTimeout)
	if err != nil {
		conn.Close()
		return fmt.Errorf("%w: welcome: %v", ErrUnavailable, err)
	}
	envelope := protocol.GetRootAsWireEnvelope(payload, 0)
	if envelope.ProtoVersion() != ProtocolVersion ||
		envelope.PayloadType() != protocol.WirePayloadWelcome {
		conn.Close()
		if response, ok := envelopeResponse(envelope); ok {
			if engineErr := responseError(response); engineErr != nil {
				return engineErr
			}
		}
		return fmt.Errorf("%w: handshake rejected", ErrCapability)
	}
	var table flatbuffers.Table
	if !envelope.Payload(&table) {
		conn.Close()
		return ErrProtocol
	}
	var welcome protocol.Welcome
	welcome.Init(table.Bytes, table.Pos)
	c.conn = conn
	c.reader = reader
	c.welcome = Welcome{
		ActorNamespace: welcome.ActorNs(),
		Admin:          welcome.Admin(),
		MaxFrameBytes:  welcome.MaximumFrameBytes(),
		MaxBatchEvents: welcome.MaximumBatchEvents(),
		NextClientSeq:  welcome.NextClientSeq(),
	}
	if !c.welcome.Admin && c.welcome.NextClientSeq == 0 {
		c.dropConnLocked()
		return ErrProtocol
	}
	return nil
}

func (c *Client) dropConnLocked() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.reader = nil
	}
}

func (c *Client) consumeRecoveryLocked(delta uint64) error {
	if delta > c.cfg.SequenceRecoveryLimit-c.recovered {
		return fmt.Errorf("%w: recovered=%d requested=%d limit=%d",
			ErrSequenceRecovery, c.recovered, delta, c.cfg.SequenceRecoveryLimit)
	}
	c.recovered += delta
	return nil
}

func (c *Client) initializeSequenceLocked() error {
	if c.welcome.Admin {
		return nil
	}
	tail := c.welcome.NextClientSeq - 1
	if err := c.consumeRecoveryLocked(tail); err != nil {
		return err
	}
	c.clientSeq = tail
	return nil
}

func (c *Client) reconnectTransportLocked() error {
	c.dropConnLocked()
	return c.connectLocked()
}

// reconnectLocked re-establishes the connection and resends every pending
// unacknowledged write in sequence order. Writes with an unknown outcome keep
// their exact sequence; writes known never to have reached cortexd may be
// aligned to the server-reported free tail within the lifetime recovery bound.
func (c *Client) reconnectLocked() error {
	if err := c.reconnectTransportLocked(); err != nil {
		return err
	}
	if len(c.pending) == 0 && !c.welcome.Admin {
		tail := c.welcome.NextClientSeq - 1
		if tail > c.clientSeq {
			delta := tail - c.clientSeq
			if err := c.consumeRecoveryLocked(delta); err != nil {
				return err
			}
			c.clientSeq = tail
		}
	}
	for len(c.pending) > 0 {
		if err := c.sendPendingHeadLocked(); err != nil {
			c.dropConnLocked()
			return err
		}
		c.pending = c.pending[1:]
	}
	return nil
}

func (c *Client) advancePendingLocked(delta uint64) error {
	if err := c.consumeRecoveryLocked(delta); err != nil {
		return err
	}
	c.clientSeq += delta
	for index := range c.pending {
		c.pending[index].clientSeq += delta
	}
	return nil
}

func (c *Client) sendPendingHeadLocked() error {
	for {
		entry := c.pending[0]
		if !c.welcome.Admin {
			next := c.welcome.NextClientSeq
			if entry.unknown {
				if next != entry.clientSeq && next != entry.clientSeq+1 {
					return fmt.Errorf("%w: client_seq=%d server_next=%d",
						ErrOutcomeUnknown, entry.clientSeq, next)
				}
			} else if next != entry.clientSeq {
				if next < entry.clientSeq {
					return fmt.Errorf("%w: client_seq=%d server_next=%d",
						ErrOutcomeUnknown, entry.clientSeq, next)
				}
				if err := c.advancePendingLocked(next - entry.clientSeq); err != nil {
					return err
				}
				entry = c.pending[0]
			}
		}
		err := c.sendAdmittedLocked(entry.clientSeq, entry.send)
		if err == nil {
			if entry.onSuccess != nil {
				entry.onSuccess()
			}
			return nil
		}
		if sequenceConflict(err) {
			if entry.unknown {
				return fmt.Errorf("%w: client_seq=%d: %v",
					ErrOutcomeUnknown, entry.clientSeq, err)
			}
			if err := c.reconnectTransportLocked(); err != nil {
				return err
			}
			continue
		}
		var rejected *rejectedWriteError
		if !errors.As(err, &rejected) {
			return err
		}
		c.pending = c.pending[1:]
		if c.clientSeq > 0 {
			c.clientSeq--
		}
		for index := range c.pending {
			c.pending[index].clientSeq--
		}
		if entry.onFailure != nil {
			entry.onFailure(rejected.cause)
		}
		return rejected.cause
	}
}

func (c *Client) ensureLocked() error {
	if c.closed {
		return ErrClosed
	}
	if c.conn != nil {
		return nil
	}
	return c.reconnectLocked()
}

// roundTripLocked sends one finished request builder and returns the matching
// Response table bytes. Event frames (subscription pushes) are skipped.
func (c *Client) roundTripLocked(requestID uint64, finished []byte) (*protocol.Response, []byte, error) {
	if err := writeFrame(c.conn, finished, c.cfg.RequestTimeout); err != nil {
		c.dropConnLocked()
		return nil, nil, fmt.Errorf("%w: write: %v", ErrUnavailable, err)
	}
	for {
		payload, err := c.reader.next(c.cfg.RequestTimeout)
		if err != nil {
			c.dropConnLocked()
			return nil, nil, fmt.Errorf("%w: read: %v", ErrUnavailable, err)
		}
		envelope := protocol.GetRootAsWireEnvelope(payload, 0)
		if envelope.PayloadType() == protocol.WirePayloadEvent {
			continue
		}
		response, ok := envelopeResponse(envelope)
		if !ok {
			c.dropConnLocked()
			return nil, nil, ErrProtocol
		}
		if response.RequestId() != requestID {
			c.dropConnLocked()
			return nil, nil, ErrProtocol
		}
		return response, payload, nil
	}
}

func envelopeResponse(envelope *protocol.WireEnvelope) (*protocol.Response, bool) {
	if envelope.PayloadType() != protocol.WirePayloadResponse {
		return nil, false
	}
	var table flatbuffers.Table
	if !envelope.Payload(&table) {
		return nil, false
	}
	response := &protocol.Response{}
	response.Init(table.Bytes, table.Pos)
	return response, true
}

func responseError(response *protocol.Response) error {
	if response.Status() == protocol.ResponseStatusok {
		return nil
	}
	var table flatbuffers.Table
	if response.Payload(&table) &&
		response.PayloadType() == protocol.ResponsePayloadErrorDetail {
		var detail protocol.ErrorDetail
		detail.Init(table.Bytes, table.Pos)
		return &EngineError{
			Code:        detail.Code(),
			SystemError: detail.SystemError(),
			Lsn:         detail.Lsn(),
			Offset:      detail.Offset(),
		}
	}
	return ErrProtocol
}

func responseTable(response *protocol.Response, want protocol.ResponsePayload) (flatbuffers.Table, error) {
	if err := responseError(response); err != nil {
		return flatbuffers.Table{}, err
	}
	if response.PayloadType() != want {
		return flatbuffers.Table{}, ErrProtocol
	}
	var table flatbuffers.Table
	if !response.Payload(&table) {
		return flatbuffers.Table{}, ErrProtocol
	}
	return table, nil
}

func (c *Client) nextRequestIDLocked() uint64 {
	c.requestID++
	return c.requestID
}

// NextClientSeq allocates the next append sequence number.
func (c *Client) NextClientSeq() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clientSeq++
	return c.clientSeq
}

func buildAppendRequest(requestID, clientSeq uint64, connectionID [16]byte,
	events []AppendEvent) []byte {
	builder := flatbuffers.NewBuilder(1024)
	offsets := make([]flatbuffers.UOffsetT, len(events))
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		payload := wrapEnvelope(event, connectionID, clientSeq,
			uint32(index), uint32(len(events)))
		payloadOffset := builder.CreateByteVector(payload)
		conversation := builder.CreateByteVector(event.Conversation[:])
		protocol.AppendEventStart(builder)
		protocol.AppendEventAddKind(builder, byte(event.Kind))
		protocol.AppendEventAddConversation(builder, conversation)
		protocol.AppendEventAddPayload(builder, payloadOffset)
		offsets[index] = protocol.AppendEventEnd(builder)
	}
	protocol.AppendStartEventsVector(builder, len(offsets))
	for index := len(offsets) - 1; index >= 0; index-- {
		builder.PrependUOffsetT(offsets[index])
	}
	eventsVector := builder.EndVector(len(offsets))
	protocol.AppendStart(builder)
	protocol.AppendAddClientSeq(builder, clientSeq)
	protocol.AppendAddEvents(builder, eventsVector)
	appendOffset := protocol.AppendEnd(builder)
	return finishRequest(builder, requestID, protocol.RequestPayloadAppend,
		appendOffset)
}

func (c *Client) appendOnceLocked(clientSeq uint64, events []AppendEvent) (AppendAck, error) {
	requestID := c.nextRequestIDLocked()
	finished := buildAppendRequest(requestID, clientSeq, c.cfg.ConnectionID, events)
	response, _, err := c.roundTripLocked(requestID, finished)
	if err != nil {
		return AppendAck{}, err
	}
	table, err := responseTable(response, protocol.ResponsePayloadAppendAck)
	if err != nil {
		return AppendAck{}, err
	}
	var wireAck protocol.AppendAck
	wireAck.Init(table.Bytes, table.Pos)
	ack := AppendAck{
		ClientSeq: wireAck.ClientSeq(),
		FirstLsn:  wireAck.FirstLsn(),
		LastLsn:   wireAck.LastLsn(),
		Duplicate: wireAck.Duplicate(),
		LeafCount: wireAck.LeafCount(),
	}
	copy(ack.LastLeafHash[:], wireAck.LastLeafHashBytes())
	copy(ack.Root[:], wireAck.MmrRootBytes())
	return ack, nil
}

// Engine error codes the sequencing policy discriminates on. Values mirror
// neocortex/src/core/error.h.
const (
	CodeSequenceViolation   = 3
	CodeCapabilityDenied    = 47
	CodeIdempotencyConflict = 50
)

func sequenceConflict(err error) bool {
	var engineErr *EngineError
	if !errors.As(err, &engineErr) {
		return false
	}
	return engineErr.Code == CodeSequenceViolation ||
		engineErr.Code == CodeIdempotencyConflict
}

// sendAdmittedLocked delivers one admitted write at a fixed client sequence.
// Any post-send error is ambiguous until a new Welcome reports whether cortexd
// advanced. An explicit engine rejection is released only when that handshake
// proves the sequence remains free; otherwise the exact sequence is replayed.
func (c *Client) sendAdmittedLocked(clientSeq uint64, send func(clientSeq uint64) error) error {
	err := send(clientSeq)
	if err == nil {
		if !c.welcome.Admin {
			c.welcome.NextClientSeq = clientSeq + 1
		}
		return nil
	}
	if sequenceConflict(err) {
		return err
	}
	for resolution := 0; resolution < 2; resolution++ {
		var engineErr *EngineError
		engineRejected := errors.As(err, &engineErr)
		if reconnectErr := c.reconnectTransportLocked(); reconnectErr != nil {
			return reconnectErr
		}
		if !c.welcome.Admin {
			next := c.welcome.NextClientSeq
			if engineRejected && next == clientSeq {
				return &rejectedWriteError{cause: err}
			}
			if next != clientSeq && next != clientSeq+1 {
				return fmt.Errorf("%w: client_seq=%d server_next=%d: %v",
					ErrOutcomeUnknown, clientSeq, next, err)
			}
		}
		if resolution == 1 {
			return err
		}
		err = send(clientSeq)
		if err == nil {
			if !c.welcome.Admin {
				c.welcome.NextClientSeq = clientSeq + 1
			}
			return nil
		}
		if sequenceConflict(err) {
			return err
		}
	}
	return err
}

// writeAdmittedLocked runs the full self-healing sequence policy for one
// write that has been admitted at c.clientSeq:
//   - an engine rejection releases the sequence slot only after a reconnect
//     proves the server never recorded it, so the chain does not wedge;
//   - a sequence/idempotency conflict means the server durably holds a write
//     the client considered rejected (apply succeeded but the projection step
//     failed before acknowledgement); the slot is burned, so retry once at
//     the next sequence.
func (c *Client) writeAdmittedLocked(send func(clientSeq uint64) error) error {
	for {
		err := c.sendAdmittedLocked(c.clientSeq, send)
		if err == nil {
			return nil
		}
		if sequenceConflict(err) {
			if reconnectErr := c.reconnectTransportLocked(); reconnectErr != nil {
				return reconnectErr
			}
			next := c.welcome.NextClientSeq
			if next <= c.clientSeq {
				c.clientSeq--
				return fmt.Errorf("%w: client_seq=%d server_next=%d",
					ErrSequenceRecovery, c.clientSeq+1, next)
			}
			delta := next - c.clientSeq
			if recoveryErr := c.consumeRecoveryLocked(delta); recoveryErr != nil {
				c.clientSeq--
				return recoveryErr
			}
			c.clientSeq = next
			continue
		}
		var rejected *rejectedWriteError
		if errors.As(err, &rejected) {
			c.clientSeq--
			return rejected
		}
		return err
	}
}

func (c *Client) retainPendingLocked(send func(clientSeq uint64) error, cause error,
	onSuccess func(), onFailure func(error)) error {
	c.dropConnLocked()
	c.pending = append(c.pending, pendingWrite{
		clientSeq: c.clientSeq,
		send:      send,
		unknown:   true,
		onSuccess: onSuccess,
		onFailure: onFailure,
	})
	return &PendingWriteError{ClientSeq: c.clientSeq, Cause: cause}
}

// Append synchronously appends one batch. If the connection breaks after the
// sequence is admitted and reconnect cannot complete, Append returns a typed
// PendingWriteError: the caller knows the exact write is retained for
// same-sequence resolution and MUST NOT retry it.
func (c *Client) Append(ctx context.Context, events []AppendEvent) (AppendAck, error) {
	if len(events) == 0 {
		return AppendAck{}, fmt.Errorf("%w: empty batch", ErrProtocol)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return AppendAck{}, err
	}
	if err := c.ensureLocked(); err != nil {
		return AppendAck{}, err
	}
	c.clientSeq++
	var ack AppendAck
	send := func(clientSeq uint64) error {
		result, err := c.appendOnceLocked(clientSeq, events)
		if err != nil {
			return err
		}
		ack = result
		return nil
	}
	err := c.writeAdmittedLocked(send)
	var rejected *rejectedWriteError
	if err != nil && !errors.Is(err, ErrSequenceRecovery) &&
		!errors.As(err, &rejected) {
		err = c.retainPendingLocked(send, err, nil, nil)
	}
	return ack, err
}

// QueueAppend appends with degraded-mode buffering: while cortexd is down the
// batch enters the bounded pending queue and is flushed, in sequence order,
// by the next successful reconnect. A full queue is an honest typed failure
// before a sequence is allocated, never a silent drop.
func (c *Client) QueueAppend(ctx context.Context, events []AppendEvent) (AppendAck, bool, error) {
	return c.queueAppend(ctx, events, nil)
}

func (c *Client) queueAppend(ctx context.Context, events []AppendEvent,
	onPendingResult func(AppendAck, error)) (AppendAck, bool, error) {
	if len(events) == 0 {
		return AppendAck{}, false, fmt.Errorf("%w: empty batch", ErrProtocol)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return AppendAck{}, false, err
	}
	if c.closed {
		return AppendAck{}, false, ErrClosed
	}
	var ack AppendAck
	send := func(clientSeq uint64) error {
		result, err := c.appendOnceLocked(clientSeq, events)
		if err != nil {
			return err
		}
		ack = result
		return nil
	}
	if c.conn == nil {
		reconnectErr := c.reconnectLocked()
		if reconnectErr == nil {
			// Continue below on the restored connection.
		} else if !errors.Is(reconnectErr, ErrUnavailable) {
			return AppendAck{}, false, reconnectErr
		} else {
			if len(c.pending) >= c.cfg.PendingLimit {
				return AppendAck{}, false, ErrQueueFull
			}
			c.clientSeq++
			entry := pendingWrite{clientSeq: c.clientSeq, send: send}
			if onPendingResult != nil {
				entry.onSuccess = func() { onPendingResult(ack, nil) }
				entry.onFailure = func(err error) {
					onPendingResult(AppendAck{}, err)
				}
			}
			c.pending = append(c.pending, entry)
			return AppendAck{}, true, nil
		}
	}
	c.clientSeq++
	err := c.writeAdmittedLocked(send)
	if err == nil {
		return ack, false, nil
	}
	var rejected *rejectedWriteError
	if !errors.Is(err, ErrSequenceRecovery) && !errors.As(err, &rejected) {
		c.dropConnLocked()
		entry := pendingWrite{clientSeq: c.clientSeq, send: send, unknown: true}
		if onPendingResult != nil {
			entry.onSuccess = func() { onPendingResult(ack, nil) }
			entry.onFailure = func(err error) {
				onPendingResult(AppendAck{}, err)
			}
		}
		c.pending = append(c.pending, entry)
		return AppendAck{}, true, nil
	}
	return AppendAck{}, false, err
}

// request performs one non-append request round trip with reconnect-once.
func (c *Client) request(ctx context.Context, build func(builder *flatbuffers.Builder, requestID uint64) []byte,
	want protocol.ResponsePayload) (flatbuffers.Table, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return flatbuffers.Table{}, nil, err
	}
	if err := c.ensureLocked(); err != nil {
		return flatbuffers.Table{}, nil, err
	}
	attempt := func() (flatbuffers.Table, []byte, error) {
		requestID := c.nextRequestIDLocked()
		builder := flatbuffers.NewBuilder(1024)
		finished := build(builder, requestID)
		response, payload, err := c.roundTripLocked(requestID, finished)
		if err != nil {
			return flatbuffers.Table{}, nil, err
		}
		table, err := responseTable(response, want)
		return table, payload, err
	}
	table, payload, err := attempt()
	if err == nil || !errors.Is(err, ErrUnavailable) {
		return table, payload, err
	}
	if reconnectErr := c.reconnectLocked(); reconnectErr != nil {
		return flatbuffers.Table{}, nil, reconnectErr
	}
	return attempt()
}

func finishEnvelope(builder *flatbuffers.Builder, payloadType protocol.WirePayload,
	payload flatbuffers.UOffsetT) {
	protocol.WireEnvelopeStart(builder)
	protocol.WireEnvelopeAddProtoVersion(builder, ProtocolVersion)
	protocol.WireEnvelopeAddPayloadType(builder, payloadType)
	protocol.WireEnvelopeAddPayload(builder, payload)
	envelope := protocol.WireEnvelopeEnd(builder)
	builder.FinishWithFileIdentifier(envelope, []byte("NCPR"))
}

func finishRequest(builder *flatbuffers.Builder, requestID uint64,
	payloadType protocol.RequestPayload, payload flatbuffers.UOffsetT) []byte {
	protocol.RequestStart(builder)
	protocol.RequestAddRequestId(builder, requestID)
	protocol.RequestAddPayloadType(builder, payloadType)
	protocol.RequestAddPayload(builder, payload)
	request := protocol.RequestEnd(builder)
	finishEnvelope(builder, protocol.WirePayloadRequest, request)
	return builder.FinishedBytes()
}

func writeFrame(conn net.Conn, payload []byte, timeout time.Duration) error {
	if len(payload) == 0 || len(payload) > maximumFrameBytes {
		return fmt.Errorf("%w: frame length %d", ErrProtocol, len(payload))
	}
	frame := make([]byte, frameHeaderBytes+len(payload))
	putU32(frame[0:4], uint32(len(payload)))
	putU32(frame[4:8], crc32.Checksum(payload, crc32cTable))
	copy(frame[frameHeaderBytes:], payload)
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	_, err := conn.Write(frame)
	return err
}

type frameReader struct {
	conn    net.Conn
	pending []byte
}

func newFrameReader(conn net.Conn) *frameReader {
	return &frameReader{conn: conn}
}

func (r *frameReader) next(timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	buffer := make([]byte, 64*1024)
	for {
		if len(r.pending) >= frameHeaderBytes {
			length := getU32(r.pending[0:4])
			checksum := getU32(r.pending[4:8])
			if length == 0 || length > maximumFrameBytes {
				return nil, fmt.Errorf("%w: frame length %d", ErrProtocol, length)
			}
			total := frameHeaderBytes + int(length)
			if len(r.pending) >= total {
				payload := make([]byte, length)
				copy(payload, r.pending[frameHeaderBytes:total])
				r.pending = r.pending[total:]
				if crc32.Checksum(payload, crc32cTable) != checksum {
					return nil, fmt.Errorf("%w: frame checksum", ErrProtocol)
				}
				return payload, nil
			}
		}
		if err := r.conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		count, err := r.conn.Read(buffer)
		if err != nil {
			return nil, err
		}
		r.pending = append(r.pending, buffer[:count]...)
	}
}

func putU32(out []byte, value uint32) {
	out[0] = byte(value)
	out[1] = byte(value >> 8)
	out[2] = byte(value >> 16)
	out[3] = byte(value >> 24)
}

func getU32(in []byte) uint32 {
	return uint32(in[0]) | uint32(in[1])<<8 | uint32(in[2])<<16 | uint32(in[3])<<24
}

// Supervisor keeps one cortexd process alive: it spawns the daemon and
// restarts it whenever it exits, until Stop. A cortexd crash therefore never
// takes the agent down; the client reconnects through the same socket.
type Supervisor struct {
	Binary       string
	ConfigPath   string
	RestartDelay time.Duration

	mu      sync.Mutex
	cmd     *exec.Cmd
	stopped bool
	done    chan struct{}
}

// Start launches cortexd and the restart monitor.
func (s *Supervisor) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		return errors.New("cortexclient: supervisor already started")
	}
	if s.RestartDelay <= 0 {
		s.RestartDelay = 200 * time.Millisecond
	}
	s.done = make(chan struct{})
	if err := s.spawnLocked(); err != nil {
		close(s.done)
		s.done = nil
		return err
	}
	go s.monitor()
	return nil
}

func (s *Supervisor) spawnLocked() error {
	cmd := exec.Command(s.Binary, "--config", s.ConfigPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd = cmd
	return nil
}

func (s *Supervisor) monitor() {
	for {
		s.mu.Lock()
		cmd := s.cmd
		stopped := s.stopped
		s.mu.Unlock()
		if stopped || cmd == nil {
			close(s.done)
			return
		}
		waitErr := cmd.Wait()
		_ = waitErr
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			close(s.done)
			return
		}
		s.mu.Unlock()
		time.Sleep(s.RestartDelay)
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			close(s.done)
			return
		}
		if err := s.spawnLocked(); err != nil {
			s.stopped = true
			s.mu.Unlock()
			close(s.done)
			return
		}
		s.mu.Unlock()
	}
}

// Pid reports the running daemon's pid, or 0.
func (s *Supervisor) Pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Stop terminates the daemon and the monitor.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stopped = true
	cmd := s.cmd
	done := s.done
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	if done != nil {
		<-done
	}
}
