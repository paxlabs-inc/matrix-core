package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"matrix/workforce/internal/audit"
	"matrix/workforce/internal/contracts"
)

func main() {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, audit.MaxPacketBytes+1))
	if err != nil {
		os.Exit(21)
	}
	packet, err := contracts.DecodeCanonical[contracts.VerdictPacket, *contracts.VerdictPacket](input)
	if err != nil {
		os.Exit(21)
	}
	mode := packet.Predicates[0].Description
	if secret, found := strings.CutPrefix(mode, "exit-after-leak:"); found {
		_, _ = fmt.Fprintln(os.Stderr, secret)
		os.Exit(22)
	}
	switch mode {
	case "private-state-probe":
		if _, err := os.Stat("/session/private-auditor-state"); err == nil {
			os.Exit(23)
		}
		if err := os.WriteFile(
			"/session/private-auditor-state", []byte("must-die-with-session"), 0o600,
		); err != nil {
			os.Exit(24)
		}
	}
	decision, err := audit.Evaluate(packet)
	if err != nil {
		os.Exit(25)
	}
	if mode == "forge-digest" {
		decision.ReasonCodes = []string{"invented:pass"}
	}
	encoded, err := contracts.EncodeCanonical(&decision)
	if err != nil {
		os.Exit(26)
	}
	if mode == "duplicate-output" {
		encoded = append(
			[]byte(`{"schema_version":"workforce.v1",`),
			encoded[1:]...,
		)
	}
	_, _ = os.Stdout.Write(encoded)
}
