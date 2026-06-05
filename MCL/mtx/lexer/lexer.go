// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package lexer tokenises MatrixScript (.mtx) source into a flat token stream.
//
// The lexer is line-oriented:
//   - It emits NEWLINE tokens.
//   - It emits INDENT tokens for leading 2-space indentation at line start.
//   - It skips comment lines (# ...) entirely but emits a NEWLINE at the end.
//   - It normalises CRLF to LF on read.
//
// The lexer handles string literals (with escape sequences), URI literals
// (matrix://...), section headers (§NAME), and all keywords/identifiers
// defined in the grammar.
package lexer

import (
	"unicode"
	"unicode/utf8"

	"matrix/mcl/mtx/token"
)

// Lexer scans MatrixScript source bytes and produces tokens.
type Lexer struct {
	src []byte // normalised source (LF only)

	pos     int // current byte offset into src
	readPos int // next byte to read (pos+width of current rune)
	ch      rune

	line    int // 1-based line number
	col     int // 1-based column (byte offset from line start + 1)
	lineOff int // byte offset of the start of the current line

	atLineStart bool // true when we're at column 1 of a new line
}

// New creates a Lexer over src. src is normalised (CRLF→LF) internally.
func New(src []byte) *Lexer {
	norm := normaliseCRLF(src)
	l := &Lexer{
		src:         norm,
		line:        1,
		col:         1,
		lineOff:     0,
		atLineStart: true,
	}
	l.readRune()
	return l
}

// NextToken returns the next token from the input.
func (l *Lexer) NextToken() token.Token {
	// At the start of a line, check for indent or section header.
	if l.atLineStart {
		return l.scanLineStart()
	}

	l.skipInlineWhitespace()

	pos := l.currentPos()

	switch {
	case l.ch == 0:
		return token.Token{Type: token.EOF, Literal: "", Pos: pos}

	case l.ch == '\n':
		l.readRune()
		l.newLine()
		return token.Token{Type: token.NEWLINE, Literal: "\n", Pos: pos}

	case l.ch == '#':
		return l.scanComment(pos)

	case l.ch == '"':
		return l.scanString(pos)

	case l.ch == '=':
		if l.peekByte() == '=' {
			l.readRune()
			l.readRune()
			return token.Token{Type: token.EQEQ, Literal: "==", Pos: pos}
		}
		l.readRune()
		return token.Token{Type: token.EQUALS, Literal: "=", Pos: pos}

	case l.ch == ':':
		l.readRune()
		return token.Token{Type: token.COLON, Literal: ":", Pos: pos}

	case l.ch == '.':
		if l.peekByte() == '.' && l.peekByteAt(1) == '.' {
			l.readRune()
			l.readRune()
			l.readRune()
			return token.Token{Type: token.ELLIPSIS, Literal: "...", Pos: pos}
		}
		l.readRune()
		return token.Token{Type: token.DOT, Literal: ".", Pos: pos}

	case l.ch == ',':
		l.readRune()
		return token.Token{Type: token.COMMA, Literal: ",", Pos: pos}

	case l.ch == '[':
		l.readRune()
		return token.Token{Type: token.LBRACKET, Literal: "[", Pos: pos}

	case l.ch == ']':
		l.readRune()
		return token.Token{Type: token.RBRACKET, Literal: "]", Pos: pos}

	case l.ch == '{':
		l.readRune()
		return token.Token{Type: token.LBRACE, Literal: "{", Pos: pos}

	case l.ch == '}':
		l.readRune()
		return token.Token{Type: token.RBRACE, Literal: "}", Pos: pos}

	case l.ch == '(':
		l.readRune()
		return token.Token{Type: token.LPAREN, Literal: "(", Pos: pos}

	case l.ch == ')':
		l.readRune()
		return token.Token{Type: token.RPAREN, Literal: ")", Pos: pos}

	case l.ch == '|':
		l.readRune()
		return token.Token{Type: token.PIPE, Literal: "|", Pos: pos}

	case l.ch == '@':
		l.readRune()
		return token.Token{Type: token.AT, Literal: "@", Pos: pos}

	case l.ch == '/':
		l.readRune()
		return token.Token{Type: token.SLASH, Literal: "/", Pos: pos}

	case l.ch == '<':
		if l.peekByte() == '-' {
			l.readRune()
			l.readRune()
			return token.Token{Type: token.ARROW, Literal: "<-", Pos: pos}
		}
		if l.peekByte() == '=' {
			l.readRune()
			l.readRune()
			return token.Token{Type: token.LTEQ, Literal: "<=", Pos: pos}
		}
		l.readRune()
		return token.Token{Type: token.LT, Literal: "<", Pos: pos}

	case l.ch == '>':
		if l.peekByte() == '=' {
			l.readRune()
			l.readRune()
			return token.Token{Type: token.GTEQ, Literal: ">=", Pos: pos}
		}
		l.readRune()
		return token.Token{Type: token.GT, Literal: ">", Pos: pos}

	case isDigit(l.ch):
		return l.scanNumber(pos)

	case isIdentStart(l.ch):
		return l.scanIdentOrKeyword(pos)

	default:
		ch := l.ch
		l.readRune()
		return token.Token{Type: token.ILLEGAL, Literal: string(ch), Pos: pos}
	}
}

// AllTokens drains the lexer and returns all tokens including EOF.
func (l *Lexer) AllTokens() []token.Token {
	var tokens []token.Token
	for {
		tok := l.NextToken()
		tokens = append(tokens, tok)
		if tok.Type == token.EOF {
			break
		}
	}
	return tokens
}

// ---- line-start handling ----

func (l *Lexer) scanLineStart() token.Token {
	l.atLineStart = false

	pos := l.currentPos()

	// Blank line
	if l.ch == '\n' {
		l.readRune()
		l.newLine()
		return token.Token{Type: token.NEWLINE, Literal: "\n", Pos: pos}
	}

	// EOF at line start
	if l.ch == 0 {
		return token.Token{Type: token.EOF, Literal: "", Pos: pos}
	}

	// Section header: § at column 1
	if l.ch == 0xA7 { // §
		return l.scanSection(pos)
	}
	// § is U+00A7 encoded as 0xC2 0xA7 in UTF-8; utf8.DecodeRune already handles this
	// so l.ch will be 0xA7 (167). But let's also check via the rune directly.

	// Comment at line start
	if l.ch == '#' {
		return l.scanComment(pos)
	}

	// 2-space indent detection
	if l.ch == ' ' && l.peekByte() == ' ' {
		l.readRune() // consume first space
		l.readRune() // consume second space
		return token.Token{Type: token.INDENT, Literal: "  ", Pos: pos}
	}

	// Otherwise, fall through to normal token scanning
	return l.NextToken()
}

// ---- section header ----

func (l *Lexer) scanSection(pos token.Pos) token.Token {
	start := l.pos
	l.readRune() // skip §

	// Read uppercase identifier
	for isUpperLetter(l.ch) || isDigit(l.ch) || l.ch == '_' {
		l.readRune()
	}

	lit := string(l.src[start:l.pos])
	return token.Token{Type: token.SECTION, Literal: lit, Pos: pos}
}

// ---- comments ----

func (l *Lexer) scanComment(pos token.Pos) token.Token {
	// Skip everything until newline or EOF.
	for l.ch != '\n' && l.ch != 0 {
		l.readRune()
	}
	// Don't consume the newline — let the next call emit NEWLINE.
	// But we need to report we're at a comment. Since grammar says comments
	// are ignored by the runtime, we skip and recurse.
	// However, the parser needs NEWLINEs for structure, so we don't eat the \n.
	return l.NextToken()
}

// ---- string literal ----

func (l *Lexer) scanString(pos token.Pos) token.Token {
	l.readRune() // skip opening "

	var buf []byte
	for {
		switch {
		case l.ch == 0 || l.ch == '\n':
			// Unterminated string
			return token.Token{Type: token.ILLEGAL, Literal: string(buf), Pos: pos}

		case l.ch == '\\':
			l.readRune()
			switch l.ch {
			case '"':
				buf = append(buf, '"')
			case '\\':
				buf = append(buf, '\\')
			case 'n':
				buf = append(buf, '\n')
			case 't':
				buf = append(buf, '\t')
			default:
				// Unknown escape — keep as-is
				buf = append(buf, '\\')
				buf = appendRune(buf, l.ch)
			}
			l.readRune()

		case l.ch == '"':
			l.readRune() // skip closing "
			return token.Token{Type: token.STRING, Literal: string(buf), Pos: pos}

		default:
			buf = appendRune(buf, l.ch)
			l.readRune()
		}
	}
}

// ---- number ----

func (l *Lexer) scanNumber(pos token.Pos) token.Token {
	start := l.pos
	for isDigit(l.ch) {
		l.readRune()
	}

	if l.ch == '.' && isDigit(rune(l.peekByte())) {
		l.readRune() // skip .
		for isDigit(l.ch) {
			l.readRune()
		}
		return token.Token{Type: token.FLOAT, Literal: string(l.src[start:l.pos]), Pos: pos}
	}

	return token.Token{Type: token.INT, Literal: string(l.src[start:l.pos]), Pos: pos}
}

// ---- identifier / keyword ----

func (l *Lexer) scanIdentOrKeyword(pos token.Pos) token.Token {
	start := l.pos
	for isIdentContinue(l.ch) {
		l.readRune()
	}

	lit := string(l.src[start:l.pos])

	// Check for compound cortex keywords: cortex.find, cortex.resolve, cortex.context
	if lit == "cortex" && l.ch == '.' {
		dotPos := l.pos
		l.readRune() // skip .
		fnStart := l.pos
		for isIdentContinue(l.ch) {
			l.readRune()
		}
		fnName := string(l.src[fnStart:l.pos])
		compound := "cortex." + fnName
		switch compound {
		case "cortex.find":
			return token.Token{Type: token.KW_CORTEX_FIND, Literal: compound, Pos: pos}
		case "cortex.resolve":
			return token.Token{Type: token.KW_CORTEX_RESOLVE, Literal: compound, Pos: pos}
		case "cortex.context":
			return token.Token{Type: token.KW_CORTEX_CONTEXT, Literal: compound, Pos: pos}
		default:
			// Not a known cortex function — backtrack.
			// Emit "cortex" as IDENT, then let the DOT + next ident be scanned separately.
			l.pos = dotPos
			l.readPos = dotPos
			l.readRune()
			// Recompute col
			l.col = dotPos - l.lineOff + 1
			return token.Token{Type: token.IDENT, Literal: "cortex", Pos: pos}
		}
	}

	// Check for matrix:// URI prefix
	if lit == "matrix" && l.ch == ':' && l.peekByte() == '/' && l.peekByteAt(1) == '/' {
		return l.scanURI(pos, start)
	}

	// Keyword or plain ident
	ty := token.LookupIdent(lit)
	return token.Token{Type: ty, Literal: lit, Pos: pos}
}

// scanURI scans a matrix:// URI starting from the 'm' of 'matrix'.
// The 'matrix' part has already been consumed; we're at ':'.
func (l *Lexer) scanURI(pos token.Pos, start int) token.Token {
	// Consume ://
	l.readRune() // :
	l.readRune() // /
	l.readRune() // /

	// Read until whitespace, newline, EOF, or closing delimiter (], ), >)
	for l.ch != 0 && l.ch != '\n' && l.ch != ' ' && l.ch != '\t' &&
		l.ch != ']' && l.ch != ')' && l.ch != '>' && l.ch != ',' {
		l.readRune()
	}

	return token.Token{Type: token.URI, Literal: string(l.src[start:l.pos]), Pos: pos}
}

// ---- rune reading ----

func (l *Lexer) readRune() {
	if l.readPos >= len(l.src) {
		l.ch = 0
		l.pos = len(l.src)
		return
	}

	r, w := utf8.DecodeRune(l.src[l.readPos:])
	l.ch = r
	l.pos = l.readPos
	l.readPos += w
	l.col = l.pos - l.lineOff + 1
}

func (l *Lexer) peekByte() byte {
	if l.readPos >= len(l.src) {
		return 0
	}
	return l.src[l.readPos]
}

func (l *Lexer) peekByteAt(offset int) byte {
	idx := l.readPos + offset
	if idx >= len(l.src) {
		return 0
	}
	return l.src[idx]
}

func (l *Lexer) currentPos() token.Pos {
	return token.Pos{
		Offset: l.pos,
		Line:   l.line,
		Col:    l.col,
	}
}

func (l *Lexer) newLine() {
	l.line++
	l.lineOff = l.pos
	l.col = 1
	l.atLineStart = true
}

func (l *Lexer) skipInlineWhitespace() {
	for l.ch == ' ' || l.ch == '\t' {
		l.readRune()
	}
}

// ---- character classification ----

func isIdentStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isIdentContinue(r rune) bool {
	return isIdentStart(r) || isDigit(r) || r == '_' || r == '-'
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

func isUpperLetter(r rune) bool {
	return r >= 'A' && r <= 'Z'
}

// ---- helpers ----

func appendRune(buf []byte, r rune) []byte {
	if r < utf8.RuneSelf {
		return append(buf, byte(r))
	}
	var tmp [utf8.UTFMax]byte
	n := utf8.EncodeRune(tmp[:], r)
	return append(buf, tmp[:n]...)
}

func normaliseCRLF(src []byte) []byte {
	// Fast path: no CR in input
	hasCR := false
	for _, b := range src {
		if b == '\r' {
			hasCR = true
			break
		}
	}
	if !hasCR {
		return src
	}

	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		if src[i] == '\r' {
			if i+1 < len(src) && src[i+1] == '\n' {
				continue // skip CR, the LF will be appended next iteration
			}
			out = append(out, '\n') // bare CR → LF
		} else {
			out = append(out, src[i])
		}
	}
	return out
}

// NFC normalisation placeholder — spec §3.1 says "UTF-8 NFC before parse".
// For v1 we rely on input already being NFC (standard for all modern editors).
// Full NFC normalisation would add a golang.org/x/text dependency.
// TODO: add golang.org/x/text/unicode/norm when the dep is justified.
var _ = unicode.Version // reference unicode to prevent "unused import"

// Copyright © 2026 Paxlabs Inc. All rights reserved.
