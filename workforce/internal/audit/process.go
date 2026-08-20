package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"reflect"

	"centra/workforce/internal/auditorworker"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/processisolation"
)

const (
	MaxPacketBytes = auditorworker.MaxPacketBytes
	MaxOutputBytes = auditorworker.MaxOutputBytes
)

type Runner struct {
	Bubblewrap              string
	Binary                  string
	DeveloperAuthorityKeyID string
	DeveloperAuthorityKey   ed25519.PublicKey
}

type VerifiedDecision struct {
	decision       Decision
	packetDigest   contracts.ContentHash
	decisionDigest contracts.ContentHash
}

func (runner Runner) Run(
	ctx context.Context,
	packet contracts.VerdictPacket,
) (Decision, error) {
	verified, err := runner.RunVerified(ctx, packet)
	return verified.decision, err
}

func (runner Runner) RunVerified(
	ctx context.Context,
	packet contracts.VerdictPacket,
) (VerifiedDecision, error) {
	if err := packet.Validate(); err != nil {
		return VerifiedDecision{}, err
	}
	if packet.Developer != nil {
		if err := contracts.VerifyDeveloperAuditEvidence(
			*packet.Developer, runner.DeveloperAuthorityKeyID,
			runner.DeveloperAuthorityKey,
		); err != nil {
			return VerifiedDecision{}, fmt.Errorf("audit: developer authority: %w", err)
		}
	}
	input, err := contracts.EncodeCanonical(&packet)
	if err != nil || len(input) > MaxPacketBytes {
		return VerifiedDecision{}, fmt.Errorf("audit: packet exceeds input bound")
	}
	expected, err := Evaluate(packet)
	if err != nil {
		return VerifiedDecision{}, fmt.Errorf("audit: evaluate packet: %w", err)
	}
	environment := map[string]string{
		"WORKFORCE_SESSION": "1",
	}
	if packet.Developer != nil {
		environment["WORKFORCE_DEVELOPER_AUDIT_KEY_ID"] = runner.DeveloperAuthorityKeyID
		environment["WORKFORCE_DEVELOPER_AUDIT_PUBLIC_KEY"] =
			base64.RawURLEncoding.EncodeToString(runner.DeveloperAuthorityKey)
	}
	command, err := processisolation.Command(ctx, processisolation.Spec{
		Bubblewrap: runner.Bubblewrap, Binary: runner.Binary,
		ExpectedBuildDigest: packet.Runtime.AuditorBuildDigest.Digest,
		Target:              "/workforce-auditor", Env: environment,
	})
	if err != nil {
		return VerifiedDecision{}, fmt.Errorf("audit: isolated process command: %w", err)
	}
	command.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	command.Stdout = &boundedWriter{target: &stdout, remaining: MaxOutputBytes}
	command.Stderr = &boundedWriter{target: io.Discard, remaining: MaxOutputBytes}
	if err := command.Run(); err != nil {
		return VerifiedDecision{}, fmt.Errorf("audit: isolated process failed: %w", err)
	}
	decision, err := contracts.DecodeCanonical[Decision, *Decision](stdout.Bytes())
	if err != nil {
		return VerifiedDecision{}, err
	}
	if !reflect.DeepEqual(decision, expected) {
		return VerifiedDecision{}, fmt.Errorf("audit: process decision does not match kernel evaluation")
	}
	decisionBytes, err := contracts.EncodeCanonical(&decision)
	if err != nil {
		return VerifiedDecision{}, err
	}
	return VerifiedDecision{
		decision: decision, packetDigest: packetHash(packet),
		decisionDigest: hashBytes(decisionBytes),
	}, nil
}

type boundedWriter struct {
	target    io.Writer
	remaining int
}

func (writer *boundedWriter) Write(value []byte) (int, error) {
	if len(value) > writer.remaining {
		return 0, fmt.Errorf("audit: subprocess output bound exceeded")
	}
	n, err := writer.target.Write(value)
	writer.remaining -= n
	return n, err
}
