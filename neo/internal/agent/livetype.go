// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"matrix/neo/internal/llm"
)

// Live-typing channel (NEO-WORKBENCH task 1.2): while the model is still
// GENERATING a write_file tool call, the raw argument-JSON fragments stream in
// via llm.Delta.Tool. liveTyper incrementally decodes the "path" and "content"
// string values out of that partial JSON and emits bounded ToolStream events,
// so an open editor renders Neo typing the file in real time. The channel is
// strictly best-effort: it never touches the transcript, the dispatch, or the
// durable trace — the final ToolEnd step (and the file on disk) stay the
// authoritative complete state, so a dropped or capped stream degrades to the
// final content, never a corrupted buffer.

// liveTypeMaxBytes caps the decoded content streamed per call; past it the
// call stops emitting (the editor converges on the final state instead).
const liveTypeMaxBytes = 768 * 1024

// liveTyper tracks the in-flight tool calls of ONE model turn.
type liveTyper struct {
	emit  func(ToolEvent)
	calls map[int]*liveWriteScan
}

func newLiveTyper(emit func(ToolEvent)) *liveTyper {
	return &liveTyper{emit: emit, calls: map[int]*liveWriteScan{}}
}

// isLiveTypedTool reports whether a streamed call gets the typing channel:
// any alias of the fs full-content write (bare name write_file).
func isLiveTypedTool(name string) bool {
	bare := name
	if i := strings.LastIndex(name, "__"); i >= 0 {
		bare = name[i+2:]
	}
	return bare == "write_file"
}

// feed consumes one raw argument fragment and emits any newly decodable
// content.
func (lt *liveTyper) feed(tc *llm.ToolCallDelta) {
	if lt == nil || tc == nil || lt.emit == nil {
		return
	}
	sc := lt.calls[tc.Index]
	if sc == nil {
		sc = &liveWriteScan{}
		lt.calls[tc.Index] = sc
	}
	if sc.ignore {
		return
	}
	if tc.ID != "" {
		sc.id = tc.ID
	}
	if tc.Name != "" && sc.name == "" {
		sc.name = tc.Name
		if !isLiveTypedTool(sc.name) {
			sc.ignore = true
			sc.raw = nil
			return
		}
	}
	sc.raw = append(sc.raw, tc.Args...)
	sc.scan()
	if sc.emitted >= liveTypeMaxBytes {
		sc.ignore = true
		sc.raw = nil
		return
	}
	if sc.path == "" || len(sc.pending) == 0 {
		return
	}
	frag := string(sc.pending)
	sc.pending = sc.pending[:0]
	lt.emit(ToolEvent{
		ID:           sc.id,
		Name:         sc.name,
		Phase:        ToolStream,
		StreamPath:   sc.path,
		StreamDelta:  frag,
		StreamOffset: sc.emitted,
	})
	sc.emitted += len(frag)
}

// liveWriteScan is a resumable single-pass tokenizer over the growing raw
// argument JSON of one write_file call. It only understands what it needs: a
// top-level object whose "path" and "content" members are strings (any other
// member value — string, number, literal, nested object/array — is skipped
// with full string/escape awareness). Decoding pauses cleanly at end-of-input
// (including mid-escape) and resumes on the next fragment.
type liveWriteScan struct {
	id, name string
	ignore   bool

	raw []byte
	pos int // resume cursor into raw

	state    wsState
	depth    int  // nesting depth while skipping a non-string value
	inStr    bool // inside a string while skipping a nested value
	key      strings.Builder
	strVal   strings.Builder // accumulates a completed small string value (the key or path)
	capture  wsCapture       // what the current string value feeds
	path     string
	pending  []byte // decoded content bytes not yet emitted
	emitted  int    // decoded content bytes already emitted
	contDone bool   // content string closed
}

type wsState int

const (
	wsStart     wsState = iota // before '{'
	wsExpectKey                // inside object, before a key (or '}')
	wsInKey                    // inside the key string
	wsColon                    // between key and value
	wsValue                    // before a value
	wsInString                 // inside a string value being decoded
	wsSkipValue                // inside a non-string value being skipped
	wsAfter                    // after a value, before ',' or '}'
	wsDone                     // top-level object closed
)

type wsCapture int

const (
	capNone wsCapture = iota
	capKey
	capPath
	capContent
)

// scan advances over any newly appended bytes, pausing at end-of-input.
func (s *liveWriteScan) scan() {
	for s.pos < len(s.raw) {
		c := s.raw[s.pos]
		switch s.state {
		case wsStart:
			s.pos++
			if c == '{' {
				s.state = wsExpectKey
			}
		case wsExpectKey:
			s.pos++
			switch c {
			case '"':
				s.key.Reset()
				s.capture = capKey
				s.state = wsInKey
			case '}':
				s.state = wsDone
			}
		case wsInKey, wsInString:
			if !s.decodeString() {
				return // incomplete escape — wait for more bytes
			}
		case wsColon:
			s.pos++
			if c == ':' {
				s.state = wsValue
			}
		case wsValue:
			switch c {
			case ' ', '\t', '\n', '\r':
				s.pos++
			case '"':
				s.pos++
				switch s.key.String() {
				case "path":
					s.strVal.Reset()
					s.capture = capPath
				case "content":
					s.capture = capContent
				default:
					s.capture = capNone
				}
				s.state = wsInString
			case '{', '[':
				s.pos++
				s.depth = 1
				s.inStr = false
				s.state = wsSkipValue
			default:
				// number / true / false / null: skip to the delimiter.
				s.pos++
				s.depth = 0
				s.inStr = false
				s.state = wsSkipValue
			}
		case wsSkipValue:
			s.pos++
			if s.inStr {
				if c == '\\' {
					if s.pos >= len(s.raw) {
						return
					}
					s.pos++ // skip the escaped byte (\uXXXX hex needs no structure here)
				} else if c == '"' {
					s.inStr = false
				}
				continue
			}
			switch c {
			case '"':
				s.inStr = true
			case '{', '[':
				s.depth++
			case '}', ']':
				s.depth--
				if s.depth <= 0 {
					if s.depth < 0 {
						// closing the top-level object (bare-value skip ran into '}')
						s.state = wsDone
					} else {
						s.state = wsAfter
					}
				}
			case ',':
				if s.depth == 0 {
					s.state = wsExpectKey
				}
			}
		case wsAfter:
			s.pos++
			switch c {
			case ',':
				s.state = wsExpectKey
			case '}':
				s.state = wsDone
			}
		case wsDone:
			return
		}
	}
}

// decodeString consumes string bytes from s.pos until the closing quote or
// end-of-input, routing decoded characters to the active capture. Returns
// false when it stopped mid-escape (resume when more bytes arrive).
func (s *liveWriteScan) decodeString() bool {
	for s.pos < len(s.raw) {
		c := s.raw[s.pos]
		if c == '"' {
			s.pos++
			switch s.capture {
			case capKey:
				s.state = wsColon
			case capPath:
				s.path = s.strVal.String()
				s.state = wsAfter
			case capContent:
				s.contDone = true
				s.state = wsAfter
			default:
				s.state = wsAfter
			}
			if s.state == wsColon {
				return true
			}
			return true
		}
		if c != '\\' {
			// Plain bytes (UTF-8 passes through verbatim).
			s.put(s.raw[s.pos : s.pos+1])
			s.pos++
			continue
		}
		// Escape sequence: need at least 2 bytes, \uXXXX needs 6, and a UTF-16
		// surrogate pair needs the full 12 before it can be decoded.
		rest := s.raw[s.pos:]
		if len(rest) < 2 {
			return false
		}
		switch rest[1] {
		case '"', '\\', '/':
			s.put(rest[1:2])
			s.pos += 2
		case 'b':
			s.put([]byte{'\b'})
			s.pos += 2
		case 'f':
			s.put([]byte{'\f'})
			s.pos += 2
		case 'n':
			s.put([]byte{'\n'})
			s.pos += 2
		case 'r':
			s.put([]byte{'\r'})
			s.pos += 2
		case 't':
			s.put([]byte{'\t'})
			s.pos += 2
		case 'u':
			if len(rest) < 6 {
				return false
			}
			hi, err := strconv.ParseUint(string(rest[2:6]), 16, 32)
			if err != nil {
				// Malformed escape: pass it through raw rather than corrupting.
				s.put(rest[:6])
				s.pos += 6
				break
			}
			r := rune(hi)
			n := 6
			if utf16.IsSurrogate(r) {
				if len(rest) < 12 {
					return false
				}
				if rest[6] == '\\' && rest[7] == 'u' {
					if lo, err2 := strconv.ParseUint(string(rest[8:12]), 16, 32); err2 == nil {
						r = utf16.DecodeRune(r, rune(lo))
						n = 12
					}
				}
				if n == 6 {
					r = utf8.RuneError
				}
			}
			var buf [4]byte
			s.put(buf[:utf8.EncodeRune(buf[:], r)])
			s.pos += n
		default:
			// Unknown escape: pass through verbatim.
			s.put(rest[:2])
			s.pos += 2
		}
	}
	return true
}

// put routes decoded string bytes to the active capture.
func (s *liveWriteScan) put(b []byte) {
	switch s.capture {
	case capKey:
		s.key.Write(b)
	case capPath:
		s.strVal.Write(b)
	case capContent:
		s.pending = append(s.pending, b...)
	}
}
