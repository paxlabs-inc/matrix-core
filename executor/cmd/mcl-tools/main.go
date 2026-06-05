// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// mcl-tools — inspect + smoke-test agent tool manifests.
//
// Subcommands:
//
//	mcl-tools list-servers   -manifest path           — show the server pool
//	mcl-tools list-tools     -manifest path           — list every tool URI
//	mcl-tools describe-tool  -manifest path -uri URI  — show one tool's metadata
//	mcl-tools verify         -manifest path           — parse + validate only
//	mcl-tools call           -manifest path -uri URI -args '{"k":"v"}'
//	                                                  — spawn the server, dispatch a call
//
// The `call` subcommand is the operator-side smoke tool: it boots the
// MCP servers declared in the manifest, executes one tools/call, prints
// the result, then drains. Useful for debugging "is this server
// actually responding?" without booting the full executor.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"matrix/executor/mcp"
	"matrix/executor/tool"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "list-servers":
		runListServers(os.Args[2:])
	case "list-tools":
		runListTools(os.Args[2:])
	case "describe-tool":
		runDescribeTool(os.Args[2:])
	case "verify":
		runVerify(os.Args[2:])
	case "call":
		runCall(os.Args[2:])
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "mcl-tools: unknown subcommand %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mcl-tools — Matrix agent tool manifest inspection and smoke testing

USAGE
  mcl-tools <subcommand> [flags]

SUBCOMMANDS
  list-servers     List MCP servers in the manifest
  list-tools       List every tool URI exposed by the manifest
  describe-tool    Show metadata for one tool URI
  verify           Parse + validate the manifest (no network)
  call             Spawn servers, invoke one tool, print result

EXAMPLES
  mcl-tools list-servers -manifest agents/default.json
  mcl-tools verify       -manifest agents/default.json
  mcl-tools call -manifest agents/default.json \
                 -uri matrix://tool/mcp/fs/list_directory@2024.11.1 \
                 -args '{"path":"/workspace"}'
`)
}

// ---------- list-servers ----------

func runListServers(argv []string) {
	fs := flag.NewFlagSet("list-servers", flag.ExitOnError)
	manifest := fs.String("manifest", "", "path to agent manifest JSON")
	mustParse(fs, argv)
	must(*manifest != "", "list-servers requires -manifest")

	m := mustLoad(*manifest)
	for _, s := range m.Servers {
		fmt.Printf("%s\t%s\t%s@%s\t%d tools\n", s.Alias, s.Transport, s.PackageDigest, s.Version, len(s.Tools))
	}
}

// ---------- list-tools ----------

func runListTools(argv []string) {
	fs := flag.NewFlagSet("list-tools", flag.ExitOnError)
	manifest := fs.String("manifest", "", "path to agent manifest JSON")
	jsonOut := fs.Bool("json", false, "emit JSON")
	mustParse(fs, argv)
	must(*manifest != "", "list-tools requires -manifest")

	m := mustLoad(*manifest)
	r, err := tool.NewRegistry(tool.RegistryParams{Manifest: m})
	must(err == nil, fmt.Sprintf("registry: %v", err))

	uris := r.List()
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(uris)
		return
	}
	for _, u := range uris {
		fmt.Println(u)
	}
}

// ---------- describe-tool ----------

func runDescribeTool(argv []string) {
	fs := flag.NewFlagSet("describe-tool", flag.ExitOnError)
	manifest := fs.String("manifest", "", "path to agent manifest JSON")
	uri := fs.String("uri", "", "tool URI to describe")
	mustParse(fs, argv)
	must(*manifest != "", "describe-tool requires -manifest")
	must(*uri != "", "describe-tool requires -uri")

	m := mustLoad(*manifest)
	r, err := tool.NewRegistry(tool.RegistryParams{Manifest: m})
	must(err == nil, fmt.Sprintf("registry: %v", err))

	t, err := r.Get(*uri)
	must(err == nil, fmt.Sprintf("get tool: %v", err))

	fmt.Printf("URI:              %s\n", t.URI())
	fmt.Printf("Description:      %s\n", t.Description())
	fmt.Printf("Side-effect:      %s\n", t.SideEffectClass())
	if mt, ok := t.(*tool.MCPTool); ok {
		fmt.Printf("Provider:         mcp (server alias %q)\n", mt.Server())
		fmt.Printf("Server-local:     %s\n", mt.Name())
	} else if nt, ok := t.(*tool.NativeTool); ok {
		fmt.Printf("Provider:         native (namespace %q)\n", nt.Namespace())
		fmt.Printf("Digest:           %s\n", nt.Digest())
	}
}

// ---------- verify ----------

func runVerify(argv []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	manifest := fs.String("manifest", "", "path to agent manifest JSON")
	mustParse(fs, argv)
	must(*manifest != "", "verify requires -manifest")

	m := mustLoad(*manifest)
	fmt.Printf("ok: agent=%s schema=%d servers=%d native=%d\n",
		m.Agent, m.SchemaVersion, len(m.Servers), len(m.NativeTools))
}

// ---------- call ----------

func runCall(argv []string) {
	fs := flag.NewFlagSet("call", flag.ExitOnError)
	manifest := fs.String("manifest", "", "path to agent manifest JSON")
	uri := fs.String("uri", "", "tool URI to invoke")
	argsJSON := fs.String("args", "{}", "JSON object of tool args")
	timeoutSec := fs.Int("timeout", 30, "spawn+call timeout (seconds)")
	mustParse(fs, argv)
	must(*manifest != "", "call requires -manifest")
	must(*uri != "", "call requires -uri")

	var args map[string]interface{}
	must(json.Unmarshal([]byte(*argsJSON), &args) == nil, "call -args must be valid JSON object")

	parsed, err := tool.ParseToolURI(*uri)
	must(err == nil, fmt.Sprintf("parse uri: %v", err))

	m := mustLoad(*manifest)

	// Build the Manager and Spawn the relevant server (only the one
	// that backs this tool — keep CLI invocations fast).
	mgr := mcp.NewManager(mcp.ManagerParams{StderrSink: os.Stderr})
	defer mgr.Close()

	if parsed.IsMCP() {
		spec, ok := findServerSpec(m, parsed.Server)
		must(ok, fmt.Sprintf("server alias %q not in manifest", parsed.Server))
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
		defer cancel()
		if _, err := mgr.Spawn(ctx, spec); err != nil {
			fmt.Fprintf(os.Stderr, "spawn %s: %v\n", parsed.Server, err)
			os.Exit(1)
		}
	}

	r, err := tool.NewRegistry(tool.RegistryParams{Manifest: m, MCP: mgr})
	must(err == nil, fmt.Sprintf("registry: %v", err))

	t, err := r.Get(*uri)
	must(err == nil, fmt.Sprintf("get tool: %v", err))

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	res, err := t.Call(ctx, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "call: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	if res.IsError {
		os.Exit(3)
	}
}

// ---------- helpers ----------

// findServerSpec resolves an alias in the manifest to the mcp.ServerSpec
// the manager wants. Resolves $env: refs in env+headers from os.LookupEnv.
func findServerSpec(m *tool.AgentManifest, alias string) (mcp.ServerSpec, bool) {
	for _, s := range m.Servers {
		if s.Alias != alias {
			continue
		}
		env, _, err := tool.ResolveEnvList(s.Env, os.LookupEnv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: %v\n", err)
		}
		hdr := make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			vv, ok := tool.ResolveEnv(v, os.LookupEnv)
			if !ok {
				fmt.Fprintf(os.Stderr, "warn: unresolved header env %s\n", v)
				continue
			}
			hdr[k] = vv
		}
		expected := make([]string, 0, len(s.Tools))
		for _, t := range s.Tools {
			expected = append(expected, t.Name)
		}
		// Inherit the executor process environment so tools that look at
		// PATH / HOME / etc continue to work.
		fullEnv := append(append([]string(nil), os.Environ()...), env...)
		return mcp.ServerSpec{
			Alias:         s.Alias,
			Transport:     s.Transport,
			Command:       s.Command,
			Args:          s.Args,
			Env:           fullEnv,
			Endpoint:      s.Endpoint,
			Headers:       hdr,
			PackageDigest: s.PackageDigest,
			ExpectedTools: expected,
		}, true
	}
	return mcp.ServerSpec{}, false
}

func mustLoad(path string) *tool.AgentManifest {
	m, err := tool.LoadAgentManifest(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return m
}

func mustParse(fs *flag.FlagSet, argv []string) {
	if err := fs.Parse(argv); err != nil {
		os.Exit(2)
	}
}

func must(cond bool, msg string) {
	if !cond {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(2)
	}
}

// silence unused-import warning when the build has no callers of strings.
var _ = strings.TrimSpace

// Copyright © 2026 Paxlabs Inc. All rights reserved.
