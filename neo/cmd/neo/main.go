// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Command neo runs the Neo default agent as an interactive CLI: a normal
// function-calling conversational loop with Neocortex-backed memory, the shared
// MCP tool surface, and core_execute delegation to the MCL pipeline.
//
// Usage:
//
//	neo [-config neo.kvx] [-manifest agents/default.json] [-prompt "do X"]
//
// Without -prompt it runs a REPL (one line per turn; /exit or EOF to quit).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"matrix/executor/tool"
	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/config"
	"matrix/neo/internal/delegate"
	neollm "matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/server"
	"matrix/neo/internal/tools"
	"matrix/neo/internal/writeback"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := tool.DisableProcessDump(); err != nil {
			fmt.Fprintf(os.Stderr, "neo: secure process boundary: %v\n", err)
			os.Exit(1)
		}
		runServe(os.Args[2:])
		return
	}
	runInteractive()
}

// runInteractive is the local CLI: a REPL, or a single -prompt turn.
func runInteractive() {
	var (
		configPath = flag.String("config", "", "path to a runtime neo.kvx config (optional)")
		manifest   = flag.String("manifest", "", "agent manifest with MCP servers (overrides config)")
		dataRoot   = flag.String("data-root", "", "Neo runtime data root (overrides config)")
		actor      = flag.String("actor", "", "Neocortex actor name (overrides config)")
		prompt     = flag.String("prompt", "", "run a single turn with this prompt, then exit")
		noTools    = flag.Bool("no-tools", false, "skip native and integration tools (chat-only)")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo: load config: %v\n", err)
		os.Exit(1)
	}
	if *manifest != "" {
		cfg.ManifestPath = *manifest
	}
	if *dataRoot != "" {
		cfg.DataRoot = *dataRoot
	}
	if *actor != "" {
		cfg.NeocortexActor = *actor
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	rep := &stdoutReporter{}
	in := bufio.NewReader(os.Stdin)

	// --- main + cheap models ---
	main, err := newClient(cfg.MainModel, 0.4, 8192, true, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo: cannot start the model %q: %v\n", cfg.MainModel, err)
		fmt.Fprintf(os.Stderr, "      set FIREWORKS_API_KEY (or MATRIX_GATEWAY_URL + MATRIX_GATEWAY_TOKEN) and retry.\n")
		os.Exit(1)
	}
	cheap, err := newClient(cfg.CheapModel, 0.2, 1024, false, cfg)
	if err != nil {
		cheap = nil // compaction falls back to the main model
	}

	// Neocortex is required: Neo has no alternate memory engine.
	pager, err := memory.Open(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo: Neocortex unavailable: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if pager != nil {
			_ = pager.Close()
		}
	}()

	// --- tools (best-effort) ---
	var tm *tools.Manager
	if !*noTools {
		workspaceDir := server.WorkspaceRoot(os.Getenv("NEO_WORKSPACE_DIR"))
		nativeGitPath := "/opt/matrix/coding-bin/git"
		if _, statErr := os.Stat(nativeGitPath); statErr != nil {
			nativeGitPath = "git"
		}
		tm, err = tools.Spawn(ctx, tools.Options{
			ManifestPath:    cfg.ManifestPath,
			StderrSink:      os.Stderr,
			NativeRoot:      workspaceDir,
			NativeReadRoots: []string{"/data/media", strings.TrimSpace(os.Getenv("MATRIX_MEDIA_DIR"))},
			NativeStateDir:  filepath.Join(filepath.Dir(cfg.DataRoot), "native-services"),
			NativeGitPath:   nativeGitPath,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "neo: tools unavailable (%v) — continuing chat-only\n", err)
			tm = nil
		}
	}
	defer func() {
		if tm != nil {
			_ = tm.Close()
		}
	}()

	// --- core_execute delegation to the MCL daemon ---
	if tm != nil {
		dele := delegate.New(delegate.Options{
			BaseURL:      cfg.DaemonURL,
			Token:        os.Getenv("NEO_DAEMON_TOKEN"),
			CallerDID:    cfg.ActorDID,
			CallerWallet: os.Getenv("NEO_CALLER_WALLET"),
			Approver:     newApprover(in, rep),
			Notify:       rep.Notice,
		})
		tm.SetDelegate(dele.Run)
	}

	// Explicit memory lookup tool (mirrors serve.go).
	if tm != nil && pager != nil {
		tm.SetRecall(pager.Recall)
		tm.SetMemoryMutation(pager.Mutate)
	}

	// --- background write-back consolidation (best-effort, needs memory) ---
	var cons agent.Consolidator
	if pager != nil {
		// Memory write-back: a stronger extractor (its quality sets memory
		// quality) + the cheap model for the rare relation-classify path.
		extract, eerr := newClient(cfg.ConsolidationModel, 0.2, 2048, false, cfg)
		if eerr != nil || extract == nil {
			extract = main // fall back to the main model if the extractor won't start
		}
		wc := writeback.New(extract, cheap, pager, cfg)
		wc.Start()
		defer wc.Stop()
		cons = wc
	}

	ag := agent.New(agent.Options{
		Config:       cfg,
		Main:         main,
		Cheap:        cheap,
		Tools:        tm,
		Pager:        pager,
		Reporter:     rep,
		Consolidator: cons,
	})

	printBanner(cfg, tm, pager)

	// --- one-shot ---
	if strings.TrimSpace(*prompt) != "" {
		if err := ag.Chat(ctx, *prompt); err != nil {
			fmt.Fprintf(os.Stderr, "neo: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// --- REPL ---
	fmt.Print("\nyou> ")
	for {
		line, ok := readLine(in)
		if !ok {
			break
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		if line == "" {
			fmt.Print("you> ")
			continue
		}
		if err := ag.Chat(ctx, line); err != nil {
			fmt.Fprintf(os.Stderr, "neo: %v\n", err)
		}
		if ctx.Err() != nil {
			break
		}
		fmt.Print("\nyou> ")
	}
}

func newClient(model string, temp float64, maxTok int, enableThinking bool, cfg config.Config) (*neollm.Client, error) {
	return newSlotClient(model, temp, maxTok, "neo", enableThinking, cfg)
}

// newOmniClient builds the stateless MiMo v2.5 omni vision grounder used only
// inside DOJO's desktop_look. The omni id ("mimo-v2.5") is not the Xiaomi text
// planner id ("mimo-v2.5-pro"), so the provider is pinned explicitly and the
// client talks DIRECTLY to api.xiaomimimo.com with the MiMo key — the same
// direct posture VOICE uses for ASR — rather than riding the gateway. Returns
// nil when no MiMo key is configured (desktop_look then stays unadvertised).
func newOmniClient(model string) (*neollm.Client, error) {
	key := strings.TrimSpace(os.Getenv("MIMO_API_KEY"))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("XIAOMI_API_KEY"))
	}
	if key == "" {
		return nil, fmt.Errorf("no MIMO_API_KEY/XIAOMI_API_KEY for omni grounding")
	}
	return neollm.New(mcllm.Config{
		Model:       model,
		Provider:    mcllm.ProviderXiaomi,
		ProviderSet: true,
		Endpoint:    envOrDefault("NEO_OMNI_URL", "https://api.xiaomimimo.com/v1/chat/completions"),
		APIKey:      key,
		Temperature: 0.1,
		MaxTokens:   2048,
	})
}

// newSlotClient builds a gateway-metered chat client tagged with the given
// slot label, so spend is attributed correctly (Neo's own "neo" slot, or
// Cassandra's dedicated "cassandra" slot for completeness audits). enableThinking
// opts the client into extended reasoning where the toggle matters (xAI Grok's
// reasoning_effort control; Baseten Kimi's chat_template_args opt-in) — set ONLY
// for the user-facing conversational loop (Neo).
// Cassandra, background sub-agents, and the token-tight cheap/consolidation
// roles all run with thinking OFF; the core MCL pipeline manages its own.
func newSlotClient(model string, temp float64, maxTok int, slot string, enableThinking bool, cfg config.Config) (*neollm.Client, error) {
	return neollm.New(mcllm.Config{
		Model:          model,
		Temperature:    temp,
		MaxTokens:      maxTok,
		GatewayURL:     cfg.GatewayURL,
		ActorDID:       cfg.ActorDID,
		SlotLabel:      slot,
		EnableThinking: enableThinking,
	})
}

// newApprover returns an Approver that prompts on the shared stdin reader.
// Safe because Chat (and thus any gate) runs synchronously while the REPL is
// blocked, so there is never a concurrent read of stdin.
func newApprover(in *bufio.Reader, rep *stdoutReporter) delegate.Approver {
	return func(ctx context.Context, nodeID, question string, options []string) (bool, string) {
		rep.Notice("approval needed — " + oneLine(question))
		if len(options) > 0 {
			fmt.Fprintf(os.Stderr, "    options: %s\n", strings.Join(options, " | "))
		}
		fmt.Fprint(os.Stderr, "    approve? [y/N] ")
		line, ok := readLine(in)
		if !ok {
			return false, ""
		}
		l := strings.ToLower(line)
		approved := l == "y" || l == "yes" || l == "approve" || l == "ok"
		return approved, ""
	}
}

func readLine(r *bufio.Reader) (string, bool) {
	line, err := r.ReadString('\n')
	if line == "" && err != nil {
		return "", false
	}
	return strings.TrimSpace(line), true
}

func printBanner(cfg config.Config, tm *tools.Manager, pager *memory.Pager) {
	fmt.Printf("%s — Matrix default agent\n", cfg.AgentName)
	fmt.Printf("  model: %s\n", cfg.MainModel)
	if pager != nil {
		mode := "salience"
		if pager.HasEmbedder() {
			mode = "semantic+salience"
		}
		fmt.Printf("  memory: %s (actor=%s, %s recall)\n", cfg.DataRoot, cfg.NeocortexActor, mode)
	} else {
		fmt.Printf("  memory: (none)\n")
	}
	if tm != nil {
		fmt.Printf("  tools: %d available", len(tm.NaturalToolNames()))
		if esc := tm.EscalateToolNames(); len(esc) > 0 {
			fmt.Printf(" (+%d behind core_execute)", len(esc))
		}
		fmt.Println()
		for _, w := range tm.Warnings() {
			fmt.Printf("  ! %s\n", w)
		}
	} else {
		fmt.Printf("  tools: (none)\n")
	}
}

// stdoutReporter renders the agent's output to a terminal: answers on stdout,
// ephemeral progress + notices on stderr so piping `-prompt` yields a clean
// answer.
type stdoutReporter struct{}

func (stdoutReporter) Say(text string, _ bool) { fmt.Printf("\nneo> %s\n", strings.TrimSpace(text)) }
func (stdoutReporter) Status(text string)      { fmt.Fprintf(os.Stderr, "  · %s\n", oneLine(text)) }

// Progress prints the ephemeral narrate-before-act intent stub to stderr (like
// Status): the terminal has no durable-thread notion, so ephemeral vs durable
// only matters for the SSE/UI surface.
func (stdoutReporter) Progress(text string) { fmt.Fprintf(os.Stderr, "  · %s\n", oneLine(text)) }
func (stdoutReporter) Notice(text string)   { fmt.Fprintf(os.Stderr, "  » %s\n", oneLine(text)) }
func (stdoutReporter) Think(text string)    { fmt.Fprintf(os.Stderr, "  ~ %s\n", oneLine(text)) }

// Delta is a no-op for the CLI: the terminal prints the complete answer at the
// end (Say); live token streaming is an SSE/UI affordance.
func (stdoutReporter) Delta(int, string, string) {}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
