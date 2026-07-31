package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"matrix/workforce/internal/auditorworker"
	"matrix/workforce/internal/contracts"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv, os.Environ))
}

func run(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	getenv func(string) string,
	environ func() []string,
) int {
	flags := flag.NewFlagSet("workforce-auditor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	versionOnly := flags.Bool("version", false, "print the workforce-auditor build version")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: workforce-auditor [-version]")
		return 2
	}
	if *versionOnly {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if getenv("WORKFORCE_SESSION") != "1" {
		fmt.Fprintln(stderr, "workforce-auditor: isolated session is required")
		return 3
	}
	input, err := io.ReadAll(io.LimitReader(stdin, auditorworker.MaxPacketBytes+1))
	if err != nil || len(input) > auditorworker.MaxPacketBytes {
		fmt.Fprintln(stderr, "workforce-auditor: invalid VerdictPacket")
		return 2
	}
	packet, err := contracts.DecodeCanonical[contracts.VerdictPacket, *contracts.VerdictPacket](input)
	if err != nil {
		fmt.Fprintln(stderr, "workforce-auditor: invalid VerdictPacket:", err)
		return 2
	}
	if !exactEnvironment(environ(), packet.Developer != nil) {
		fmt.Fprintln(stderr, "workforce-auditor: exact isolated environment is required")
		return 3
	}
	if packet.Developer != nil {
		publicKey, err := base64.RawURLEncoding.DecodeString(
			getenv("WORKFORCE_DEVELOPER_AUDIT_PUBLIC_KEY"),
		)
		if err != nil || len(publicKey) != ed25519.PublicKeySize ||
			contracts.VerifyDeveloperAuditEvidence(
				*packet.Developer,
				getenv("WORKFORCE_DEVELOPER_AUDIT_KEY_ID"),
				ed25519.PublicKey(publicKey),
			) != nil {
			fmt.Fprintln(stderr, "workforce-auditor: developer authority is invalid")
			return 3
		}
	}
	decision, err := auditorworker.Evaluate(packet)
	if err != nil {
		fmt.Fprintln(stderr, "workforce-auditor: rejected VerdictPacket:", err)
		return 3
	}
	encoded, err := contracts.EncodeCanonical(&decision)
	if err != nil {
		fmt.Fprintln(stderr, "workforce-auditor: encode decision:", err)
		return 3
	}
	if _, err := stdout.Write(encoded); err != nil {
		fmt.Fprintln(stderr, "workforce-auditor: write decision:", err)
		return 3
	}
	return 0
}

func exactEnvironment(values []string, developer bool) bool {
	expected := 2
	if developer {
		expected = 4
	}
	if len(values) != expected {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		name, content, found := strings.Cut(value, "=")
		allowed := name == "WORKFORCE_SESSION" || name == "PWD"
		if developer {
			allowed = allowed || name == "WORKFORCE_DEVELOPER_AUDIT_KEY_ID" ||
				name == "WORKFORCE_DEVELOPER_AUDIT_PUBLIC_KEY"
		}
		if !found || !allowed {
			return false
		}
		if name == "PWD" && content != "/session" {
			return false
		}
		seen[name] = true
	}
	if developer && (!seen["WORKFORCE_DEVELOPER_AUDIT_KEY_ID"] ||
		!seen["WORKFORCE_DEVELOPER_AUDIT_PUBLIC_KEY"]) {
		return false
	}
	return seen["WORKFORCE_SESSION"] && seen["PWD"]
}
