package actorstate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/processisolation"
	"matrix/workforce/internal/seatworker"
)

const (
	MaxPacketBytes = seatworker.MaxPacketBytes
	MaxOutputBytes = seatworker.MaxOutputBytes
)

type Runner struct {
	Bubblewrap string
	Binary     string
}

func (runner Runner) Run(
	ctx context.Context,
	packet contracts.WorkPacket,
) (SeatOutput, error) {
	return runner.run(ctx, packet, nil)
}

func (runner Runner) RunModel(
	ctx context.Context,
	packet contracts.WorkPacket,
	modelResponse []byte,
) (SeatOutput, error) {
	if len(modelResponse) == 0 {
		return SeatOutput{}, fmt.Errorf("actorstate: model response is required")
	}
	return runner.run(ctx, packet, modelResponse)
}

func (runner Runner) run(
	ctx context.Context,
	packet contracts.WorkPacket,
	modelResponse []byte,
) (SeatOutput, error) {
	if err := packet.Validate(); err != nil {
		return SeatOutput{}, err
	}
	session := seatworker.SessionInput{
		SchemaVersion: contracts.SchemaVersionV1,
		Packet:        packet, ModelResponse: append([]byte(nil), modelResponse...),
	}
	if err := session.Validate(); err != nil {
		return SeatOutput{}, err
	}
	input, err := contracts.EncodeCanonical(&session)
	if err != nil {
		return SeatOutput{}, err
	}
	expected, err := Orient(packet)
	if err != nil {
		return SeatOutput{}, fmt.Errorf("actorstate: orient packet: %w", err)
	}
	if len(modelResponse) > 0 {
		expected, err = seatworker.ApplyModel(packet, modelResponse)
		if err != nil {
			return SeatOutput{}, fmt.Errorf("actorstate: validate model response: %w", err)
		}
	}
	if len(input) > MaxPacketBytes {
		return SeatOutput{}, fmt.Errorf("actorstate: packet exceeds input bound")
	}
	command, err := processisolation.Command(ctx, processisolation.Spec{
		Bubblewrap: runner.Bubblewrap, Binary: runner.Binary, Target: "/workforce-seat",
		ExpectedBuildDigest: packet.Lease.Runtime.BuildDigest.Digest,
		Env: map[string]string{
			"WORKFORCE_SESSION": "1",
		},
	})
	if err != nil {
		return SeatOutput{}, fmt.Errorf("actorstate: isolated seat command: %w", err)
	}
	command.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	command.Stdout = &limitedWriter{writer: &stdout, remaining: MaxOutputBytes}
	command.Stderr = &limitedWriter{writer: io.Discard, remaining: MaxOutputBytes}
	if err := command.Run(); err != nil {
		return SeatOutput{}, fmt.Errorf("actorstate: isolated seat failed: %w", err)
	}
	output, err := contracts.DecodeCanonical[SeatOutput, *SeatOutput](stdout.Bytes())
	if err != nil {
		return SeatOutput{}, fmt.Errorf("actorstate: decode seat output: %w", err)
	}
	if !reflect.DeepEqual(output, expected) {
		return SeatOutput{}, fmt.Errorf("actorstate: seat output does not match kernel projection")
	}
	return output, nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (writer *limitedWriter) Write(value []byte) (int, error) {
	if len(value) > writer.remaining {
		return 0, fmt.Errorf("actorstate: subprocess output bound exceeded")
	}
	n, err := writer.writer.Write(value)
	writer.remaining -= n
	return n, err
}
