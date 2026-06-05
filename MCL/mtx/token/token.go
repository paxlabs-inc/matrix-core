// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package token defines the token types produced by the MatrixScript lexer.
//
// Every production in grammar.bnf maps to one or more token types here.
// The lexer emits a flat stream of these tokens; the parser consumes them.
package token

import "fmt"

// Type is the enumerated type of a lexer token.
type Type int

const (
	// Special
	ILLEGAL Type = iota
	EOF
	NEWLINE // \n (significant — line-oriented grammar)

	// Literals
	IDENT      // [a-zA-Z][a-zA-Z0-9_-]*
	INT        // 123
	FLOAT      // 1.23
	STRING     // "..." (includes interpolation chars as raw content)
	URI        // matrix://...
	BOOL_TRUE  // true
	BOOL_FALSE // false

	// Section header
	SECTION // §NAME (the § + uppercase ident as one token)

	// Punctuation / operators
	EQUALS   // =
	COLON    // :
	DOT      // .
	COMMA    // ,
	LBRACKET // [
	RBRACKET // ]
	LBRACE   // { (interpolation open inside strings — emitted by parser, not lexer)
	RBRACE   // } (interpolation close inside strings — emitted by parser, not lexer)
	LPAREN   // (
	RPAREN   // )
	ARROW    // <-
	LT       // <
	LTEQ     // <=
	GT       // >
	GTEQ     // >=
	EQEQ     // ==
	PIPE     // |
	AT       // @
	HASH     // # (comment leader — the lexer skips comment bodies)
	ELLIPSIS // ... (in uri_wildcard_type)
	SLASH    // /

	// Indentation
	INDENT // exactly 2 leading spaces on a line (N1/N2 in grammar notes)

	// Keywords — blocks
	KW_ON      // on
	KW_END     // end
	KW_PROMPT  // prompt
	KW_RESOLVE // resolve
	KW_UNKNOWN // unknown
	KW_CLARIFY // clarify
	KW_SLOT    // slot
	KW_NONE    // none
	KW_ENUM    // enum (in enum<...>)

	// Keywords — modifiers
	KW_REQUIRED // required
	KW_OPTIONAL // optional

	// Keywords — condition prefixes
	KW_VERB       // verb (in on verb=...)
	KW_CONFIDENCE // confidence (in on confidence<...)

	// Keywords — roles (prompt block)
	KW_SYSTEM    // system
	KW_USER      // user
	KW_ASSISTANT // assistant

	// Keywords — severity
	KW_BLOCKING  // blocking
	KW_PREFERRED // preferred

	// Keywords — failure actions
	KW_FAIL  // fail
	KW_RETRY // retry
	KW_GATE  // gate

	// Keywords — suggest actions
	KW_RAISE_BUDGET     // raise_budget
	KW_EXTEND_DEADLINE  // extend_deadline
	KW_AMEND_CONSTRAINT // amend_constraint
	KW_DELEGATE         // delegate
	KW_ABANDON          // abandon

	// Keywords — failure reasons
	KW_UNKNOWN_INFORMATION // unknown_information
	KW_POLICY_VIOLATION    // policy_violation
	KW_OUT_OF_BUDGET       // out_of_budget
	KW_OUT_OF_SCOPE        // out_of_scope
	KW_AMBIGUOUS_REQUEST   // ambiguous_request
	KW_TOOL_FAILURE        // tool_failure
	KW_EXTERNAL_FAILURE    // external_failure
	KW_TIMEOUT             // timeout
	KW_CANCELLED_BY_USER   // cancelled_by_user
	KW_CORRECTION_INVALID  // correction_invalid

	// Keywords — cortex functions
	KW_CORTEX_FIND    // cortex.find
	KW_CORTEX_RESOLVE // cortex.resolve
	KW_CORTEX_CONTEXT // cortex.context

	// Keywords — D7 closed verbs (used in verb= conditions and space_list values)
	KW_FIND      // find
	KW_ACQUIRE   // acquire
	KW_BUILD     // build
	KW_MODIFY    // modify
	KW_DELIVER   // deliver
	KW_ANALYZE   // analyze
	KW_NEGOTIATE // negotiate
	KW_SCHEDULE  // schedule
	KW_MONITOR   // monitor

	// Keywords — other
	KW_ACTION  // action
	KW_SUGGEST // suggest
	KW_REASON  // reason

	// Keywords — determinism / seed_policy
	KW_SEEDABLE    // seedable
	KW_BEST_EFFORT // best_effort
	KW_PER_INTENT  // per_intent
	KW_PER_SESSION // per_session
	KW_PER_ACTOR   // per_actor
)

// keywords maps keyword strings to their token type.
// The lexer checks this map after scanning an IDENT to see if it's a keyword.
var keywords = map[string]Type{
	"on":       KW_ON,
	"end":      KW_END,
	"prompt":   KW_PROMPT,
	"resolve":  KW_RESOLVE,
	"unknown":  KW_UNKNOWN,
	"clarify":  KW_CLARIFY,
	"slot":     KW_SLOT,
	"none":     KW_NONE,
	"enum":     KW_ENUM,
	"required": KW_REQUIRED,
	"optional": KW_OPTIONAL,
	"true":     BOOL_TRUE,
	"false":    BOOL_FALSE,

	// Condition prefixes
	"verb":       KW_VERB,
	"confidence": KW_CONFIDENCE,

	// Prompt roles
	"system":    KW_SYSTEM,
	"user":      KW_USER,
	"assistant": KW_ASSISTANT,

	// Severity
	"blocking":  KW_BLOCKING,
	"preferred": KW_PREFERRED,

	// Failure actions
	"fail":  KW_FAIL,
	"retry": KW_RETRY,
	"gate":  KW_GATE,

	// Suggest actions
	"raise_budget":     KW_RAISE_BUDGET,
	"extend_deadline":  KW_EXTEND_DEADLINE,
	"amend_constraint": KW_AMEND_CONSTRAINT,
	"delegate":         KW_DELEGATE,
	"abandon":          KW_ABANDON,

	// Failure reasons
	"unknown_information": KW_UNKNOWN_INFORMATION,
	"policy_violation":    KW_POLICY_VIOLATION,
	"out_of_budget":       KW_OUT_OF_BUDGET,
	"out_of_scope":        KW_OUT_OF_SCOPE,
	"ambiguous_request":   KW_AMBIGUOUS_REQUEST,
	"tool_failure":        KW_TOOL_FAILURE,
	"external_failure":    KW_EXTERNAL_FAILURE,
	"timeout":             KW_TIMEOUT,
	"cancelled_by_user":   KW_CANCELLED_BY_USER,
	"correction_invalid":  KW_CORRECTION_INVALID,

	// Other
	"action":  KW_ACTION,
	"suggest": KW_SUGGEST,
	"reason":  KW_REASON,

	// Determinism / seed_policy
	"seedable":    KW_SEEDABLE,
	"best_effort": KW_BEST_EFFORT,
	"per_intent":  KW_PER_INTENT,
	"per_session": KW_PER_SESSION,
	"per_actor":   KW_PER_ACTOR,

	// D7 closed verbs
	"find":      KW_FIND,
	"acquire":   KW_ACQUIRE,
	"build":     KW_BUILD,
	"modify":    KW_MODIFY,
	"deliver":   KW_DELIVER,
	"analyze":   KW_ANALYZE,
	"negotiate": KW_NEGOTIATE,
	"schedule":  KW_SCHEDULE,
	"monitor":   KW_MONITOR,
}

// LookupIdent returns the keyword token type for ident if it is a keyword,
// or IDENT otherwise.
func LookupIdent(ident string) Type {
	if tok, ok := keywords[ident]; ok {
		return tok
	}
	return IDENT
}

// IsKeyword reports whether t is a keyword token type.
func (t Type) IsKeyword() bool {
	return t >= KW_ON && t <= KW_PER_ACTOR
}

// Pos records a source position: byte offset, line, and column (all 1-based).
type Pos struct {
	Offset int // byte offset from start of file (0-based)
	Line   int // 1-based line number
	Col    int // 1-based column (byte offset from start of line + 1)
}

func (p Pos) String() string {
	return fmt.Sprintf("%d:%d", p.Line, p.Col)
}

// Token is a single lexical token produced by the scanner.
type Token struct {
	Type    Type
	Literal string // raw text of the token
	Pos     Pos    // start position
}

func (t Token) String() string {
	return fmt.Sprintf("%s(%q)@%s", t.Type, t.Literal, t.Pos)
}

// String returns a human-readable name for the token type.
func (t Type) String() string {
	switch t {
	case ILLEGAL:
		return "ILLEGAL"
	case EOF:
		return "EOF"
	case NEWLINE:
		return "NEWLINE"
	case IDENT:
		return "IDENT"
	case INT:
		return "INT"
	case FLOAT:
		return "FLOAT"
	case STRING:
		return "STRING"
	case URI:
		return "URI"
	case BOOL_TRUE:
		return "TRUE"
	case BOOL_FALSE:
		return "FALSE"
	case SECTION:
		return "SECTION"
	case EQUALS:
		return "="
	case COLON:
		return ":"
	case DOT:
		return "."
	case COMMA:
		return ","
	case LBRACKET:
		return "["
	case RBRACKET:
		return "]"
	case LBRACE:
		return "{"
	case RBRACE:
		return "}"
	case LPAREN:
		return "("
	case RPAREN:
		return ")"
	case ARROW:
		return "<-"
	case LT:
		return "<"
	case LTEQ:
		return "<="
	case GT:
		return ">"
	case GTEQ:
		return ">="
	case EQEQ:
		return "=="
	case PIPE:
		return "|"
	case AT:
		return "@"
	case HASH:
		return "#"
	case ELLIPSIS:
		return "..."
	case SLASH:
		return "/"
	case INDENT:
		return "INDENT"
	default:
		if t.IsKeyword() {
			for k, v := range keywords {
				if v == t {
					return "KW(" + k + ")"
				}
			}
		}
		return fmt.Sprintf("Type(%d)", int(t))
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
