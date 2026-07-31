package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"matrix/workforce/internal/actorstate"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/seatworker"
)

func main() {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, actorstate.MaxPacketBytes+1))
	if err != nil {
		os.Exit(11)
	}
	session, err := contracts.DecodeCanonical[
		seatworker.SessionInput, *seatworker.SessionInput,
	](input)
	if err != nil {
		os.Exit(11)
	}
	packet := session.Packet
	if secret, found := strings.CutPrefix(packet.Goal.Title, "exit-after-leak:"); found {
		_, _ = fmt.Fprintln(os.Stderr, secret)
		os.Exit(12)
	}
	output, err := actorstate.Orient(packet)
	if err != nil {
		os.Exit(13)
	}
	if packet.Goal.Title == "forge-output" {
		output.InputCounts.Tools++
	}
	encoded, err := contracts.EncodeCanonical(&output)
	if err != nil {
		os.Exit(14)
	}
	if packet.Goal.Title == "duplicate-output" {
		encoded = append(
			[]byte(`{"schema_version":"workforce.v1",`),
			encoded[1:]...,
		)
	}
	_, _ = os.Stdout.Write(encoded)
}
