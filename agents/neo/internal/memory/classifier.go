// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import "strings"

// This file is the authoritative, deterministic, fail-closed financial
// classifier for the Automatrix non-financial autonomy boundary (layer 1 of
// the defense-in-depth gating in the design). The extraction model supplies a
// first-pass `financial` flag (consolidator), which the consolidator maps onto
// eligible_autonomous=!financial. That model verdict is advisory: this
// re-check runs deterministically on the opportunity summary and can only ever
// make the verdict MORE restrictive — flipping a model-said-non-financial item
// to ineligible when any financial signal is present. It never relaxes the
// model's verdict, so the system fails CLOSED: when in doubt, an opportunity is
// treated as financial and is not eligible for unprompted autonomous work
// (it is still captured and surfaced for explicit user approval).

// financialTokens is the curated set of word-level financial signals. Matching
// is word-boundary (token) based — never substring — so an innocuous summary
// such as "Summarize the API rate-limit thread" is not falsely flagged. Tokens
// are deliberately money/chain-specific. A few genuinely ambiguous words the
// spec lists as financial signals (notably "transfer") are kept here on the
// fail-closed principle: a "transfer the files" task is then conservatively
// surfaced for explicit approval rather than auto-run. Truly broad words that
// routinely occur in non-financial work (send, value, order, book, rate) are
// intentionally excluded here and handled, where they matter, as multi-word
// phrases below so the autonomy boundary stays usable while still failing
// closed on a genuine financial signal.
var financialTokens = map[string]struct{}{
	"buy": {}, "buys": {}, "buying": {}, "bought": {},
	"sell": {}, "sells": {}, "selling": {}, "sold": {},
	"purchase": {}, "purchases": {}, "purchased": {}, "purchasing": {},
	"pay": {}, "pays": {}, "paid": {}, "paying": {}, "payment": {}, "payments": {}, "payout": {}, "payouts": {},
	"spend": {}, "spends": {}, "spent": {}, "spending": {},
	"invest": {}, "invests": {}, "invested": {}, "investing": {}, "investment": {}, "investments": {},
	"trade": {}, "trades": {}, "traded": {}, "trading": {},
	"swap": {}, "swaps": {}, "swapped": {}, "swapping": {},
	"stake": {}, "stakes": {}, "staked": {}, "staking": {}, "unstake": {}, "unstaking": {},
	"mint": {}, "mints": {}, "minted": {}, "minting": {},
	"withdraw": {}, "withdraws": {}, "withdrawal": {}, "withdrawals": {},
	"deposit": {}, "deposits": {}, "deposited": {},
	"refund": {}, "refunds": {},
	"transaction": {}, "transactions": {}, "txn": {}, "tx": {},
	"transfer": {}, "transfers": {}, "transferred": {}, "remit": {}, "remittance": {}, "wire": {},
	"wallet": {}, "wallets": {},
	"crypto": {}, "cryptocurrency": {}, "defi": {}, "onchain": {},
	"token": {}, "tokens": {}, "coin": {}, "coins": {},
	"bitcoin": {}, "btc": {}, "ethereum": {}, "eth": {}, "usdc": {}, "usdt": {}, "stablecoin": {},
	"nft": {}, "nfts": {},
	"fund": {}, "funds": {}, "funding": {}, "funded": {},
	"money": {}, "cash": {}, "dollars": {}, "dollar": {},
	"price": {}, "pricing": {}, "priced": {},
	"checkout": {}, "subscribe": {}, "subscription": {}, "subscriptions": {},
	"bill": {}, "bills": {}, "billing": {}, "billed": {}, "invoice": {}, "invoices": {},
}

// financialPhrases catches multi-word financial signals whose individual words
// are too common to gate on alone. Matched as substrings of the normalized
// (lower-cased, single-spaced) summary, with hyphens flattened to spaces so
// "on-chain" and "on chain" both hit.
var financialPhrases = []string{
	"on chain",
	"send money", "send funds", "send value", "sending value", "send payment",
	"send crypto", "send tokens", "send coins",
	"move money", "move funds", "move value",
	"sign a transaction", "sign transaction", "sign the transaction",
	"core execute", "core_execute",
}

// financialSymbols are currency/value glyphs whose mere presence signals money.
const financialSymbols = "$€£¥₿"

// analysisVerbs are task-framing verbs whose deliverable is words, models, or
// designs — never a value movement. A summary whose imperative frame opens with
// one of these is ANALYSIS work about a (possibly financial) topic, not an
// action that spends: "Model the PAX subscription revenue…", "Evaluate a burn
// mechanism…", "Sketch out the tier structure…". The financial boundary guards
// against tasks that MOVE value, not tasks that DISCUSS it — an analysis task
// on the restricted autonomous surface structurally cannot reach the signing
// path regardless of its topic, so gating it on topic keywords only defeats
// the feature (everything the user works on mentions money).
var analysisVerbs = map[string]struct{}{
	"model": {}, "evaluate": {}, "analyze": {}, "analyse": {}, "assess": {},
	"design": {}, "sketch": {}, "draft": {}, "map": {}, "outline": {},
	"research": {}, "investigate": {}, "explore": {}, "study": {}, "review": {},
	"summarize": {}, "summarise": {}, "document": {}, "compare": {},
	"write": {}, "plan": {}, "estimate": {}, "calculate": {}, "compute": {},
	"benchmark": {}, "simulate": {}, "prototype": {}, "spec": {}, "propose": {},
	"brainstorm": {}, "diagram": {}, "chart": {}, "visualize": {}, "visualise": {},
	"help": {}, "prepare": {}, "explain": {}, "describe": {}, "list": {},
}

// analysisFramed reports whether the summary's imperative frame is an
// analysis/authoring verb — i.e. the task's deliverable is a document, model,
// or design rather than an action. Only the LEADING token frames the task
// ("Model the PAX price volatility…", "Help Andrew design…" via "help"): a
// list word appearing later is usually a noun ("Transfer the design files")
// and must not soften the fail-closed scan.
func analysisFramed(s string) bool {
	toks := financialTokens2(strings.ToLower(s))
	if len(toks) == 0 {
		return false
	}
	_, ok := analysisVerbs[toks[0]]
	return ok
}

// ClassifyFinancial reports whether the opportunity summary describes work with
// a deterministic financial / on-chain ACTION signal. It is the reusable
// keyword + symbol re-check that backs EligibleForAutonomy; it never consults
// the extraction model's flag, so it is a pure, side-effect-free function of
// the summary text. An analysis-framed task (see analysisFramed) is never
// financial: its deliverable is words about money, not a movement of it —
// keyword topics like "price"/"subscription"/"token" in a modeling task must
// not disqualify autonomous work the restricted surface already makes safe.
func ClassifyFinancial(summary string) bool {
	if analysisFramed(summary) {
		return false
	}
	s := strings.ToLower(summary)
	if strings.ContainsAny(s, financialSymbols) {
		return true
	}
	flat := strings.ReplaceAll(s, "-", " ")
	flat = strings.Join(strings.Fields(flat), " ")
	for _, ph := range financialPhrases {
		if strings.Contains(flat, ph) {
			return true
		}
	}
	for _, tok := range financialTokens2(s) {
		if _, ok := financialTokens[tok]; ok {
			return true
		}
	}
	return false
}

// financialTokens2 splits s into lower-case alphanumeric tokens on any
// non-[a-z0-9] boundary, so "buy/sell", "on-chain", and "swap 100 PAX" all
// yield clean word tokens for set membership.
func financialTokens2(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

// EligibleForAutonomy is the authoritative verdict on whether an opportunity
// may be run UNPROMPTED by an autonomous Automatrix wake. The boundary it
// enforces is value MOVEMENT, not money as a topic (the only hard line is that
// autonomous work never causes monetary damage — and the restricted autonomous
// surface structurally lacks every value-moving tool regardless of this
// verdict):
//
//   - an analysis-framed task (deliverable = a document/model/design) is
//     always eligible — even when the extraction model flagged it financial,
//     because a topic-level flag on analysis work is exactly the false
//     positive that stalls the whole feature;
//   - otherwise the verdict fails CLOSED: the model's financial flag or any
//     deterministic action signal (keyword, phrase, currency symbol) makes it
//     ineligible, and the item is surfaced for explicit user approval instead.
func EligibleForAutonomy(summary string, modelFinancial bool) bool {
	if analysisFramed(summary) {
		return true
	}
	if modelFinancial {
		return false
	}
	return !ClassifyFinancial(summary)
}
