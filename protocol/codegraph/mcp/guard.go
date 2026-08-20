package mcp

import (
	"fmt"
	"strings"
)

// guardFragment enforces the fragments-not-source invariant at the tool
// boundary: every result must be a well-formed .kvx fragment of ids, locations,
// signatures, and one-line summaries — never a raw source range. Any line that
// falls outside the fragment grammar is treated as a possible source leak and
// fails closed, so it never reaches the agent.
//
// The grammar of a fragment line is exactly one of:
//   - empty
//   - a comment line beginning with "#"
//   - a NODE header line beginning with "NODE "
//   - the literal "EDGES"
//   - an indented "  key=value" field (including "  ^type=..." reverse edges)
//
// Source bodies (indented with tabs, or unindented statements/decls) match none
// of these, so a leaked range is rejected.
func guardFragment(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("empty fragment")
	}
	sawHeader := false
	for i, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# FRAGMENT"):
			sawHeader = true
		case strings.HasPrefix(line, "#"):
			// other comment lines are allowed
		case strings.HasPrefix(line, "NODE "):
			// node header
		case line == "EDGES":
			// edge block marker
		case strings.HasPrefix(line, "  ") && strings.Contains(line, "="):
			// indented key=value field (incl. caret reverse-edge lines)
		default:
			return fmt.Errorf("line %d is not a fragment line (possible raw source): %q", i+1, line)
		}
	}
	if !sawHeader {
		return fmt.Errorf("fragment missing # FRAGMENT header")
	}
	return nil
}
