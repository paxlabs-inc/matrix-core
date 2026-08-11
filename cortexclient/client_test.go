// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortexclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	flatbuffers "matrix/cortexclient/internal/flatbuffers"
	"matrix/cortexclient/wire/neocortex/protocol"
)

func cortexdBinary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("NEOCORTEX_CORTEXD"); path != "" {
		return path
	}
	for _, candidate := range []string{
		"../neocortex/build/debug/cortexd",
		"../neocortex/build/repro-a/cortexd",
	} {
		absolute, err := filepath.Abs(candidate)
		if err == nil {
			if _, statErr := os.Stat(absolute); statErr == nil {
				return absolute
			}
		}
	}
	t.Fatalf("real cortexd binary not found; build neocortex or set NEOCORTEX_CORTEXD")
	return ""
}

type daemonFixture struct {
	socket     string
	supervisor *Supervisor
	actorToken [32]byte
	adminToken [32]byte
}

func startDaemon(t *testing.T) *daemonFixture {
	t.Helper()
	directory := t.TempDir()
	socket := filepath.Join(directory, "cortexd.sock")
	configPath := filepath.Join(directory, "cortexd.conf")
	config := "socket=" + socket + "\n" +
		"data=" + filepath.Join(directory, "data") + "\n" +
		"user=0102030405060708090a0b0c0d0e0f10\n" +
		"kek=1111111111111111111111111111111111111111111111111111111111111111\n" +
		"signing_seed=2222222222222222222222222222222222222222222222222222222222222222\n" +
		"admin_token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"actor=11:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	supervisor := &Supervisor{
		Binary:       cortexdBinary(t),
		ConfigPath:   configPath,
		RestartDelay: 100 * time.Millisecond,
	}
	if err := supervisor.Start(); err != nil {
		t.Fatalf("start cortexd: %v", err)
	}
	t.Cleanup(supervisor.Stop)
	fixture := &daemonFixture{socket: socket, supervisor: supervisor}
	for index := range fixture.actorToken {
		fixture.actorToken[index] = 0xbb
	}
	for index := range fixture.adminToken {
		fixture.adminToken[index] = 0xaa
	}
	waitForSocket(t, socket)
	return fixture
}

func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cortexd socket never appeared at %s", socket)
}

func connectionID(value byte) [16]byte {
	var id [16]byte
	id[0] = value
	id[15] = value ^ 0xa5
	return id
}

func dialActor(t *testing.T, fixture *daemonFixture, connection byte) *Client {
	t.Helper()
	client, err := Dial(Config{
		SocketPath:      fixture.socket,
		CapabilityToken: fixture.actorToken,
		ConnectionID:    connectionID(connection),
		PendingLimit:    4,
	})
	if err != nil {
		t.Fatalf("dial actor: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	if client.Welcome().Admin || client.Welcome().ActorNamespace != 11 {
		t.Fatalf("unexpected welcome %+v", client.Welcome())
	}
	return client
}

func dialActorConfig(fixture *daemonFixture, connection byte) Config {
	return Config{
		SocketPath:      fixture.socket,
		CapabilityToken: fixture.actorToken,
		ConnectionID:    connectionID(connection),
		PendingLimit:    4,
	}
}

func appendWithoutReadingAck(t *testing.T, fixture *daemonFixture,
	connection byte, clientSeq uint64, events []AppendEvent) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", fixture.socket, 5*time.Second)
	if err != nil {
		t.Fatalf("raw dial: %v", err)
	}
	builder := flatbuffers.NewBuilder(256)
	id := connectionID(connection)
	connectionOffset := builder.CreateByteVector(id[:])
	token := builder.CreateByteVector(fixture.actorToken[:])
	protocol.HelloStart(builder)
	protocol.HelloAddProtoVersion(builder, ProtocolVersion)
	protocol.HelloAddConnectionId(builder, connectionOffset)
	protocol.HelloAddCapabilityToken(builder, token)
	hello := protocol.HelloEnd(builder)
	finishEnvelope(builder, protocol.WirePayloadHello, hello)
	if err := writeFrame(conn, builder.FinishedBytes(), 5*time.Second); err != nil {
		conn.Close()
		t.Fatalf("raw hello write: %v", err)
	}
	reader := newFrameReader(conn)
	payload, err := reader.next(5 * time.Second)
	if err != nil {
		conn.Close()
		t.Fatalf("raw welcome read: %v", err)
	}
	envelope := protocol.GetRootAsWireEnvelope(payload, 0)
	if envelope.ProtoVersion() != ProtocolVersion ||
		envelope.PayloadType() != protocol.WirePayloadWelcome {
		conn.Close()
		t.Fatalf("raw welcome rejected")
	}
	request := buildAppendRequest(1, clientSeq, id, events)
	if err := writeFrame(conn, request, 5*time.Second); err != nil {
		conn.Close()
		t.Fatalf("raw append write: %v", err)
	}
	return conn
}

func relayOneFrame(source net.Conn, destination net.Conn, timeout time.Duration) ([]byte, error) {
	payload, err := newFrameReader(source).next(timeout)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(destination, payload, timeout); err != nil {
		return nil, err
	}
	return payload, nil
}

func startLostAckProxy(t *testing.T, socket, backend string) <-chan error {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen lost-ack proxy: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	done := make(chan error, 1)
	go func() {
		front, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		back, dialErr := net.DialTimeout("unix", backend, 5*time.Second)
		if dialErr != nil {
			front.Close()
			done <- dialErr
			return
		}
		defer front.Close()
		defer back.Close()
		if _, relayErr := relayOneFrame(front, back, 5*time.Second); relayErr != nil {
			done <- relayErr
			return
		}
		if _, relayErr := relayOneFrame(back, front, 5*time.Second); relayErr != nil {
			done <- relayErr
			return
		}
		if _, relayErr := relayOneFrame(front, back, 5*time.Second); relayErr != nil {
			done <- relayErr
			return
		}
		if _, readErr := newFrameReader(back).next(5 * time.Second); readErr != nil {
			done <- readErr
			return
		}
		listener.Close()
		done <- nil
	}()
	return done
}

func startPassthroughProxy(t *testing.T, socket, backend string) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen passthrough proxy: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			front, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			back, dialErr := net.DialTimeout("unix", backend, 5*time.Second)
			if dialErr != nil {
				front.Close()
				continue
			}
			go func() {
				defer front.Close()
				defer back.Close()
				copied := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(back, front)
					copied <- struct{}{}
				}()
				go func() {
					_, _ = io.Copy(front, back)
					copied <- struct{}{}
				}()
				<-copied
			}()
		}
	}()
}

func startProtocolErrorProxy(t *testing.T, socket, backend string) <-chan error {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen protocol-error proxy: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	done := make(chan error, 1)
	go func() {
		front, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		back, dialErr := net.DialTimeout("unix", backend, 5*time.Second)
		if dialErr != nil {
			front.Close()
			done <- dialErr
			return
		}
		if _, relayErr := relayOneFrame(front, back, 5*time.Second); relayErr != nil {
			front.Close()
			back.Close()
			done <- relayErr
			return
		}
		welcome, relayErr := relayOneFrame(back, front, 5*time.Second)
		if relayErr != nil {
			front.Close()
			back.Close()
			done <- relayErr
			return
		}
		if _, relayErr = relayOneFrame(front, back, 5*time.Second); relayErr != nil {
			front.Close()
			back.Close()
			done <- relayErr
			return
		}
		if _, readErr := newFrameReader(back).next(5 * time.Second); readErr != nil {
			front.Close()
			back.Close()
			done <- readErr
			return
		}
		if writeErr := writeFrame(front, welcome, 5*time.Second); writeErr != nil {
			front.Close()
			back.Close()
			done <- writeErr
			return
		}
		front.Close()
		back.Close()
		done <- nil

		front, acceptErr = listener.Accept()
		if acceptErr != nil {
			return
		}
		back, dialErr = net.DialTimeout("unix", backend, 5*time.Second)
		if dialErr != nil {
			front.Close()
			return
		}
		defer front.Close()
		defer back.Close()
		copied := make(chan struct{}, 2)
		go func() {
			_, _ = io.Copy(back, front)
			copied <- struct{}{}
		}()
		go func() {
			_, _ = io.Copy(front, back)
			copied <- struct{}{}
		}()
		<-copied
	}()
	return done
}

func waitForLogEvents(t *testing.T, admin *Client, actor uint16, want uint64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := admin.AdminStats(context.Background(), actor)
		if err == nil && stats.LogEvents == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	stats, err := admin.AdminStats(context.Background(), actor)
	t.Fatalf("log events never reached %d: stats=%+v err=%v", want, stats, err)
}

func transcriptTexts(t *testing.T, client *Client, conversation [16]byte) []string {
	t.Helper()
	records, truncated, err := client.Transcript(context.Background(), conversation, 0, 16384)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if truncated {
		t.Fatalf("transcript unexpectedly truncated")
	}
	texts := make([]string, 0, len(records))
	for _, record := range records {
		texts = append(texts, fmt.Sprintf("%d:%s", record.Kind,
			eventText(record.Kind, record.Payload)))
	}
	return texts
}

func TestLoopSeamContractAgainstRealCortexd(t *testing.T) {
	fixture := startDaemon(t)
	client := dialActor(t, fixture, 1)
	ctx := context.Background()
	if _, err := client.Append(ctx, []AppendEvent{UserMsgEvent(
		ConversationBytes("conv-previous"),
		"Earlier conversation observed moltbook.com serving traffic.",
	)}); err != nil {
		t.Fatalf("append previous-conversation memory: %v", err)
	}

	seam, err := NewLoopSeam(client, SeamConfig{
		ConversationID: "conv-contract",
		BudgetTokens:   1_000_000,
	})
	if err != nil {
		t.Fatalf("new seam: %v", err)
	}

	seam.RecordUser("what is the status of moltbook.com")
	citation, err := seam.CommitToolExecution(ctx, ToolExecution{
		CallID:         "call-1",
		ToolName:       "web_fetch",
		Arguments:      json.RawMessage(`{"url":"https://moltbook.com"}`),
		Result:         json.RawMessage(`{"status":200,"body":"moltbook lives"}`),
		Expect:         "fetch the homepage",
		IdempotencyKey: "idem-1",
		MatchVerdict:   "matched",
		SubgoalID:      "subgoal-1",
	})
	if err != nil {
		t.Fatalf("commit tool execution: %v", err)
	}
	if citation.Seq == 0 || citation.LeafCount < citation.Seq {
		t.Fatalf("citation lacks engine sequence truth: %+v", citation)
	}
	var zero [32]byte
	if citation.LeafHash == zero || citation.JournalRoot == zero {
		t.Fatalf("citation lacks engine hash truth: %+v", citation)
	}
	seam.RecordAssistantWorking("moltbook.com is reachable, drafting answer")
	seam.RecordDelivery("moltbook.com is alive and serving traffic")
	if err := seam.RecordError(); err != nil {
		t.Fatalf("record error: %v", err)
	}

	conv, lo, hi := seam.ProvenanceRange()
	if conv != "conv-contract" || lo == 0 || hi < lo {
		t.Fatalf("provenance range %s %d %d", conv, lo, hi)
	}

	if _, err := seam.Consolidate(ctx, []Assertion{{
		BeliefID:          []byte("belief-moltbook1"),
		Type:              6, // out-of-range types are engine-rejected; 2=constraint below
		CanonicalIdentity: "moltbook.com",
		Value:             []byte("moltbook.com is alive"),
		ValidFromNs:       1,
		Provenance:        [][2]uint64{{lo, hi}},
	}}); err == nil {
		t.Fatalf("expected typed rejection for out-of-range belief type")
	}
	if _, err := seam.Consolidate(ctx, []Assertion{{
		BeliefID:          []byte("belief-moltbook1"),
		Type:              2, // constraint: a resident tier type
		CanonicalIdentity: "moltbook.com",
		Value:             []byte("moltbook.com is alive"),
		ValidFromNs:       1,
		Provenance:        [][2]uint64{{lo, hi}},
	}}); err != nil {
		t.Fatalf("consolidate: %v", err)
	}

	rendered, err := seam.Activate(ctx, ActivationQuery{
		Query:    "moltbook.com",
		Premises: []string{"the user asked about moltbook.com"},
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	for _, expect := range []string{"moltbook.com is alive", "[premises]"} {
		if !strings.Contains(rendered, expect) {
			t.Fatalf("activation missing %q:\n%s", expect, rendered)
		}
	}
	for _, forbidden := range []string{
		"what is the status of moltbook.com", // authoritative live transcript
		"moltbook lives",                     // operational tool result
		"drafting answer",                    // undelivered reasoning
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("activation leaked %q:\n%s", forbidden, rendered)
		}
	}

	bundle, err := seam.ActivateBundle(ctx, ActivationQuery{Query: "moltbook.com"})
	if err != nil {
		t.Fatalf("activate bundle: %v", err)
	}
	if bundle.SpentTokens == 0 || bundle.SpentTokens > bundle.BudgetTokens {
		t.Fatalf("bundle spend %d budget %d", bundle.SpentTokens, bundle.BudgetTokens)
	}
	var used []uint64
	for _, section := range []int{0, 4, 6} {
		for _, item := range bundle.Sections[section].Items {
			used = append(used, item.Provenance...)
		}
	}
	if len(used) == 0 {
		t.Fatal("semantic activation lacks attestable provenance")
	}
	foundExplicitRecall := false
	for _, memory := range ProjectBundle(bundle) {
		if memory.Tier != "recall" {
			continue
		}
		foundExplicitRecall = true
		if memory.ConversationID == "" || memory.Date == "" ||
			memory.SourceType == "" || memory.Confidence <= 0 ||
			memory.RelevanceScore <= 0 || memory.SelectionReason == "" ||
			memory.SourceIdentity == "" || memory.EpistemicStatus == "" ||
			len(memory.Provenance) == 0 {
			t.Fatalf("real cortexd recall lacked provenance: %#v", memory)
		}
	}
	if !foundExplicitRecall {
		t.Fatalf("real cortexd bundle lacked explicit previous-conversation recall: %#v", bundle.Sections[6].Items)
	}
	count, err := client.Attest(ctx, used, nil)
	if err != nil || count == 0 {
		t.Fatalf("attest count=%d err=%v", count, err)
	}

	lsn, err := seam.SaveTurnCheckpoint(ctx, "turn-1", []byte(`{"step":3}`))
	if err != nil || lsn == 0 {
		t.Fatalf("save checkpoint lsn=%d err=%v", lsn, err)
	}
	blob, latestLsn, err := seam.LatestTurnCheckpoint(ctx, "turn-1")
	if err != nil || latestLsn != lsn || string(blob) != `{"step":3}` {
		t.Fatalf("latest checkpoint blob=%q lsn=%d err=%v", blob, latestLsn, err)
	}
	if _, _, err := seam.LatestTurnCheckpoint(ctx, "turn-unknown"); !errors.Is(err, ErrAbsent) {
		t.Fatalf("expected ErrAbsent, got %v", err)
	}
}

func TestKillNineMidTurnRecoversWithoutLossOrDuplication(t *testing.T) {
	fixture := startDaemon(t)
	client := dialActor(t, fixture, 2)
	ctx := context.Background()
	conversation := ConversationBytes("conv-kill9")

	if _, err := client.Append(ctx, []AppendEvent{
		UserMsgEvent(conversation, "before the crash")}); err != nil {
		t.Fatalf("append before crash: %v", err)
	}
	if _, err := client.WriteCheckpoint(ctx, "turn-crash", []byte("cursor-1")); err != nil {
		t.Fatalf("checkpoint before crash: %v", err)
	}

	pid := fixture.supervisor.Pid()
	if pid == 0 {
		t.Fatalf("no cortexd pid")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 cortexd: %v", err)
	}
	waitForRestart(t, fixture, pid)

	if _, err := client.Append(ctx, []AppendEvent{
		UserMsgEvent(conversation, "after the crash")}); err != nil {
		t.Fatalf("append after crash: %v", err)
	}

	texts := transcriptTexts(t, client, conversation)
	occurrences := map[string]int{}
	for _, text := range texts {
		occurrences[text]++
	}
	for _, expect := range []string{
		"1:user: before the crash",
		"1:user: after the crash",
	} {
		if occurrences[expect] != 1 {
			t.Fatalf("expected exactly one %q, transcript %v", expect, texts)
		}
	}
	blob, _, err := client.LatestCheckpoint(ctx, "turn-crash")
	if err != nil || string(blob) != "cursor-1" {
		t.Fatalf("checkpoint lost across kill -9: blob=%q err=%v", blob, err)
	}
}

func TestCheckpointAttestSequencedAcrossKillNine(t *testing.T) {
	fixture := startDaemon(t)
	client := dialActor(t, fixture, 6)
	admin, err := Dial(Config{
		SocketPath:      fixture.socket,
		CapabilityToken: fixture.adminToken,
		ConnectionID:    connectionID(7),
	})
	if err != nil {
		t.Fatalf("dial admin: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	ctx := context.Background()
	conversation := ConversationBytes("conv-seq")

	anchor, err := client.Append(ctx, []AppendEvent{
		UserMsgEvent(conversation, "anchor")})
	if err != nil {
		t.Fatalf("append anchor: %v", err)
	}
	if count, err := client.Attest(ctx, []uint64{anchor.FirstLsn}, nil); err != nil || count != 1 {
		t.Fatalf("attest count=%d err=%v", count, err)
	}
	firstLsn, err := client.WriteCheckpoint(ctx, "turn-seq", []byte("blob-1"))
	if err != nil || firstLsn == 0 {
		t.Fatalf("checkpoint lsn=%d err=%v", firstLsn, err)
	}
	before, err := admin.AdminStats(ctx, 11)
	if err != nil {
		t.Fatalf("stats before: %v", err)
	}

	pid := fixture.supervisor.Pid()
	if pid == 0 {
		t.Fatalf("no cortexd pid")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 cortexd: %v", err)
	}
	waitForRestart(t, fixture, pid)

	// The dead connection forces the sequenced resend path: the write rides
	// reconnect, the rebuilt dedup chain admits it at the next sequence, and
	// it applies exactly once.
	secondLsn, err := client.WriteCheckpoint(ctx, "turn-seq", []byte("blob-2"))
	if err != nil || secondLsn <= firstLsn {
		t.Fatalf("checkpoint after crash lsn=%d err=%v", secondLsn, err)
	}
	if count, err := client.Attest(ctx, []uint64{anchor.FirstLsn}, nil); err != nil || count != 1 {
		t.Fatalf("attest after crash count=%d err=%v", count, err)
	}
	after, err := admin.AdminStats(ctx, 11)
	if err != nil {
		t.Fatalf("stats after: %v", err)
	}
	if after.LogEvents != before.LogEvents+2 {
		t.Fatalf("expected exactly one checkpoint and one attestation after crash: before=%d after=%d",
			before.LogEvents, after.LogEvents)
	}
	blob, blobLsn, err := client.LatestCheckpoint(ctx, "turn-seq")
	if err != nil || blobLsn != secondLsn || string(blob) != "blob-2" {
		t.Fatalf("latest checkpoint blob=%q lsn=%d err=%v", blob, blobLsn, err)
	}

	// Full outage: the checkpoint is admitted on the stale connection, fails
	// honestly, and stays queued at its sequence slot; the attest that
	// follows fails in ensure before a sequence is allocated, so it must not
	// queue and must not burn a slot.
	fixture.supervisor.Stop()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && fixture.supervisor.Pid() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := client.WriteCheckpoint(ctx, "turn-seq", []byte("blob-3")); !errors.Is(err, ErrWritePending) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected explicit pending checkpoint, got %v", err)
	} else {
		var pending *PendingWriteError
		if !errors.As(err, &pending) || pending.ClientSeq == 0 {
			t.Fatalf("pending checkpoint lacks sequence identity: %v", err)
		}
	}
	if _, err := client.Attest(ctx, []uint64{anchor.FirstLsn}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable attest, got %v", err)
	}
	if client.PendingLen() != 1 {
		t.Fatalf("expected exactly the admitted checkpoint queued: %d", client.PendingLen())
	}

	restarted := &Supervisor{
		Binary:       cortexdBinary(t),
		ConfigPath:   fixture.supervisor.ConfigPath,
		RestartDelay: 100 * time.Millisecond,
	}
	if err := restarted.Start(); err != nil {
		t.Fatalf("restart cortexd: %v", err)
	}
	t.Cleanup(restarted.Stop)
	waitForSocket(t, fixture.socket)

	if _, err := client.Append(ctx, []AppendEvent{
		UserMsgEvent(conversation, "flush")}); err != nil {
		t.Fatalf("append after outage: %v", err)
	}
	if client.PendingLen() != 0 {
		t.Fatalf("pending queue not flushed: %d", client.PendingLen())
	}
	final, err := admin.AdminStats(ctx, 11)
	if err != nil {
		t.Fatalf("stats final: %v", err)
	}
	if final.LogEvents != after.LogEvents+2 {
		t.Fatalf("queued writes not applied exactly once: after=%d final=%d",
			after.LogEvents, final.LogEvents)
	}
	blob, _, err = client.LatestCheckpoint(ctx, "turn-seq")
	if err != nil || string(blob) != "blob-3" {
		t.Fatalf("queued checkpoint lost: blob=%q err=%v", blob, err)
	}
}

func TestDaemonDeduplicatesExplicitLostAckSequenceReplay(t *testing.T) {
	fixture := startDaemon(t)
	admin, err := Dial(Config{
		SocketPath:      fixture.socket,
		CapabilityToken: fixture.adminToken,
		ConnectionID:    connectionID(22),
	})
	if err != nil {
		t.Fatalf("dial admin: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	ctx := context.Background()
	conversation := ConversationBytes("conv-lost-ack")
	events := []AppendEvent{UserMsgEvent(conversation, "committed without observed ack")}
	before, err := admin.AdminStats(ctx, 11)
	if err != nil {
		t.Fatalf("stats before: %v", err)
	}

	raw := appendWithoutReadingAck(t, fixture, 23, 1, events)
	waitForLogEvents(t, admin, 11, before.LogEvents+1)
	if err := raw.Close(); err != nil {
		t.Fatalf("close unacknowledged connection: %v", err)
	}

	replay := appendWithoutReadingAck(t, fixture, 23, 1, events)
	replayPayload, err := newFrameReader(replay).next(5 * time.Second)
	if err != nil {
		replay.Close()
		t.Fatalf("read explicit replay ack: %v", err)
	}
	if err := replay.Close(); err != nil {
		t.Fatalf("close replay connection: %v", err)
	}
	replayEnvelope := protocol.GetRootAsWireEnvelope(replayPayload, 0)
	replayResponse, ok := envelopeResponse(replayEnvelope)
	if !ok {
		t.Fatalf("explicit replay returned non-response")
	}
	table, err := responseTable(replayResponse, protocol.ResponsePayloadAppendAck)
	if err != nil {
		t.Fatalf("explicit replay response: %v", err)
	}
	var wireAck protocol.AppendAck
	wireAck.Init(table.Bytes, table.Pos)
	if !wireAck.Duplicate() || wireAck.ClientSeq() != 1 {
		t.Fatalf("explicit replay was not the stored duplicate: seq=%d duplicate=%v",
			wireAck.ClientSeq(), wireAck.Duplicate())
	}
	after, err := admin.AdminStats(ctx, 11)
	if err != nil {
		t.Fatalf("stats after: %v", err)
	}
	if after.LogEvents != before.LogEvents+1 {
		t.Fatalf("lost-ack resend double-applied: before=%d after=%d",
			before.LogEvents, after.LogEvents)
	}
	reader, err := Dial(dialActorConfig(fixture, 23))
	if err != nil {
		t.Fatalf("dial transcript reader: %v", err)
	}
	t.Cleanup(func() { reader.Close() })
	texts := transcriptTexts(t, reader, conversation)
	if len(texts) != 1 || texts[0] != "1:user: committed without observed ack" {
		t.Fatalf("lost-ack transcript: %v", texts)
	}
}

func TestClientPendingWriteResolvesLostAckAgainstRealCortexd(t *testing.T) {
	fixture := startDaemon(t)
	proxySocket := filepath.Join(t.TempDir(), "lost-ack.sock")
	dropped := startLostAckProxy(t, proxySocket, fixture.socket)
	cfg := dialActorConfig(fixture, 30)
	cfg.SocketPath = proxySocket
	cfg.DialTimeout = 200 * time.Millisecond
	cfg.RequestTimeout = 2 * time.Second
	client, err := Dial(cfg)
	if err != nil {
		t.Fatalf("dial through lost-ack proxy: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	conversation := ConversationBytes("conv-client-lost-ack")
	events := []AppendEvent{UserMsgEvent(conversation, "client path lost ack")}
	if _, err := client.Append(context.Background(), events); !errors.Is(err, ErrWritePending) {
		t.Fatalf("append did not retain unknown outcome: %v", err)
	}
	if client.PendingLen() != 1 {
		t.Fatalf("lost-ack write not pending: %d", client.PendingLen())
	}
	if err := <-dropped; err != nil {
		t.Fatalf("lost-ack proxy: %v", err)
	}
	startPassthroughProxy(t, proxySocket, fixture.socket)
	texts := transcriptTexts(t, client, conversation)
	if client.PendingLen() != 0 {
		t.Fatalf("lost-ack write not resolved: %d", client.PendingLen())
	}
	if len(texts) != 1 || texts[0] != "1:user: client path lost ack" {
		t.Fatalf("lost-ack client transcript: %v", texts)
	}
}

func TestClientResolvesCommittedWriteAfterProtocolResponseError(t *testing.T) {
	fixture := startDaemon(t)
	proxySocket := filepath.Join(t.TempDir(), "protocol-error.sock")
	corrupted := startProtocolErrorProxy(t, proxySocket, fixture.socket)
	cfg := dialActorConfig(fixture, 34)
	cfg.SocketPath = proxySocket
	cfg.RequestTimeout = 2 * time.Second
	client, err := Dial(cfg)
	if err != nil {
		t.Fatalf("dial through protocol-error proxy: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	conversation := ConversationBytes("conv-protocol-response")
	ack, err := client.Append(context.Background(), []AppendEvent{
		UserMsgEvent(conversation, "committed before corrupt response")})
	if err != nil {
		t.Fatalf("resolve protocol response error: %v", err)
	}
	if !ack.Duplicate || ack.ClientSeq != 1 {
		t.Fatalf("protocol response was not resolved by exact replay: %+v", ack)
	}
	if err := <-corrupted; err != nil {
		t.Fatalf("protocol-error proxy: %v", err)
	}
	texts := transcriptTexts(t, client, conversation)
	if len(texts) != 1 {
		t.Fatalf("protocol-error replay double-applied: %v", texts)
	}
}

func TestFreshClientDoesNotCollapseIdenticalHistoricalWrite(t *testing.T) {
	fixture := startDaemon(t)
	ctx := context.Background()
	conversation := ConversationBytes("conv-identical-restart")
	cfg := dialActorConfig(fixture, 31)
	events := []AppendEvent{UserMsgEvent(conversation, "same logical bytes")}
	first, err := Dial(cfg)
	if err != nil {
		t.Fatalf("dial first: %v", err)
	}
	if ack, appendErr := first.Append(ctx, events); appendErr != nil || ack.ClientSeq != 1 {
		t.Fatalf("first append ack=%+v err=%v", ack, appendErr)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	second, err := Dial(cfg)
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	t.Cleanup(func() { second.Close() })
	ack, err := second.Append(ctx, events)
	if err != nil {
		t.Fatalf("second identical append: %v", err)
	}
	if ack.ClientSeq != 2 || ack.Duplicate {
		t.Fatalf("fresh logical write collapsed into history: %+v", ack)
	}
	texts := transcriptTexts(t, second, conversation)
	if len(texts) != 2 {
		t.Fatalf("identical logical writes missing: %v", texts)
	}
}

func TestSequenceRecoveryLimitPersistsAcrossConflicts(t *testing.T) {
	fixture := startDaemon(t)
	ctx := context.Background()
	conversation := ConversationBytes("conv-recovery-budget")
	cfg := dialActorConfig(fixture, 32)
	cfg.SequenceRecoveryLimit = 1
	first, err := Dial(cfg)
	if err != nil {
		t.Fatalf("dial first: %v", err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := Dial(cfg)
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	t.Cleanup(func() { second.Close() })
	if _, err := first.Append(ctx, []AppendEvent{UserMsgEvent(conversation, "first-1")}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if ack, err := second.Append(ctx, []AppendEvent{UserMsgEvent(conversation, "second-2")}); err != nil || ack.ClientSeq != 2 {
		t.Fatalf("first recovery ack=%+v err=%v", ack, err)
	}
	if ack, err := first.Append(ctx, []AppendEvent{UserMsgEvent(conversation, "first-3")}); err != nil || ack.ClientSeq != 3 {
		t.Fatalf("first client recovery ack=%+v err=%v", ack, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := second.Append(ctx, []AppendEvent{
			UserMsgEvent(conversation, fmt.Sprintf("blocked-%d", attempt))}); !errors.Is(err, ErrSequenceRecovery) {
			t.Fatalf("attempt %d bypassed lifetime recovery limit: %v", attempt, err)
		}
	}
	texts := transcriptTexts(t, first, conversation)
	if len(texts) != 3 {
		t.Fatalf("recovery-limit writes leaked: %v", texts)
	}
}

func TestReusedConnectionIDRecoversPriorSequenceHistory(t *testing.T) {
	fixture := startDaemon(t)
	ctx := context.Background()
	conversation := ConversationBytes("conv-reused-connection")
	cfg := dialActorConfig(fixture, 24)
	first, err := Dial(cfg)
	if err != nil {
		t.Fatalf("dial first client: %v", err)
	}
	for _, content := range []string{"history-1", "history-2", "history-3"} {
		if _, err := first.Append(ctx, []AppendEvent{
			UserMsgEvent(conversation, content)}); err != nil {
			t.Fatalf("append %s: %v", content, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first client: %v", err)
	}

	second, err := Dial(cfg)
	if err != nil {
		t.Fatalf("dial reused identity: %v", err)
	}
	t.Cleanup(func() { second.Close() })
	ack, err := second.Append(ctx, []AppendEvent{
		UserMsgEvent(conversation, "history-4")})
	if err != nil {
		t.Fatalf("append after client restart: %v", err)
	}
	if ack.ClientSeq != 4 || ack.Duplicate {
		t.Fatalf("reused identity did not reach next sequence: %+v", ack)
	}
	texts := transcriptTexts(t, second, conversation)
	expected := []string{
		"1:user: history-1", "1:user: history-2",
		"1:user: history-3", "1:user: history-4",
	}
	if len(texts) != len(expected) {
		t.Fatalf("reused identity transcript: %v", texts)
	}
	for index := range expected {
		if texts[index] != expected[index] {
			t.Fatalf("reused identity transcript: got %v want %v", texts, expected)
		}
	}
}

func TestPermanentPendingHeadRejectionSurfaces(t *testing.T) {
	fixture := startDaemon(t)
	client := dialActor(t, fixture, 25)
	ctx := context.Background()
	conversation := ConversationBytes("conv-pending-rejection")
	fixture.supervisor.Stop()

	invalid := AssertionEvent(conversation, Assertion{
		BeliefID:          []byte("pending-invalid"),
		Type:              255,
		CanonicalIdentity: "pending-invalid",
		Value:             []byte("invalid"),
		ValidFromNs:       1,
		Provenance:        [][2]uint64{{1, 1}},
	})
	if _, queued, err := client.QueueAppend(ctx, []AppendEvent{invalid}); err != nil || !queued {
		t.Fatalf("queue invalid pending head queued=%v err=%v", queued, err)
	}

	restarted := &Supervisor{
		Binary:       cortexdBinary(t),
		ConfigPath:   fixture.supervisor.ConfigPath,
		RestartDelay: 100 * time.Millisecond,
	}
	if err := restarted.Start(); err != nil {
		t.Fatalf("restart cortexd: %v", err)
	}
	t.Cleanup(restarted.Stop)
	waitForSocket(t, fixture.socket)

	_, queued, err := client.QueueAppend(ctx, []AppendEvent{
		UserMsgEvent(conversation, "must not hide rejection")})
	var engineErr *EngineError
	if queued || !errors.As(err, &engineErr) {
		t.Fatalf("pending rejection not surfaced queued=%v err=%v", queued, err)
	}
	if client.PendingLen() != 0 {
		t.Fatalf("rejected pending head retained: %d", client.PendingLen())
	}
	if _, err := client.Append(ctx, []AppendEvent{
		UserMsgEvent(conversation, "after surfaced rejection")}); err != nil {
		t.Fatalf("append after surfaced rejection: %v", err)
	}
	texts := transcriptTexts(t, client, conversation)
	if len(texts) != 1 || texts[0] != "1:user: after surfaced rejection" {
		t.Fatalf("pending rejection transcript: %v", texts)
	}
}

func waitForRestart(t *testing.T, fixture *daemonFixture, oldPid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		pid := fixture.supervisor.Pid()
		if pid != 0 && pid != oldPid {
			waitForSocket(t, fixture.socket)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("supervisor never restarted cortexd")
}

func TestBoundedQueueThenHonestFailureWhileDown(t *testing.T) {
	fixture := startDaemon(t)
	client := dialActor(t, fixture, 3)
	ctx := context.Background()
	conversation := ConversationBytes("conv-queue")

	if _, err := client.Append(ctx, []AppendEvent{
		UserMsgEvent(conversation, "live-0")}); err != nil {
		t.Fatalf("append live: %v", err)
	}

	// Take cortexd fully down: stop the supervisor so nothing restarts it.
	fixture.supervisor.Stop()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && fixture.supervisor.Pid() != 0 {
		time.Sleep(20 * time.Millisecond)
	}

	queuedContents := []string{"queued-1", "queued-2", "queued-3", "queued-4"}
	for _, content := range queuedContents {
		_, queued, err := client.QueueAppend(ctx, []AppendEvent{
			UserMsgEvent(conversation, content)})
		if err != nil || !queued {
			t.Fatalf("queue append %q queued=%v err=%v", content, queued, err)
		}
	}
	if _, queued, err := client.QueueAppend(ctx, []AppendEvent{
		UserMsgEvent(conversation, "overflow")}); !errors.Is(err, ErrQueueFull) || queued {
		t.Fatalf("expected honest ErrQueueFull, queued=%v err=%v", queued, err)
	}
	if _, err := client.Append(ctx, []AppendEvent{
		UserMsgEvent(conversation, "sync-while-down")}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected typed ErrUnavailable, got %v", err)
	}

	restarted := &Supervisor{
		Binary:       cortexdBinary(t),
		ConfigPath:   fixture.supervisor.ConfigPath,
		RestartDelay: 100 * time.Millisecond,
	}
	if err := restarted.Start(); err != nil {
		t.Fatalf("restart cortexd: %v", err)
	}
	t.Cleanup(restarted.Stop)
	waitForSocket(t, fixture.socket)

	if _, err := client.Append(ctx, []AppendEvent{
		UserMsgEvent(conversation, "after-recovery")}); err != nil {
		t.Fatalf("append after recovery: %v", err)
	}
	if client.PendingLen() != 0 {
		t.Fatalf("pending queue not flushed: %d", client.PendingLen())
	}

	texts := transcriptTexts(t, client, conversation)
	// The synchronous append while down failed with a typed error BEFORE a
	// sequence was allocated, so it must not surface anywhere: the caller
	// owned that durability decision.
	expected := []string{
		"1:user: live-0",
		"1:user: queued-1",
		"1:user: queued-2",
		"1:user: queued-3",
		"1:user: queued-4",
		"1:user: after-recovery",
	}
	if len(texts) != len(expected) {
		t.Fatalf("transcript %v", texts)
	}
	for index, expect := range expected {
		if texts[index] != expect {
			t.Fatalf("transcript order: got %v want %v", texts, expected)
		}
	}
}

func TestLoopSeamQueuedFlushUpdatesProvenanceOnce(t *testing.T) {
	fixture := startDaemon(t)
	client := dialActor(t, fixture, 33)
	seam, err := NewLoopSeam(client, SeamConfig{
		ConversationID: "conv-queued-provenance",
		BudgetTokens:   1024,
	})
	if err != nil {
		t.Fatalf("new seam: %v", err)
	}
	fixture.supervisor.Stop()
	seam.RecordUser("queued provenance")
	if client.PendingLen() != 1 {
		t.Fatalf("record was not queued: %d", client.PendingLen())
	}
	if _, lo, hi := seam.ProvenanceRange(); lo != 0 || hi != 0 {
		t.Fatalf("queued write acknowledged before flush: %d %d", lo, hi)
	}
	restarted := &Supervisor{
		Binary:       cortexdBinary(t),
		ConfigPath:   fixture.supervisor.ConfigPath,
		RestartDelay: 100 * time.Millisecond,
	}
	if err := restarted.Start(); err != nil {
		t.Fatalf("restart cortexd: %v", err)
	}
	t.Cleanup(restarted.Stop)
	waitForSocket(t, fixture.socket)
	seam.RecordDelivery("live provenance")
	if err := seam.RecordError(); err != nil {
		t.Fatalf("record error after flush: %v", err)
	}
	conversation, lo, hi := seam.ProvenanceRange()
	if conversation != "conv-queued-provenance" || lo == 0 || hi != lo+1 {
		t.Fatalf("flushed provenance range %q %d %d", conversation, lo, hi)
	}
	if client.PendingLen() != 0 {
		t.Fatalf("queued provenance not flushed: %d", client.PendingLen())
	}
}

func TestCapabilityIsolation(t *testing.T) {
	fixture := startDaemon(t)

	var wrongToken [32]byte
	for index := range wrongToken {
		wrongToken[index] = 0x99
	}
	if _, err := Dial(Config{
		SocketPath:      fixture.socket,
		CapabilityToken: wrongToken,
		ConnectionID:    connectionID(9),
	}); err == nil {
		t.Fatalf("expected capability rejection for unknown token")
	}

	actor := dialActor(t, fixture, 4)
	if _, err := actor.AdminHealth(context.Background()); err == nil {
		t.Fatalf("actor connection must not reach the admin surface")
	} else {
		var engineErr *EngineError
		if !errors.As(err, &engineErr) || engineErr.Code != CodeCapabilityDenied {
			t.Fatalf("expected kCapabilityDenied, got %v", err)
		}
	}

	admin, err := Dial(Config{
		SocketPath:      fixture.socket,
		CapabilityToken: fixture.adminToken,
		ConnectionID:    connectionID(5),
	})
	if err != nil {
		t.Fatalf("dial admin: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	health, err := admin.AdminHealth(context.Background())
	if err != nil || !health.Ready || health.ActorCount != 1 {
		t.Fatalf("admin health %+v err=%v", health, err)
	}
	status, err := admin.AdminVerifyStatus(context.Background(), 11)
	if err != nil || status.Actor != 11 {
		t.Fatalf("verify status %+v err=%v", status, err)
	}
	if _, err := admin.AdminRebuildProjection(context.Background(), 11, "all"); err != nil {
		t.Fatalf("rebuild projection: %v", err)
	}
}
