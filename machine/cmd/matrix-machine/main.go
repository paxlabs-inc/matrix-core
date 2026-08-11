// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	machineidentity "matrix/machine/identity"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("matrix-machine", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataRoot := flags.String("data-root", "", "durable machine data root")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || flags.Arg(0) != "ensure" || *dataRoot == "" {
		fmt.Fprintln(stderr, "usage: matrix-machine -data-root <path> ensure")
		return 2
	}
	descriptor, err := machineidentity.Ensure(ctx, machineidentity.RuntimeConfig(*dataRoot))
	if err != nil {
		fmt.Fprintf(stderr, "matrix-machine: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(descriptor); err != nil {
		fmt.Fprintf(stderr, "matrix-machine: encode descriptor: %v\n", err)
		return 1
	}
	return 0
}
