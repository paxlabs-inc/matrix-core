package selfmodel

import (
	"fmt"
	"strings"

	"matrix/codegraph/model"
	"matrix/codegraph/retrieve"
)

var residentSymbols = []string{
	"Chat",
	"assembleWindowUserTail",
	"runSwarm",
	"SubagentSchemas",
	"AssertNoValueTransferTools",
	"coreExecuteSchema",
}

type Artifact struct {
	Summary string   `json:"summary"`
	Merkle  string   `json:"merkle"`
	Scope   []string `json:"scope"`
}

func Distill(ix *model.Index, merkle string, scope []string, tokenBudget int) Artifact {
	if tokenBudget <= 0 {
		tokenBudget = 800
	}
	api := retrieve.New(ix)
	parts := make([]string, 0, len(residentSymbols))
	for _, symbol := range residentSymbols {
		fragment := strings.TrimSpace(api.SymbolLookup(symbol, ""))
		if strings.Contains(fragment, "matches=0") {
			continue
		}
		parts = append(parts, fragment)
	}
	summary := "Your structural self is derived from codegraph. Your conversation loop, context-window assembly, coding and execution faculties, sub-agent decomposition, and value-transfer safety wall are represented by these compact graph fragments:\n" + strings.Join(parts, "\n")
	return Artifact{Summary: truncateTokens(summary, tokenBudget), Merkle: merkle, Scope: append([]string(nil), scope...)}
}

func Lookup(ix *model.Index, symbol string) (string, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "", fmt.Errorf("self-model symbol is required")
	}
	return retrieve.New(ix).SymbolLookup(symbol, ""), nil
}

func truncateTokens(text string, budget int) string {
	fields := strings.Fields(text)
	if len(fields) <= budget {
		return text
	}
	return strings.Join(fields[:budget], " ")
}
