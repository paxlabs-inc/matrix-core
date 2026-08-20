package mail

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
	"centra/workforce/internal/ledger"
)

const mailPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var mailPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startMailPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mail integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	mailPool, err = waitMailPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "mail integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, mailPool, mailBaseTime()); err != nil {
		mailPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "mail migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	mailPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_MailStore_ConcurrentAtLeastOnceDeliveryAndConsumption(t *testing.T) {
	fixture := newMailFixture(t, "delivery", defaultMailConfig())
	ctx := context.Background()
	envelope := fixture.envelope(t, "root", contracts.MessageInformation,
		fixture.recipient.address)
	var wait sync.WaitGroup
	results := make(chan SendResult, 12)
	errs := make(chan error, 12)
	for index := 0; index < 12; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := fixture.store.Send(ctx, envelope, SendOptions{})
			results <- result
			errs <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent duplicate send: %v", err)
		}
	}
	var first, duplicates int
	for result := range results {
		if result.Deduplicated {
			duplicates++
		} else {
			first++
		}
	}
	if first != 1 || duplicates != 11 {
		t.Fatalf("send identities first=%d duplicates=%d", first, duplicates)
	}
	changed := envelope
	changed.ID = "message:other"
	changed.Subject = "different"
	if err := SignEnvelope(&changed, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, changed, SendOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting idempotency = %v", err)
	}
	if _, _, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID, SeatID: fixture.recipient.address.SeatID,
		MessageID: envelope.ID, IdempotencyKey: "consume:root",
	}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("queued message consumed: %v", err)
	}
	delivered, err := fixture.store.Dispatch(ctx, fixture.organizationID, 100)
	if err != nil || delivered != 1 {
		t.Fatalf("dispatch = %d, %v", delivered, err)
	}
	inbox, err := fixture.store.Inbox(ctx, fixture.organizationID,
		fixture.recipient.address.SeatID, 10)
	if err != nil || len(inbox) != 1 || inbox[0].State != StateDelivered ||
		!inbox[0].BindingReady {
		t.Fatalf("inbox = %+v, %v", inbox, err)
	}
	opened, duplicate, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID, SeatID: fixture.recipient.address.SeatID,
		MessageID: envelope.ID, IdempotencyKey: "consume:root",
	})
	if err != nil || duplicate || opened.ID != envelope.ID {
		t.Fatalf("consume = %+v, duplicate=%v, %v", opened, duplicate, err)
	}
	_, duplicate, err = fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID, SeatID: fixture.recipient.address.SeatID,
		MessageID: envelope.ID, IdempotencyKey: "consume:root",
	})
	if err != nil || !duplicate {
		t.Fatalf("duplicate consume = %v, %v", duplicate, err)
	}
	if _, _, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID, SeatID: fixture.recipient.address.SeatID,
		MessageID: envelope.ID, IdempotencyKey: "consume:other",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting consume = %v", err)
	}
	for _, state := range []DeliveryState{
		StateAcknowledged, StateReplied, StateResolved,
	} {
		if err := fixture.store.Transition(ctx, TransitionRequest{
			OrganizationID: fixture.organizationID,
			SeatID:         fixture.recipient.address.SeatID, MessageID: envelope.ID,
			State: state, IdempotencyKey: "transition:" + string(state),
		}); err != nil {
			t.Fatalf("transition %s: %v", state, err)
		}
	}
	if err := fixture.store.Transition(ctx, TransitionRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.recipient.address.SeatID, MessageID: envelope.ID,
		State: StateResolved, IdempotencyKey: "transition:duplicate",
	}); err != nil {
		t.Fatalf("duplicate terminal transition: %v", err)
	}
	if err := fixture.store.Transition(ctx, TransitionRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.recipient.address.SeatID, MessageID: envelope.ID,
		State: StateAcknowledged, IdempotencyKey: "transition:regress",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal regression = %v", err)
	}
	var wakes, access int
	if err := mailPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_wake_requests
		WHERE tenant_id=$1 AND organization_id=$2 AND source_id=$3
	`, fixture.tenantID, fixture.organizationID, envelope.ID).Scan(&wakes); err != nil {
		t.Fatal(err)
	}
	if err := mailPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_mail_access_log
		WHERE tenant_id=$1 AND organization_id=$2 AND message_id=$3
	`, fixture.tenantID, fixture.organizationID, envelope.ID).Scan(&access); err != nil {
		t.Fatal(err)
	}
	if wakes != 1 || access != 6 {
		t.Fatalf("wake/access counts = %d/%d", wakes, access)
	}
}

func TestIntegration_MailStore_DelegationBindsGraphWithoutAuthorityEscalation(t *testing.T) {
	fixture := newMailFixture(t, "delegation", defaultMailConfig())
	ctx := context.Background()
	parentID := dependency.NodeID("intent:delegation")
	senderSeat := fixture.sender.address.SeatID
	if err := fixture.graph.PutNode(ctx, dependency.Node{
		ID: parentID, OrganizationID: fixture.organizationID,
		Kind: dependency.NodeIntent, OwnerSeatID: &senderSeat,
		Title: "parent intent", State: dependency.StateEligible,
		BasePriority: 5, CreatedAt: *fixture.now, UpdatedAt: *fixture.now, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	envelope := fixture.envelope(t, "delegation", contracts.MessageDelegation,
		fixture.recipient.address)
	envelope.ParentIntentID = contracts.IntentID(parentID)
	if err := SignEnvelope(&envelope, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	recipientSeat := fixture.recipient.address.SeatID
	expires := envelope.ExpiresAt
	sla := envelope.CreatedAt.Add(15 * time.Minute)
	binding := DelegationBinding{
		Node: dependency.Node{
			ID: "delegation:child", OrganizationID: fixture.organizationID,
			Kind: dependency.NodeDelegation, OwnerSeatID: &recipientSeat,
			Title: "bounded delegated work", State: dependency.StatePending,
			BasePriority: 5, CreatedAt: *fixture.now, UpdatedAt: *fixture.now, Version: 1,
		},
		Edge: dependency.Edge{
			OrganizationID: fixture.organizationID, Prerequisite: parentID,
			Dependent: "delegation:child", Kind: dependency.EdgeDelegation,
			RequiredResponseSchema: "workforce.mail.answer.v1",
			ExpiresAt:              &expires, TimeoutAction: envelope.TimeoutAction,
			SLAAt: &sla, CreatedAt: *fixture.now,
		},
	}
	result, err := fixture.store.Send(ctx, envelope, SendOptions{Delegation: &binding})
	if err != nil || !result.BindingReady {
		t.Fatalf("delegation send = %+v, %v", result, err)
	}
	snapshot, err := fixture.graph.Snapshot(ctx, fixture.organizationID)
	if err != nil {
		t.Fatal(err)
	}
	var foundNode, foundEdge bool
	for _, node := range snapshot.Nodes {
		foundNode = foundNode || node.ID == binding.Node.ID
	}
	for _, edge := range snapshot.Edges {
		foundEdge = foundEdge || edge.Dependent == binding.Edge.Dependent &&
			edge.Prerequisite == binding.Edge.Prerequisite
	}
	if !foundNode || !foundEdge {
		t.Fatalf("delegation graph missing node=%v edge=%v", foundNode, foundEdge)
	}
	invalidNode := binding
	invalidNode.Node.Title = ""
	if err := validateDelegation(envelope, invalidNode); err == nil {
		t.Fatal("delegation with invalid node accepted")
	}
	invalidEdge := binding
	invalidEdge.Edge.RequiredResponseSchema = ""
	if err := validateDelegation(envelope, invalidEdge); err == nil {
		t.Fatal("delegation with invalid edge accepted")
	}
	var leases int
	if err := mailPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_runtime_leases
		WHERE tenant_id=$1 AND organization_id=$2 AND seat_id=$3
	`, fixture.tenantID, fixture.organizationID,
		fixture.recipient.address.SeatID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("incoming delegation granted %d leases", leases)
	}
	malicious := fixture.envelope(t, "delegation-malicious",
		contracts.MessageDelegation, fixture.recipient.address)
	malicious.ParentIntentID = contracts.IntentID(parentID)
	if err := SignEnvelope(&malicious, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	bad := binding
	bad.Node.ID = "delegation:bad"
	bad.Edge.Dependent = bad.Node.ID
	bad.Node.OwnerSeatID = &senderSeat
	if _, err := fixture.store.Send(ctx, malicious,
		SendOptions{Delegation: &bad}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("authority-escalating delegation = %v", err)
	}
}

func TestIntegration_MailStore_CorrectionNoticeRemainsMandatoryUntilConsumed(t *testing.T) {
	fixture := newMailFixture(t, "correction", defaultMailConfig())
	ctx := context.Background()
	insertCorrectionFixture(t, fixture)
	envelope := fixture.envelope(t, "correction", contracts.MessageCorrection,
		fixture.recipient.address)
	binding := CorrectionBinding{
		NoticeID: "notice:correction", CorrectionID: "correction:mail",
		AffectedRecord:  "record:affected",
		RecipientSeatID: fixture.recipient.address.SeatID,
	}
	result, err := fixture.store.Send(ctx, envelope, SendOptions{Correction: &binding})
	if err != nil || !result.BindingReady {
		t.Fatalf("correction send = %+v, %v", result, err)
	}
	var state string
	if err := mailPool.QueryRow(ctx, `
		SELECT state FROM workforce_correction_notices
		WHERE tenant_id=$1 AND organization_id=$2 AND notice_id=$3
	`, fixture.tenantID, fixture.organizationID, binding.NoticeID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("unopened correction notice state = %s", state)
	}
	if _, err := fixture.store.Dispatch(ctx, fixture.organizationID, 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.recipient.address.SeatID,
		MessageID:      envelope.ID, IdempotencyKey: "consume:correction",
	}); err != nil {
		t.Fatal(err)
	}
	if err := mailPool.QueryRow(ctx, `
		SELECT state FROM workforce_correction_notices
		WHERE tenant_id=$1 AND organization_id=$2 AND notice_id=$3
	`, fixture.tenantID, fixture.organizationID, binding.NoticeID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "delivered" {
		t.Fatalf("opened correction notice state = %s", state)
	}
	if _, err := mailPool.Exec(ctx, `
		UPDATE workforce_mail_recipients
		SET state='delivered',consumption_key=NULL,opened_at=NULL
		WHERE tenant_id=$1 AND organization_id=$2 AND message_id=$3
		  AND recipient_seat_id=$4
	`, fixture.tenantID, fixture.organizationID, envelope.ID,
		fixture.recipient.address.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, duplicate, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.recipient.address.SeatID,
		MessageID:      envelope.ID, IdempotencyKey: "consume:correction-again",
	}); err != nil || duplicate {
		t.Fatalf("correction redelivery = duplicate=%v, %v", duplicate, err)
	}
	if _, err := mailPool.Exec(ctx, `
		DELETE FROM workforce_correction_notices
		WHERE tenant_id=$1 AND organization_id=$2 AND notice_id=$3
	`, fixture.tenantID, fixture.organizationID, binding.NoticeID); err != nil {
		t.Fatal(err)
	}
	if _, err := mailPool.Exec(ctx, `
		UPDATE workforce_mail_recipients
		SET state='delivered',consumption_key=NULL,opened_at=NULL
		WHERE tenant_id=$1 AND organization_id=$2 AND message_id=$3
		  AND recipient_seat_id=$4
	`, fixture.tenantID, fixture.organizationID, envelope.ID,
		fixture.recipient.address.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.recipient.address.SeatID,
		MessageID:      envelope.ID, IdempotencyKey: "consume:correction-conflict",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("acknowledged correction redelivery = %v", err)
	}
}

func TestIntegration_MailStore_QuotasLoopsExpiryAndTimeouts(t *testing.T) {
	config := defaultMailConfig()
	config.MaxMailboxMessages = 1
	config.MaxThreadMessages = 2
	config.MaxThreadDepth = 2
	config.MaxRecipients = 2
	config.MaxAutoReplies = 1
	config.MaxAttachmentBytes = 1024
	config.MaxMessageLifetime = time.Hour
	fixture := newMailFixture(t, "bounds", config)
	ctx := context.Background()
	root := fixture.envelope(t, "bounds-root", contracts.MessageInformation,
		fixture.recipient.address)
	if _, err := fixture.store.Send(ctx, root, SendOptions{Automatic: true}); err != nil {
		t.Fatal(err)
	}
	quota := fixture.envelope(t, "bounds-mailbox", contracts.MessageInformation,
		fixture.recipient.address)
	if _, err := fixture.store.Send(ctx, quota, SendOptions{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("mailbox quota = %v", err)
	}
	reply := fixture.reply(t, root, "bounds-reply", contracts.MessageAnswer)
	if _, err := fixture.store.Send(ctx, reply,
		SendOptions{Automatic: true}); !errors.Is(err, ErrQuota) {
		t.Fatalf("automatic reply loop = %v", err)
	}
	oversized := fixture.envelope(t, "bounds-size", contracts.MessageInformation,
		fixture.other.address)
	oversized.Artifacts = []contracts.ArtifactRef{mailArtifact("large", 1024)}
	if err := SignEnvelope(&oversized, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, oversized, SendOptions{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("attachment quota = %v", err)
	}
	duplicateRecipient := fixture.envelope(t, "bounds-duplicate",
		contracts.MessageInformation, fixture.other.address)
	duplicateRecipient.CC = []contracts.SeatAddress{fixture.other.address}
	if err := SignEnvelope(&duplicateRecipient, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, duplicateRecipient,
		SendOptions{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("duplicate recipient = %v", err)
	}
	tooMany := fixture.envelope(t, "bounds-recipients",
		contracts.MessageInformation, fixture.other.address)
	tooMany.To = append(tooMany.To, fixture.recipient.address)
	tooMany.CC = []contracts.SeatAddress{fixture.sender.address}
	if err := SignEnvelope(&tooMany, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, tooMany, SendOptions{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("recipient bound = %v", err)
	}
	lifetime := fixture.envelope(t, "bounds-lifetime",
		contracts.MessageInformation, fixture.other.address)
	lifetime.ExpiresAt = lifetime.CreatedAt.Add(2 * time.Hour)
	if err := SignEnvelope(&lifetime, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, lifetime, SendOptions{}); !errors.Is(err, ErrExpired) {
		t.Fatalf("lifetime bound = %v", err)
	}
	if _, err := fixture.store.Dispatch(ctx, fixture.organizationID, 10); err != nil {
		t.Fatal(err)
	}
	*fixture.now = fixture.now.Add(2 * time.Hour)
	if _, _, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.recipient.address.SeatID,
		MessageID:      root.ID, IdempotencyKey: "consume:expired",
	}); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired consume = %v", err)
	}
	var timeouts int
	if err := mailPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_mail_timeouts
		WHERE tenant_id=$1 AND organization_id=$2 AND message_id=$3
	`, fixture.tenantID, fixture.organizationID, root.ID).Scan(&timeouts); err != nil {
		t.Fatal(err)
	}
	if timeouts != 1 {
		t.Fatalf("timeout actions = %d", timeouts)
	}
}

func TestIntegration_MailStore_SignatureTenantAndRevocationFailClosed(t *testing.T) {
	fixture := newMailFixture(t, "security", defaultMailConfig())
	ctx := context.Background()
	privateMail := fixture.envelope(t, "security-private-mailbox",
		contracts.MessageInformation, fixture.recipient.address)
	if _, err := fixture.store.Send(ctx, privateMail, SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if delivered, err := fixture.store.Dispatch(ctx, fixture.organizationID, 10); err != nil ||
		delivered != 1 {
		t.Fatalf("dispatch private mailbox message = %d, %v", delivered, err)
	}
	if inbox, err := fixture.store.Inbox(
		ctx, fixture.organizationID, fixture.other.address.SeatID, 10,
	); err != nil || len(inbox) != 0 {
		t.Fatalf("another seat observed private mailbox state: %#v, %v", inbox, err)
	}
	if _, _, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.other.address.SeatID,
		MessageID:      privateMail.ID,
		IdempotencyKey: "consume:wrong-seat",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another seat consumed private mailbox state: %v", err)
	}
	otherTenant := newMailFixture(t, "security-other-tenant", defaultMailConfig())
	if inbox, err := otherTenant.store.Inbox(
		ctx, fixture.organizationID, fixture.recipient.address.SeatID, 10,
	); err != nil || len(inbox) != 0 {
		t.Fatalf("another tenant observed private mailbox state: %#v, %v", inbox, err)
	}
	if _, _, err := otherTenant.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.recipient.address.SeatID,
		MessageID:      privateMail.ID,
		IdempotencyKey: "consume:wrong-tenant",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another tenant consumed private mailbox state: %v", err)
	}

	envelope := fixture.envelope(t, "security-tamper",
		contracts.MessageInformation, fixture.recipient.address)
	envelope.Subject = "tampered after signature"
	if _, err := fixture.store.Send(ctx, envelope, SendOptions{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered signature = %v", err)
	}
	valid := fixture.envelope(t, "security-valid",
		contracts.MessageInformation, fixture.recipient.address)
	if err := fixture.store.RevokeSeatKey(ctx, fixture.organizationID,
		fixture.sender.address.SeatID, fixture.sender.keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, valid, SendOptions{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked sender = %v", err)
	}
	if err := fixture.store.RevokeSeatKey(ctx, fixture.organizationID,
		fixture.sender.address.SeatID, fixture.sender.keyID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("duplicate revocation = %v", err)
	}
	crossTenant := valid
	crossTenant.ID = "message:cross-tenant"
	crossTenant.IdempotencyKey = "send:cross-tenant"
	crossTenant.To[0].OrganizationID = "organization:other"
	if err := SignEnvelope(&crossTenant, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, crossTenant,
		SendOptions{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-tenant recipient = %v", err)
	}
}

func TestIntegration_MailStore_ValidationBindingAndLifecycleFailures(t *testing.T) {
	fixture := newMailFixture(t, "failure-paths", defaultMailConfig())
	ctx := context.Background()

	if _, err := fixture.store.ResolveBindings(ctx, fixture.organizationID, 0); err == nil {
		t.Fatal("zero binding limit accepted")
	}
	if _, err := fixture.store.ResolveBindings(ctx, fixture.organizationID, 1001); err == nil {
		t.Fatal("oversized binding limit accepted")
	}
	if _, err := fixture.store.Dispatch(ctx, fixture.organizationID, 0); err == nil {
		t.Fatal("zero dispatch limit accepted")
	}
	if _, err := fixture.store.Dispatch(ctx, fixture.organizationID, 10001); err == nil {
		t.Fatal("oversized dispatch limit accepted")
	}
	if _, err := fixture.store.Inbox(ctx, fixture.organizationID,
		fixture.recipient.address.SeatID, 0); err == nil {
		t.Fatal("zero inbox limit accepted")
	}
	if _, err := fixture.store.Inbox(ctx, fixture.organizationID,
		fixture.recipient.address.SeatID, 1001); err == nil {
		t.Fatal("oversized inbox limit accepted")
	}
	if err := fixture.store.Transition(ctx, TransitionRequest{
		State: StateDelivered, IdempotencyKey: "invalid:transition",
	}); err == nil {
		t.Fatal("implicit delivery transition accepted")
	}
	if err := fixture.store.Transition(ctx, TransitionRequest{
		State: StateRejected, IdempotencyKey: "bad value",
	}); err == nil {
		t.Fatal("invalid transition idempotency accepted")
	}
	if err := fixture.store.Transition(ctx, TransitionRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.recipient.address.SeatID,
		MessageID:      "message:missing",
		State:          StateRejected, IdempotencyKey: "reject:missing",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown transition = %v", err)
	}

	invalidClock := *fixture.store
	invalidClock.now = func() time.Time { return time.Time{} }
	if _, err := invalidClock.Dispatch(ctx, fixture.organizationID, 1); !errors.Is(err, ErrUncertain) {
		t.Fatalf("invalid clock = %v", err)
	}
	clockEnvelope := fixture.envelope(t, "invalid-clock",
		contracts.MessageInformation, fixture.other.address)
	if _, err := invalidClock.Send(ctx, clockEnvelope,
		SendOptions{}); !errors.Is(err, ErrUncertain) {
		t.Fatalf("invalid-clock send = %v", err)
	}
	if err := invalidClock.RevokeSeatKey(ctx, fixture.organizationID,
		fixture.sender.address.SeatID, fixture.sender.keyID); !errors.Is(err, ErrUncertain) {
		t.Fatalf("invalid-clock revocation = %v", err)
	}
	for index, request := range []ConsumeRequest{
		{OrganizationID: "bad value", SeatID: fixture.recipient.address.SeatID, MessageID: "message:x", IdempotencyKey: "consume:x"},
		{OrganizationID: fixture.organizationID, SeatID: "bad value", MessageID: "message:x", IdempotencyKey: "consume:x"},
		{OrganizationID: fixture.organizationID, SeatID: fixture.recipient.address.SeatID, MessageID: "bad value", IdempotencyKey: "consume:x"},
		{OrganizationID: fixture.organizationID, SeatID: fixture.recipient.address.SeatID, MessageID: "message:x", IdempotencyKey: "bad value"},
	} {
		if _, _, err := fixture.store.Consume(ctx, request); err == nil {
			t.Fatalf("invalid consume request %d accepted", index)
		}
	}
	if _, _, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.recipient.address.SeatID,
		MessageID:      "message:not-found", IdempotencyKey: "consume:not-found",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown consumption = %v", err)
	}
	if _, err := New(mailPool, mailVault(t, "tenant:different"), fixture.graph,
		fixture.tenantID, defaultMailConfig(), func() time.Time { return *fixture.now }); err == nil {
		t.Fatal("mismatched Vault tenancy accepted")
	}

	unknownSeat := newMailSeat(t, fixture.organizationID,
		"department:unknown", "seat:unknown")
	if err := fixture.store.PublishSeatKey(ctx, SeatKey{}); err == nil {
		t.Fatal("empty seat key accepted")
	}
	if err := fixture.store.PublishSeatKey(ctx, SeatKey{
		Address: unknownSeat.address, KeyID: unknownSeat.keyID,
		PublicKey: unknownSeat.public, EffectiveAt: *fixture.now,
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("key without authority = %v", err)
	}
	if err := fixture.store.PublishSeatKey(ctx, SeatKey{
		Address: fixture.sender.address, KeyID: "bad value",
		PublicKey: fixture.sender.public, EffectiveAt: *fixture.now,
	}); err == nil {
		t.Fatal("invalid key id accepted")
	}
	if err := fixture.store.PublishSeatKey(ctx, SeatKey{
		Address: fixture.sender.address, KeyID: "key:short",
		PublicKey: []byte{1}, EffectiveAt: *fixture.now,
	}); err == nil {
		t.Fatal("short public key accepted")
	}
	if err := fixture.store.PublishSeatKey(ctx, SeatKey{
		Address: fixture.sender.address, KeyID: fixture.sender.keyID,
		PublicKey: fixture.sender.public, EffectiveAt: *fixture.now,
	}); err != nil {
		t.Fatalf("idempotent key publication = %v", err)
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PublishSeatKey(ctx, SeatKey{
		Address: fixture.sender.address, KeyID: fixture.sender.keyID,
		PublicKey: otherPublic, EffectiveAt: *fixture.now,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting key publication = %v", err)
	}

	missingBinding := fixture.envelope(t, "missing-binding",
		contracts.MessageCorrection, fixture.recipient.address)
	if _, err := fixture.store.Send(ctx, missingBinding, SendOptions{}); err == nil {
		t.Fatal("correction without binding accepted")
	}
	missingDelegation := fixture.envelope(t, "missing-delegation",
		contracts.MessageDelegation, fixture.recipient.address)
	if _, err := fixture.store.Send(ctx, missingDelegation, SendOptions{}); err == nil {
		t.Fatal("delegation without binding accepted")
	}
	missingParent := fixture.envelope(t, "missing-parent",
		contracts.MessageDelegation, fixture.other.address)
	missingParentBinding := DelegationBinding{
		Node: dependency.Node{
			ID: "delegation:missing-parent", OrganizationID: fixture.organizationID,
			Kind: dependency.NodeDelegation, OwnerSeatID: &fixture.other.address.SeatID,
			Title: "unbound delegation", State: dependency.StatePending,
			BasePriority: 5, CreatedAt: *fixture.now, UpdatedAt: *fixture.now, Version: 1,
		},
	}
	expires := missingParent.ExpiresAt
	sla := missingParent.CreatedAt.Add(10 * time.Minute)
	missingParentBinding.Edge = dependency.Edge{
		OrganizationID: fixture.organizationID,
		Prerequisite:   dependency.NodeID(missingParent.ParentIntentID),
		Dependent:      missingParentBinding.Node.ID, Kind: dependency.EdgeDelegation,
		RequiredResponseSchema: "workforce.mail.answer.v1",
		ExpiresAt:              &expires, TimeoutAction: missingParent.TimeoutAction,
		SLAAt: &sla, CreatedAt: *fixture.now,
	}
	if _, err := fixture.store.Send(ctx, missingParent,
		SendOptions{Delegation: &missingParentBinding}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("delegation with missing parent = %v", err)
	}
	mismatchedCorrection := fixture.envelope(t, "mismatched-correction",
		contracts.MessageCorrection, fixture.other.address)
	if _, err := fixture.store.Send(ctx, mismatchedCorrection, SendOptions{
		Correction: &CorrectionBinding{
			NoticeID: "notice:mismatch", CorrectionID: "correction:mismatch",
			AffectedRecord:  "record:mismatch",
			RecipientSeatID: fixture.sender.address.SeatID,
		},
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("mismatched correction recipient = %v", err)
	}
	forbiddenBinding := fixture.envelope(t, "forbidden-binding",
		contracts.MessageInformation, fixture.recipient.address)
	if _, err := fixture.store.Send(ctx, forbiddenBinding, SendOptions{
		Correction: &CorrectionBinding{},
	}); err == nil {
		t.Fatal("information message binding accepted")
	}
	badAlgorithm := fixture.envelope(t, "bad-algorithm",
		contracts.MessageInformation, fixture.recipient.address)
	badAlgorithm.Signature.Algorithm = "rsa"
	if _, err := fixture.store.Send(ctx, badAlgorithm, SendOptions{}); err == nil {
		t.Fatalf("invalid signature algorithm = %v", err)
	}
	badEncoding := fixture.envelope(t, "bad-encoding",
		contracts.MessageInformation, fixture.recipient.address)
	badEncoding.Signature.Value = "not+base64"
	if _, err := fixture.store.Send(ctx, badEncoding, SendOptions{}); err == nil {
		t.Fatalf("invalid signature encoding = %v", err)
	}
	wrongDepartment := fixture.envelope(t, "wrong-department",
		contracts.MessageInformation, fixture.recipient.address)
	wrongDepartment.From.DepartmentID = fixture.other.address.DepartmentID
	if err := SignEnvelope(&wrongDepartment, fixture.sender.keyID,
		fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, wrongDepartment,
		SendOptions{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong sender department = %v", err)
	}
	overflow := fixture.envelope(t, "attachment-overflow",
		contracts.MessageInformation, fixture.other.address)
	overflow.Artifacts = []contracts.ArtifactRef{mailArtifact("overflow", ^uint64(0))}
	if err := SignEnvelope(&overflow, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, overflow, SendOptions{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("attachment overflow = %v", err)
	}
	if err := fixture.store.RevokeSeatKey(ctx, fixture.organizationID,
		fixture.recipient.address.SeatID, fixture.recipient.keyID); err != nil {
		t.Fatal(err)
	}
	revokedRecipient := fixture.envelope(t, "revoked-recipient",
		contracts.MessageInformation, fixture.recipient.address)
	if _, err := fixture.store.Send(ctx, revokedRecipient,
		SendOptions{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked recipient = %v", err)
	}

	invalidCorrection := fixture.envelope(t, "binding-error",
		contracts.MessageCorrection, fixture.other.address)
	result, err := fixture.store.Send(ctx, invalidCorrection, SendOptions{
		Correction: &CorrectionBinding{
			NoticeID: "notice:missing", CorrectionID: "correction:missing",
			AffectedRecord:  "record:missing",
			RecipientSeatID: fixture.other.address.SeatID,
		},
	})
	if !errors.Is(err, ErrUnauthorized) || result.BindingReady {
		t.Fatalf("missing correction binding = %+v, %v", result, err)
	}
	var bindingState, bindingError string
	if err := mailPool.QueryRow(ctx, `
		SELECT binding_state,COALESCE(binding_error,'')
		FROM workforce_mail_messages
		WHERE tenant_id=$1 AND organization_id=$2 AND message_id=$3
	`, fixture.tenantID, fixture.organizationID, invalidCorrection.ID).Scan(
		&bindingState, &bindingError,
	); err != nil {
		t.Fatal(err)
	}
	if bindingState != "pending" || bindingError == "" {
		t.Fatalf("binding recovery state = %s, %q", bindingState, bindingError)
	}
}

func TestIntegration_MailStore_ThreadDepthAutomaticAndTerminalVariants(t *testing.T) {
	config := defaultMailConfig()
	config.MaxThreadDepth = 2
	config.MaxAutoReplies = 1
	fixture := newMailFixture(t, "threads", config)
	ctx := context.Background()

	root := fixture.envelope(t, "thread-root", contracts.MessageInformation,
		fixture.recipient.address)
	if _, err := fixture.store.Send(ctx, root, SendOptions{Automatic: true}); err != nil {
		t.Fatal(err)
	}
	reply := fixture.reply(t, root, "thread-reply", contracts.MessageAnswer)
	if _, err := fixture.store.Send(ctx, reply, SendOptions{}); err != nil {
		t.Fatalf("valid reply = %v", err)
	}
	deep := fixture.reply(t, reply, "thread-deep", contracts.MessageAnswer)
	if _, err := fixture.store.Send(ctx, deep, SendOptions{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("depth overflow = %v", err)
	}
	automatic := fixture.reply(t, root, "thread-auto", contracts.MessageAnswer)
	if _, err := fixture.store.Send(ctx, automatic,
		SendOptions{Automatic: true}); !errors.Is(err, ErrQuota) {
		t.Fatalf("automatic reply quota = %v", err)
	}
	missing := fixture.envelope(t, "thread-missing", contracts.MessageAnswer,
		fixture.other.address)
	missing.ThreadID = root.ThreadID
	parent := contracts.MessageID("message:not-found")
	missing.InReplyTo = &parent
	if err := SignEnvelope(&missing, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, missing, SendOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing reply parent = %v", err)
	}
	secondRoot := fixture.envelope(t, "thread-second-root",
		contracts.MessageInformation, fixture.other.address)
	secondRoot.ThreadID = root.ThreadID
	if err := SignEnvelope(&secondRoot, fixture.sender.keyID,
		fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Send(ctx, secondRoot, SendOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second thread root = %v", err)
	}

	expiring := fixture.envelope(t, "dispatch-expired",
		contracts.MessageInformation, fixture.other.address)
	if _, err := fixture.store.Send(ctx, expiring, SendOptions{}); err != nil {
		t.Fatal(err)
	}
	*fixture.now = fixture.now.Add(time.Hour)
	delivered, err := fixture.store.Dispatch(ctx, fixture.organizationID, 100)
	if err != nil || delivered != 0 {
		t.Fatalf("expired dispatch = %d, %v", delivered, err)
	}
	if _, _, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID,
		SeatID:         fixture.other.address.SeatID,
		MessageID:      expiring.ID, IdempotencyKey: "consume:terminal-expired",
	}); !errors.Is(err, ErrExpired) {
		t.Fatalf("terminal expired consume = %v", err)
	}

	*fixture.now = fixture.now.Add(-time.Hour)
	rejected := fixture.envelope(t, "terminal-rejected",
		contracts.MessageInformation, fixture.other.address)
	if _, err := fixture.store.Send(ctx, rejected, SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Dispatch(ctx, fixture.organizationID, 100); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Transition(ctx, TransitionRequest{
		OrganizationID: fixture.organizationID, SeatID: fixture.other.address.SeatID,
		MessageID: rejected.ID, State: StateRejected,
		IdempotencyKey: "transition:rejected",
	}); err != nil {
		t.Fatalf("reject delivered message = %v", err)
	}
	if _, _, err := fixture.store.Consume(ctx, ConsumeRequest{
		OrganizationID: fixture.organizationID, SeatID: fixture.other.address.SeatID,
		MessageID: rejected.ID, IdempotencyKey: "consume:rejected",
	}); !errors.Is(err, ErrExpired) {
		t.Fatalf("rejected consume = %v", err)
	}
}

func TestMailTypes_RejectInvalidConstructionSigningAndTransitions(t *testing.T) {
	if _, err := New(nil, nil, nil, "", Config{}, nil); err == nil {
		t.Fatal("empty store construction accepted")
	}
	invalidConfigs := []Config{
		{},
		{MaxMailboxMessages: 100001, MaxThreadMessages: 1, MaxThreadDepth: 1, MaxRecipients: 1, MaxAutoReplies: 1, MaxAttachmentBytes: 1, MaxMessageLifetime: time.Hour},
		{MaxMailboxMessages: 1, MaxThreadMessages: 1, MaxThreadDepth: 1, MaxRecipients: 1, MaxAutoReplies: 1, MaxAttachmentBytes: 1, MaxMessageLifetime: 366 * 24 * time.Hour},
	}
	for index, config := range invalidConfigs {
		if err := config.Validate(); err == nil {
			t.Fatalf("invalid config %d accepted", index)
		}
	}
	for _, state := range []DeliveryState{
		StateQueued, StateDelivered, StateOpened, StateAcknowledged, StateReplied,
		StateResolved, StateExpired, StateRejected, StateCancelled, StateCorrected,
	} {
		if !state.Valid() {
			t.Fatalf("state %q rejected", state)
		}
	}
	if DeliveryState("other").Valid() || StateOpened.Terminal() ||
		!StateResolved.Terminal() {
		t.Fatal("delivery closed-set semantics changed")
	}
	if err := SignEnvelope(nil, "key", nil); err == nil {
		t.Fatal("nil signing input accepted")
	}
	envelope := contracts.MessageEnvelope{}
	if err := SignEnvelope(&envelope, "bad value",
		make(ed25519.PrivateKey, ed25519.PrivateKeySize)); err == nil {
		t.Fatal("invalid signing key id accepted")
	}
	if err := SignEnvelope(&envelope, "key:valid", []byte{1}); err == nil {
		t.Fatal("invalid private key accepted")
	}
	_, validPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignEnvelope(&envelope, "key:valid", validPrivate); err == nil {
		t.Fatal("invalid envelope signed")
	}
	if _, err := SigningBytes(envelope); err == nil {
		t.Fatal("invalid envelope signing bytes accepted")
	}
	if err := validateToken("test", "bad value"); err == nil {
		t.Fatal("invalid token accepted")
	}
	if err := validateToken("test", strings.Repeat("a", 129)); err == nil {
		t.Fatal("oversized token accepted")
	}
	if err := validateDelegation(contracts.MessageEnvelope{},
		DelegationBinding{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invalid delegation = %v", err)
	}
	if err := validateCorrection(contracts.MessageEnvelope{},
		CorrectionBinding{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invalid correction = %v", err)
	}
	correctionEnvelope := contracts.MessageEnvelope{
		To: []contracts.SeatAddress{{SeatID: "seat:recipient"}},
	}
	if err := validateCorrection(correctionEnvelope, CorrectionBinding{
		NoticeID:        "bad value",
		CorrectionID:    "correction:valid",
		AffectedRecord:  "record:valid",
		RecipientSeatID: "seat:recipient",
	}); err == nil {
		t.Fatal("invalid correction token accepted")
	}
	if allowedTransition(StateQueued, StateResolved) ||
		!allowedTransition(StateOpened, StateAcknowledged) {
		t.Fatal("transition table changed")
	}
	if boundedError(errors.New(strings.Repeat("x", 600))) != strings.Repeat("x", 512) ||
		boundedError(nil) != "" {
		t.Fatal("bounded error changed")
	}
}

type mailFixture struct {
	store          *Store
	graph          *dependency.Store
	tenantID       string
	organizationID contracts.OrganizationID
	now            *time.Time
	sender         mailSeat
	recipient      mailSeat
	other          mailSeat
}

type mailSeat struct {
	address contracts.SeatAddress
	keyID   string
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func newMailFixture(t *testing.T, label string, config Config) mailFixture {
	t.Helper()
	tenant := "tenant:" + label
	organizationID := contracts.OrganizationID("organization:" + label)
	now := mailBaseTime()
	graph, err := dependency.New(mailPool, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(mailPool, mailVault(t, tenant), graph, tenant, config,
		func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	fixture := mailFixture{
		store: store, graph: graph, tenantID: tenant,
		organizationID: organizationID, now: &now,
		sender:    newMailSeat(t, organizationID, "department:developer", "seat:sender"),
		recipient: newMailSeat(t, organizationID, "department:accounting", "seat:recipient"),
		other:     newMailSeat(t, organizationID, "department:legal", "seat:other"),
	}
	for _, seat := range []*mailSeat{&fixture.sender, &fixture.recipient, &fixture.other} {
		insertSeatAuthority(t, tenant, seat.address, now)
		if err := store.PublishSeatKey(context.Background(), SeatKey{
			Address: seat.address, KeyID: seat.keyID,
			PublicKey: seat.public, EffectiveAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func newMailSeat(
	t *testing.T,
	organizationID contracts.OrganizationID,
	departmentID contracts.DepartmentID,
	seatID contracts.SeatID,
) mailSeat {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return mailSeat{
		address: contracts.SeatAddress{
			OrganizationID: organizationID, DepartmentID: departmentID, SeatID: seatID,
		},
		keyID: "key:" + string(seatID), public: publicKey, private: privateKey,
	}
}

func (fixture mailFixture) envelope(
	t *testing.T,
	label string,
	kind contracts.MessageKind,
	to contracts.SeatAddress,
) contracts.MessageEnvelope {
	t.Helper()
	envelope := contracts.MessageEnvelope{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "message:" + contracts.MessageID(label),
		ThreadID:      "thread:" + contracts.ThreadID(label),
		From:          fixture.sender.address, To: []contracts.SeatAddress{to},
		Kind: kind, Subject: "Subject " + label,
		Payload: contracts.MessagePayloadRef{
			SchemaID: "workforce.mail." + string(kind) + ".v1",
			Artifact: mailArtifact("payload:"+label, 128),
		},
		ParentIntentID: "intent:" + contracts.IntentID(label),
		RequiredAction: "Review and respond with typed evidence",
		Priority:       5, TimeoutAction: contracts.TimeoutEscalate,
		Classification: contracts.ClassificationDepartment,
		IdempotencyKey: "send:" + label, CreatedAt: *fixture.now,
		ExpiresAt: fixture.now.Add(30 * time.Minute),
	}
	deadline := fixture.now.Add(20 * time.Minute)
	envelope.Deadline = &deadline
	if err := SignEnvelope(&envelope, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func (fixture mailFixture) reply(
	t *testing.T,
	parent contracts.MessageEnvelope,
	label string,
	kind contracts.MessageKind,
) contracts.MessageEnvelope {
	t.Helper()
	envelope := fixture.envelope(t, label, kind, fixture.recipient.address)
	envelope.ThreadID = parent.ThreadID
	envelope.InReplyTo = &parent.ID
	if err := SignEnvelope(&envelope, fixture.sender.keyID, fixture.sender.private); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func mailArtifact(label string, size uint64) contracts.ArtifactRef {
	return contracts.ArtifactRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            contracts.ArtifactID(strings.ReplaceAll(label, "/", ":")),
		Hash: contracts.ContentHash{
			Algorithm: "sha256", Digest: strings.Repeat("a", 64),
		},
		MediaType: "application/json", SizeBytes: size,
	}
}

func defaultMailConfig() Config {
	return Config{
		MaxMailboxMessages: 100, MaxThreadMessages: 100,
		MaxThreadDepth: 32, MaxRecipients: 32, MaxAutoReplies: 8,
		MaxAttachmentBytes: 64 << 20, MaxMessageLifetime: 30 * 24 * time.Hour,
	}
}

func insertSeatAuthority(
	t *testing.T,
	tenant string,
	address contracts.SeatAddress,
	now time.Time,
) {
	t.Helper()
	hash := strings.Repeat("b", 64)
	if _, err := mailPool.Exec(context.Background(), `
		INSERT INTO workforce_authority_records (
			tenant_id,organization_id,authority_kind,authority_id,version,
			owner_id,key_id,effective_at,canonical_hash,sealed_record,
			material_change,created_at
		) VALUES ($1,$2,'seat',$3,1,'owner','owner-key',$4,$5,$6,FALSE,$4)
		ON CONFLICT DO NOTHING
	`, tenant, address.OrganizationID, address.SeatID, now, hash, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := mailPool.Exec(context.Background(), `
		INSERT INTO workforce_authority_heads (
			tenant_id,organization_id,authority_kind,authority_id,
			latest_version,updated_at
		) VALUES ($1,$2,'seat',$3,1,$4)
		ON CONFLICT DO NOTHING
	`, tenant, address.OrganizationID, address.SeatID, now); err != nil {
		t.Fatal(err)
	}
}

func insertCorrectionFixture(t *testing.T, fixture mailFixture) {
	t.Helper()
	ctx := context.Background()
	for _, record := range []struct {
		id, kind string
	}{
		{"record:source", "finding"},
		{"record:affected", "finding"},
		{"record:correction", "correction"},
	} {
		if _, err := mailPool.Exec(ctx, `
			INSERT INTO workforce_records (
				tenant_id,organization_id,record_id,kind,author_seat_id,
				purpose,parent_intent_id,classification,validity,schema_version,
				content_hash_algorithm,content_hash_digest,canonical_hash,
				sealed_record,created_at,effective_at
			) VALUES ($1,$2,$3,$4,$5,'correction','intent:correction',
				'organization','active','workforce.v1','sha256',$6,$6,$7,$8,$8)
		`, fixture.tenantID, fixture.organizationID, record.id, record.kind,
			fixture.sender.address.SeatID, strings.Repeat("a", 64), []byte{1},
			*fixture.now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mailPool.Exec(ctx, `
		INSERT INTO workforce_corrections (
			tenant_id,organization_id,correction_id,correction_record_id,
			source_record_id,status,materially_unsafe,created_at
		) VALUES ($1,$2,'correction:mail','record:correction',
			'record:source','open',TRUE,$3)
	`, fixture.tenantID, fixture.organizationID, *fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := mailPool.Exec(ctx, `
		INSERT INTO workforce_correction_targets (
			tenant_id,organization_id,correction_id,affected_record_id,
			consumer_seat_id,state,materially_unsafe,paused
		) VALUES ($1,$2,'correction:mail','record:affected',$3,'pending',TRUE,TRUE)
	`, fixture.tenantID, fixture.organizationID,
		fixture.recipient.address.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, err := mailPool.Exec(ctx, `
		INSERT INTO workforce_correction_notices (
			tenant_id,organization_id,notice_id,correction_id,
			affected_record_id,recipient_seat_id,state,created_at
		) VALUES ($1,$2,'notice:correction','correction:mail',
			'record:affected',$4,'pending',$3)
	`, fixture.tenantID, fixture.organizationID, *fixture.now,
		fixture.recipient.address.SeatID); err != nil {
		t.Fatal(err)
	}
}

func mailVault(t *testing.T, tenant string) *vault.UserVault {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant, KEKHex: kek,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session.UserVault()
}

func mailBaseTime() time.Time {
	return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
}

func startMailPostgres(ctx context.Context) (string, string, error) {
	command := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=password", "-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", mailPostgresImage)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start postgres: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", containerID, "5432/tcp").CombinedOutput()
	if err != nil {
		return containerID, "", fmt.Errorf("resolve postgres port: %w: %s", err, portOutput)
	}
	address := strings.TrimSpace(string(portOutput))
	separator := strings.LastIndex(address, ":")
	if separator < 0 {
		return containerID, "", fmt.Errorf("invalid postgres port output %q", address)
	}
	return containerID, "postgres://postgres:password@127.0.0.1:" +
		address[separator+1:] + "/workforce?sslmode=disable", nil
}

func waitMailPostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool, nil
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
