#!/usr/bin/env bash
# F1 — Durable live-run resume walkthrough.
#
# This script is the *recorded* runnable walkthrough referenced from the
# UX-FIXES-LEDGER F1 POST entry. It exercises the SAME server-side wire
# contract a browser flow depends on for the three resume scenarios in
# [item.F1].acceptance — (a) refresh, (b) hard-refresh, (c) wipe cookies +
# relogin — by reading GET /conversations/{id} and the SSE replay endpoint
# from independent HTTP client invocations (each `curl` is a fresh client
# with no shared headers / cookies, modelling a wipe-cookies-and-relogin).
#
# Because hermetic tests in this repo cannot drive a real LLM (no network),
# the equivalent end-to-end is encoded as the Go integration test
# `TestF1_ResumeWalkthrough_Integration` in conversations_live_test.go which
# spins up an httptest.NewServer + real Engine + real conversation.Store +
# real broker and exercises the same five steps. Run that test from this
# script as the authoritative walkthrough record:
#
#   bash agents/neo/internal/server/testdata/f1_resume_walkthrough.sh
#
# It prints the test's verbatim transcript (each step's measured live_run
# value and replayed SSE event types). The transcript is what gets pasted
# into the POST ledger entry.

set -euo pipefail
# Move to the neo module root (script lives at agents/neo/internal/server/testdata).
cd "$(dirname "$0")/../../.."

echo "==> F1 walkthrough — Go integration test (httptest + real engine)"
go test -count=1 -v ./internal/server/ -run TestF1_ResumeWalkthrough_Integration
