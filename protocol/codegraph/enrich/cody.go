package enrich

import (
	"context"
	"fmt"
	"strings"

	mcllm "centra/core/mcl/llm"
	"centra/core/mcl/mtx/interpreter"
)

// CodyConfig configures the production Summarizer that routes through the
// Centra AI gateway's cheap "cody" tier. GatewayURL and ActorDID are required for
// gateway routing; Model should be one of the cody-slot whitelist (the cheap
// glm-5p1-fast prototype model is the default cheap tier).
type CodyConfig struct {
	GatewayURL string // gateway host, no trailing slash
	ActorDID   string // X-Matrix-Actor-DID
	Model      string // cody-slot model id; default "grok-build-0.1"
	TokenEnv   string // env var holding the gateway bearer token; default MATRIX_GATEWAY_TOKEN
}

// codyModelDefault is the model in the gateway "cody" slot whitelist
// (gateway/internal/rates/rates.go): xAI's grok-build-0.1.
const codyModelDefault = "grok-build-0.1"

// summarySystemPrompt instructs the cheap tier to emit exactly one terse line
// describing a symbol's purpose — never its implementation, never source.
const summarySystemPrompt = "You summarize a code symbol in ONE terse line (<=120 chars) stating its purpose. " +
	"Output only the line: no code, no markdown, no quotes, no trailing period unless natural. " +
	"Describe what it is for, not how it is written."

// CodySummarizer is the production Summarizer: it batches per-node summary
// prompts through the gateway cody slot via the MCL LLM client. It satisfies
// Summarizer; deterministic tests use FakeSummarizer instead so no live gateway
// is required (Requirement 8.3).
type CodySummarizer struct {
	llm   interpreter.LLM
	model string
}

// NewCody builds a CodySummarizer that decodes through the gateway "cody" slot.
func NewCody(cfg CodyConfig) (*CodySummarizer, error) {
	model := cfg.Model
	if model == "" {
		model = codyModelDefault
	}
	c := &mcllm.Config{
		Model:       model,
		GatewayURL:  cfg.GatewayURL,
		ActorDID:    cfg.ActorDID,
		SlotLabel:   "cody",
		Temperature: 0, // deterministic summaries
	}
	if cfg.TokenEnv != "" {
		c.GatewayTokenEnv = cfg.TokenEnv
	}
	client, err := mcllm.New(c)
	if err != nil {
		return nil, fmt.Errorf("enrich: build cody client: %w", err)
	}
	return &CodySummarizer{llm: client, model: model}, nil
}

// Summarize sends one prompt per request through the cody slot, in order.
func (c *CodySummarizer) Summarize(ctx context.Context, reqs []Request) ([]string, error) {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		msgs := []interpreter.Message{
			{Role: "system", Content: summarySystemPrompt},
			{Role: "user", Content: codyUserPrompt(r)},
		}
		text, err := c.llm.Decode(ctx, msgs, "")
		if err != nil {
			return nil, fmt.Errorf("enrich: cody decode %s: %w", r.Id, err)
		}
		out[i] = oneLine(text)
	}
	return out, nil
}

// codyUserPrompt renders the structural facts of a symbol for the cheap tier.
// It passes only already-public graph fields (kind, name, signature, doc) —
// never raw source — so the enrichment boundary cannot leak source text.
func codyUserPrompt(r Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", r.Kind, r.QName)
	if r.Lang != "" {
		fmt.Fprintf(&b, " (%s)", r.Lang)
	}
	b.WriteByte('\n')
	if r.Sig != "" {
		fmt.Fprintf(&b, "signature: %s\n", oneLine(r.Sig))
	}
	if r.Doc != "" {
		fmt.Fprintf(&b, "doc: %s\n", oneLine(r.Doc))
	}
	return strings.TrimSpace(b.String())
}
