package mail

import (
	"testing"
	"time"

	imap "github.com/emersion/go-imap"
)

func TestFormatMessageTimeUsesUTC8(t *testing.T) {
	for _, tc := range []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 11, 25, 7, 52, 32, 0, time.UTC), "2026-11-25T15:52:32+08:00"},
		{time.Date(2026, 11, 24, 23, 30, 0, 0, time.FixedZone("EST", -5*3600)), "2026-11-25T12:30:00+08:00"},
		{time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC), "2027-01-01T00:00:00+08:00"},
	} {
		if got := FormatMessageTime(tc.in); got != tc.want {
			t.Errorf("FormatMessageTime(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDisplayMessageTime(t *testing.T) {
	if got := DisplayMessageTime("2026-11-25T07:52:32Z"); got != "2026-11-25 15:52:32" {
		t.Fatalf("got %q", got)
	}
	if got := DisplayMessageTime("Tue, 24 Nov 2026 23:30:00 -0500"); got != "2026-11-25 12:30:00" {
		t.Fatalf("RFC1123Z input: got %q", got)
	}
	if got := DisplayMessageTime("garbage"); got != "garbage" {
		t.Fatalf("unparseable input must pass through, got %q", got)
	}
}

func TestIMAPMessageDateIsUTC8(t *testing.T) {
	msg := &imap.Message{Uid: 1, Envelope: &imap.Envelope{
		Date: time.Date(2026, 11, 25, 7, 52, 32, 0, time.UTC),
	}}
	if got := toMessageSummary(msg, "INBOX").Date; got != "2026-11-25T15:52:32+08:00" {
		t.Fatalf("envelope date = %q", got)
	}
	internal := &imap.Message{Uid: 2, InternalDate: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)}
	if got := toMessageSummary(internal, "INBOX").Date; got != "2026-11-25T08:00:00+08:00" {
		t.Fatalf("internal date fallback = %q", got)
	}
}

func TestSortStillOrdersAcrossOffsets(t *testing.T) {
	messages := []Message{
		{ID: "old", Date: "2026-11-25T08:00:00+08:00"},
		{ID: "new", Date: "2026-11-25T01:00:00Z"}, // 09:00 UTC+8
	}
	sortMessages(messages)
	if messages[0].ID != "new" {
		t.Fatalf("sort must compare instants, not strings: %v", messages)
	}
}
