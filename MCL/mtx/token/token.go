// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package token defines the token types produced by the CentraScript lexer.
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
	IDENT     // [a-zA-Z][a-zA-Z0-9_-]*
	INT       // 123
	FLOAT     // 1.23
	STRING    // "..." (includes interpolation chars as raw content)
	URI       // matrix://...
	BoolTrue  // true
	BoolFalse // false

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
	KwOn      // on
	KwEnd     // end
	KwPrompt  // prompt
	KwResolve // resolve
	KwUnknown // unknown
	KwClarify // clarify
	KwSlot    // slot
	KwNone    // none
	KwEnum    // enum (in enum<...>)

	// Keywords — modifiers
	KwRequired // required
	KwOptional // optional

	// Keywords — condition prefixes
	KwVerb       // verb (in on verb=...)
	KwConfidence // confidence (in on confidence<...)

	// Keywords — roles (prompt block)
	KwSystem    // system
	KwUser      // user
	KwAssistant // assistant

	// Keywords — severity
	KwBlocking  // blocking
	KwPreferred // preferred

	// Keywords — failure actions
	KwFail  // fail
	KwRetry // retry
	KwGate  // gate

	// Keywords — suggest actions
	KwRaiseBudget     // raise_budget
	KwExtendDeadline  // extend_deadline
	KwAmendConstraint // amend_constraint
	KwDelegate        // delegate
	KwAbandon         // abandon

	// Keywords — failure reasons
	KwUnknownInformation // unknown_information
	KwPolicyViolation    // policy_violation
	KwOutOfBudget        // out_of_budget
	KwOutOfScope         // out_of_scope
	KwAmbiguousRequest   // ambiguous_request
	KwToolFailure        // tool_failure
	KwExternalFailure    // external_failure
	KwTimeout            // timeout
	KwCancelledByUser    // cancelled_by_user
	KwCorrectionInvalid  // correction_invalid

	// Keywords — cortex functions
	KwCortexFind    // cortex.find
	KwCortexResolve // cortex.resolve
	KwCortexContext // cortex.context

	// Keywords — D7 closed verbs (used in verb= conditions and space_list values)
	KwFind      // find
	KwAcquire   // acquire
	KwBuild     // build
	KwModify    // modify
	KwDeliver   // deliver
	KwAnalyze   // analyze
	KwNegotiate // negotiate
	KwSchedule  // schedule
	KwMonitor   // monitor

	// Keywords — other
	KwAction  // action
	KwSuggest // suggest
	KwReason  // reason

	// Keywords — determinism / seed_policy
	KwSeedable   // seedable
	KwBestEffort // best_effort
	KwPerIntent  // per_intent
	KwPerSession // per_session
	KwPerActor   // per_actor
)

// keywords maps keyword strings to their token type.
// The lexer checks this map after scanning an IDENT to see if it's a keyword.
var keywords = map[string]Type{
	"on":       KwOn,
	"end":      KwEnd,
	"prompt":   KwPrompt,
	"resolve":  KwResolve,
	"unknown":  KwUnknown,
	"clarify":  KwClarify,
	"slot":     KwSlot,
	"none":     KwNone,
	"enum":     KwEnum,
	"required": KwRequired,
	"optional": KwOptional,
	"true":     BoolTrue,
	"false":    BoolFalse,

	// Condition prefixes
	"verb":       KwVerb,
	"confidence": KwConfidence,

	// Prompt roles
	"system":    KwSystem,
	"user":      KwUser,
	"assistant": KwAssistant,

	// Severity
	"blocking":  KwBlocking,
	"preferred": KwPreferred,

	// Failure actions
	"fail":  KwFail,
	"retry": KwRetry,
	"gate":  KwGate,

	// Suggest actions
	"raise_budget":     KwRaiseBudget,
	"extend_deadline":  KwExtendDeadline,
	"amend_constraint": KwAmendConstraint,
	"delegate":         KwDelegate,
	"abandon":          KwAbandon,

	// Failure reasons
	"unknown_information": KwUnknownInformation,
	"policy_violation":    KwPolicyViolation,
	"out_of_budget":       KwOutOfBudget,
	"out_of_scope":        KwOutOfScope,
	"ambiguous_request":   KwAmbiguousRequest,
	"tool_failure":        KwToolFailure,
	"external_failure":    KwExternalFailure,
	"timeout":             KwTimeout,
	"cancelled_by_user":   KwCancelledByUser,
	"correction_invalid":  KwCorrectionInvalid,

	// Other
	"action":  KwAction,
	"suggest": KwSuggest,
	"reason":  KwReason,

	// Determinism / seed_policy
	"seedable":    KwSeedable,
	"best_effort": KwBestEffort,
	"per_intent":  KwPerIntent,
	"per_session": KwPerSession,
	"per_actor":   KwPerActor,

	// D7 closed verbs
	"find":      KwFind,
	"acquire":   KwAcquire,
	"build":     KwBuild,
	"modify":    KwModify,
	"deliver":   KwDeliver,
	"analyze":   KwAnalyze,
	"negotiate": KwNegotiate,
	"schedule":  KwSchedule,
	"monitor":   KwMonitor,
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
	return t >= KwOn && t <= KwPerActor
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
	case BoolTrue:
		return "TRUE"
	case BoolFalse:
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

// Copyright © 2026 Sidiora Labs. All rights reserved.
