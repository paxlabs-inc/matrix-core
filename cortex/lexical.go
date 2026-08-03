// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"matrix/cortex/keys"
	"matrix/cortex/store"
)

const LexicalSchemaVersion uint8 = 1

type lexicalPosting struct {
	SchemaVersion  uint8  `cbor:"0,keyasint"`
	ConversationID string `cbor:"1,keyasint"`
	Seq            uint64 `cbor:"2,keyasint"`
	TF             uint32 `cbor:"3,keyasint"`
	DocLen         uint32 `cbor:"4,keyasint"`
	TS             int64  `cbor:"5,keyasint"`
}

type lexicalDoc struct {
	SchemaVersion uint8  `cbor:"0,keyasint"`
	DocLen        uint32 `cbor:"1,keyasint"`
	TS            int64  `cbor:"2,keyasint"`
}

type LexicalHit struct {
	ConversationID string
	Seq            uint64
	Date           time.Time
	Score          float64
}

func lexicalTokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func lexicalTermHash(term string) [32]byte { return sha256.Sum256([]byte(term)) }

func lexicalDocKey(conv string, seq uint64) ([]byte, error) {
	k := append([]byte(nil), keys.PrefixLexicalDoc...)
	var err error
	k, err = keys.PutLPString(k, conv)
	if err != nil {
		return nil, err
	}
	return keys.PutUint64BE(k, seq), nil
}

func lexicalTermPrefix(term string) []byte {
	h := lexicalTermHash(term)
	return append(append([]byte(nil), keys.PrefixLexicalTerm...), h[:]...)
}

func lexicalPostingKey(term, conv string, seq uint64) ([]byte, error) {
	k := lexicalTermPrefix(term)
	var err error
	k, err = keys.PutLPString(k, conv)
	if err != nil {
		return nil, err
	}
	return keys.PutUint64BE(k, seq), nil
}

func lexicalRows(rec *SessionRecord) (map[string][]byte, error) {
	rows := map[string][]byte{}
	tokens := lexicalTokens(rec.Content)
	if len(tokens) == 0 {
		return rows, nil
	}
	docKey, err := lexicalDocKey(rec.ConversationID, rec.Seq)
	if err != nil {
		return nil, err
	}
	docVal, err := sessEnc.Marshal(lexicalDoc{SchemaVersion: LexicalSchemaVersion, DocLen: uint32(len(tokens)), TS: rec.TS})
	if err != nil {
		return nil, err
	}
	rows[string(docKey)] = docVal
	counts := map[string]uint32{}
	for _, token := range tokens {
		counts[token]++
	}
	terms := make([]string, 0, len(counts))
	for term := range counts {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	for _, term := range terms {
		key, kerr := lexicalPostingKey(term, rec.ConversationID, rec.Seq)
		if kerr != nil {
			return nil, kerr
		}
		val, verr := sessEnc.Marshal(lexicalPosting{SchemaVersion: LexicalSchemaVersion, ConversationID: rec.ConversationID, Seq: rec.Seq, TF: counts[term], DocLen: uint32(len(tokens)), TS: rec.TS})
		if verr != nil {
			return nil, verr
		}
		rows[string(key)] = val
	}
	return rows, nil
}

func (c *Cortex) stageLexicalMessage(wb *store.WriteBatch, rec *SessionRecord) error {
	rows, err := lexicalRows(rec)
	if err != nil {
		return err
	}
	keysSorted := make([]string, 0, len(rows))
	for k := range rows {
		keysSorted = append(keysSorted, k)
	}
	sort.Strings(keysSorted)
	for _, key := range keysSorted {
		if err := wb.Set([]byte(key), rows[key]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cortex) RebuildLexicalIndex() error {
	rows := map[string][]byte{}
	err := c.s.PrefixIter(keys.PrefixSession, func(_, value []byte) error {
		var rec SessionRecord
		if err := DecodeSessionRecord(value, &rec); err != nil {
			return err
		}
		add, err := lexicalRows(&rec)
		if err != nil {
			return err
		}
		for k, v := range add {
			rows[k] = v
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("cortex.RebuildLexicalIndex: %w", err)
	}
	return c.s.ReplaceDerivedPrefix(keys.PrefixLexical, rows)
}

func (c *Cortex) QueryLexical(queryText string, from, until time.Time, k int) ([]LexicalHit, error) {
	return c.queryLexical(queryText, "", from, until, k)
}

// QueryLexicalConversation searches only one conversation transcript. Ambient
// activation must use this scoped form; the global form is reserved for an
// explicit cross-thread recall request where every hit carries provenance.
func (c *Cortex) QueryLexicalConversation(
	queryText string,
	conversationID string,
	from, until time.Time,
	k int,
) ([]LexicalHit, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil, ErrEmptyConversationID
	}
	return c.queryLexical(queryText, conversationID, from, until, k)
}

func (c *Cortex) queryLexical(
	queryText string,
	conversationID string,
	from, until time.Time,
	k int,
) ([]LexicalHit, error) {
	terms := lexicalTokens(queryText)
	if len(terms) == 0 {
		return nil, nil
	}
	if k <= 0 {
		k = 8
	}
	var docs int
	var totalLen uint64
	if err := c.s.PrefixIter(keys.PrefixLexicalDoc, func(_, value []byte) error {
		var d lexicalDoc
		if err := sessDec.Unmarshal(value, &d); err != nil {
			return err
		}
		docs++
		totalLen += uint64(d.DocLen)
		return nil
	}); err != nil {
		return nil, nil
	}
	if docs == 0 {
		return nil, nil
	}
	avgLen := float64(totalLen) / float64(docs)
	type scored struct {
		hit   LexicalHit
		terms int
	}
	byDoc := map[string]*scored{}
	unique := map[string]struct{}{}
	for _, term := range terms {
		unique[term] = struct{}{}
	}
	for term := range unique {
		var postings []lexicalPosting
		if err := c.s.PrefixIter(lexicalTermPrefix(term), func(_, value []byte) error {
			var p lexicalPosting
			if err := sessDec.Unmarshal(value, &p); err != nil {
				return err
			}
			if conversationID != "" && p.ConversationID != conversationID {
				return nil
			}
			at := time.Unix(0, p.TS).UTC()
			if (!from.IsZero() && at.Before(from)) || (!until.IsZero() && !at.Before(until)) {
				return nil
			}
			postings = append(postings, p)
			return nil
		}); err != nil {
			return nil, nil
		}
		df := len(postings)
		if df == 0 {
			continue
		}
		idf := math.Log(1 + (float64(docs-df)+0.5)/(float64(df)+0.5))
		for _, p := range postings {
			key := fmt.Sprintf("%s\x00%d", p.ConversationID, p.Seq)
			s := byDoc[key]
			if s == nil {
				s = &scored{hit: LexicalHit{ConversationID: p.ConversationID, Seq: p.Seq, Date: time.Unix(0, p.TS).UTC()}}
				byDoc[key] = s
			}
			tf, dl := float64(p.TF), float64(p.DocLen)
			s.hit.Score += idf * (tf * 2.2) / (tf + 1.2*(0.25+0.75*dl/avgLen))
			s.terms++
		}
	}
	out := make([]LexicalHit, 0, len(byDoc))
	for _, s := range byDoc {
		s.hit.Score += float64(s.terms) / float64(len(unique))
		out = append(out, s.hit)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].ConversationID != out[j].ConversationID {
			return out[i].ConversationID < out[j].ConversationID
		}
		return out[i].Seq < out[j].Seq
	})
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}
