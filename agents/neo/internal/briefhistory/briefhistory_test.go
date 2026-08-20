// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package briefhistory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveryAndFeedbackRoundTrip(t *testing.T) {
	s := Open(t.TempDir())

	if err := s.AppendDelivery("2026-07-15", "brief-conv",
		[]string{"the album Cowboy Carter", "the album Cowboy Carter", ""},
		[]string{"https://example.com/a", "https://example.com/b"}); err != nil {
		t.Fatalf("AppendDelivery: %v", err)
	}
	if err := s.AppendFeedback("the album Cowboy Carter", VerdictNotInterested); err != nil {
		t.Fatalf("AppendFeedback: %v", err)
	}

	recs := s.Recent(0)
	if len(recs) != 2 {
		t.Fatalf("Recent = %d records, want 2", len(recs))
	}
	// Newest first: the feedback.
	if recs[0].Kind != KindFeedback || recs[0].Item != "the album Cowboy Carter" || recs[0].Verdict != VerdictNotInterested {
		t.Errorf("feedback record = %+v", recs[0])
	}
	if recs[1].Kind != KindDelivery || recs[1].Date != "2026-07-15" || recs[1].ConversationID != "brief-conv" {
		t.Errorf("delivery record = %+v", recs[1])
	}
	// Dedup + empty-drop on the rec list.
	if len(recs[1].RecIDs) != 1 || len(recs[1].NewsURLs) != 2 {
		t.Errorf("delivery lists = recs %v urls %v", recs[1].RecIDs, recs[1].NewsURLs)
	}
}

func TestRecentDeliveredNoRepeatSet(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.AppendDelivery("2026-07-14", "c", []string{"rec-a"}, []string{"https://x.test/1"}); err != nil {
		t.Fatalf("AppendDelivery: %v", err)
	}
	if err := s.AppendDelivery("2026-07-15", "c", []string{"rec-b", "rec-a"}, []string{"https://x.test/2"}); err != nil {
		t.Fatalf("AppendDelivery: %v", err)
	}
	if err := s.AppendFeedback("rec-a", VerdictLiked); err != nil {
		t.Fatalf("AppendFeedback: %v", err)
	}

	recIDs, urls := s.RecentDelivered(0)
	if len(recIDs) != 2 || len(urls) != 2 {
		t.Fatalf("RecentDelivered = recs %v urls %v", recIDs, urls)
	}
	// Windowed: only the newest delivery.
	recIDs, urls = s.RecentDelivered(1)
	if len(urls) != 1 || urls[0] != "https://x.test/2" {
		t.Errorf("windowed urls = %v", urls)
	}
	if len(recIDs) != 2 {
		t.Errorf("windowed recs = %v (newest delivery carries both)", recIDs)
	}
}

func TestFeedbackValidation(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.AppendFeedback("", VerdictLiked); err == nil {
		t.Error("empty item must be rejected")
	}
	if err := s.AppendFeedback("thing", "meh"); err == nil {
		t.Error("unknown verdict must be rejected")
	}
	if got := len(s.Recent(0)); got != 0 {
		t.Errorf("rejected feedback must not persist, got %d records", got)
	}
}

func TestTornTailSkipped(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	if err := s.AppendDelivery("2026-07-15", "c", nil, []string{"https://x.test/1"}); err != nil {
		t.Fatalf("AppendDelivery: %v", err)
	}
	// Simulate a crash-torn final line.
	f, err := os.OpenFile(filepath.Join(dir, recordFile), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"id":"bh_torn","kind":"deliv`); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	recs := Open(dir).Recent(0)
	if len(recs) != 1 || recs[0].Kind != KindDelivery {
		t.Fatalf("a torn tail must be skipped, got %+v", recs)
	}
}
