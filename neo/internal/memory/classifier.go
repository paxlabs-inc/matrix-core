// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
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

// ClassifyFinancial reports whether the opportunity summary contains a
// deterministic financial / on-chain signal. It is the reusable keyword + symbol
// re-check that backs EligibleForAutonomy; it never consults the extraction
// model's flag, so it is a pure, side-effect-free function of the summary text.
func ClassifyFinancial(summary string) bool {
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

// EligibleForAutonomy is the authoritative, fail-closed verdict on whether an
// opportunity may be run UNPROMPTED by an autonomous Automatrix wake. It is
// non-financial work only: it returns true ONLY when the extraction model did
// NOT flag the item financial AND the deterministic ClassifyFinancial re-check
// finds no financial signal in the summary. Any doubt from either source —
// model flag set, a financial keyword, a phrase, or a currency symbol — yields
// false (not eligible), so the system fails closed. A false result does not
// drop the opportunity: it is still captured and surfaced for explicit user
// approval, just never auto-run.
func EligibleForAutonomy(summary string, modelFinancial bool) bool {
	if modelFinancial {
		return false
	}
	return !ClassifyFinancial(summary)
}
