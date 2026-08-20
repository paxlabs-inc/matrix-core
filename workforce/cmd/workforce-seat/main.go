package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/seatworker"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

func run(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	getenv func(string) string,
) int {
	flags := flag.NewFlagSet("workforce-seat", flag.ContinueOnError)
	flags.SetOutput(stderr)
	versionOnly := flags.Bool("version", false, "print the workforce-seat build version")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: workforce-seat [-version]")
		return 2
	}
	if *versionOnly {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if getenv("WORKFORCE_SESSION") != "1" || !exactEnvironment(os.Environ()) {
		fmt.Fprintln(stderr, "workforce-seat: exact isolated session is required")
		return 3
	}
	input, err := io.ReadAll(io.LimitReader(stdin, seatworker.MaxPacketBytes+1))
	if err != nil || len(input) > seatworker.MaxPacketBytes {
		fmt.Fprintln(stderr, "workforce-seat: invalid WorkPacket")
		return 2
	}
	session, err := contracts.DecodeCanonical[
		seatworker.SessionInput, *seatworker.SessionInput,
	](input)
	if err != nil {
		fmt.Fprintln(stderr, "workforce-seat: invalid session input:", err)
		return 2
	}
	var output seatworker.SeatOutput
	if !session.HasModelResponse() {
		output, err = seatworker.Orient(session.Packet)
	} else {
		output, err = seatworker.ApplyModel(session.Packet, session.ModelResponse)
	}
	if err != nil {
		fmt.Fprintln(stderr, "workforce-seat: rejected WorkPacket:", err)
		return 3
	}
	encoded, err := contracts.EncodeCanonical(&output)
	if err != nil {
		fmt.Fprintln(stderr, "workforce-seat: encode output:", err)
		return 3
	}
	if _, err := stdout.Write(encoded); err != nil {
		fmt.Fprintln(stderr, "workforce-seat: write output:", err)
		return 3
	}
	return 0
}

func exactEnvironment(values []string) bool {
	if len(values) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		name, content, found := strings.Cut(value, "=")
		if !found || (name != "WORKFORCE_SESSION" && name != "PWD") {
			return false
		}
		if name == "PWD" && content != "/session" {
			return false
		}
		seen[name] = true
	}
	return seen["WORKFORCE_SESSION"] && seen["PWD"]
}
