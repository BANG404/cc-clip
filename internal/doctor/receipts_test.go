package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/daemon"
)

// TestClassifyReceiptsKeepsThreeStates pins that recent, stale and never
// received stay distinct and that none of them fails the doctor run.
func TestClassifyReceiptsKeepsThreeStates(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	receipts := []daemon.Receipt{
		{Host: "venus", Target: "claude", LastAccepted: now.Add(-2 * time.Hour)},
		{Host: "venus", Target: "codex", LastAccepted: now.Add(-ReceiptStaleAfter - time.Hour)},
		{Host: "mars", Target: "cursor", LastAccepted: now}, // other host: ignored
	}

	results := classifyReceipts("venus", receipts, now)

	byName := make(map[string]string)
	for _, r := range results {
		if !r.OK {
			t.Errorf("%s must not fail the run: %s", r.Name, r.Message)
		}
		byName[r.Name] = r.Message
	}
	cases := []struct {
		name, want, notWant string
	}{
		{"delivery-receipt:claude", "ago", "stale"},
		{"delivery-receipt:codex", "stale", ""},
		{"delivery-receipt", "never received from: cursor, opencode, agy", ""},
	}
	for _, tc := range cases {
		msg, ok := byName[tc.name]
		if !ok || !strings.Contains(msg, tc.want) || (tc.notWant != "" && strings.Contains(msg, tc.notWant)) {
			t.Errorf("%s = %q, want it to contain %q", tc.name, msg, tc.want)
		}
	}
}
