package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/session"
	"github.com/shunmei/cc-clip/internal/token"
)

// TestRecordReceiptMergesWithTheStoreOnDisk pins the merge contract: daemons on
// different ports share one file, so a write must keep receipts it did not
// make, keep the newer timestamp per key, and never grow on unknown targets.
func TestRecordReceiptMergesWithTheStoreOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), receiptStoreFile)
	older := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(48 * time.Hour)

	steps := []struct {
		host, target string
		at           time.Time
	}{
		{"venus", "codex", newer},
		{"mars", "claude", older},       // another daemon's receipt
		{"venus", "codex", older},       // out-of-order older write must not win
		{"venus", "made-up-cli", older}, // self-declared, unknown
	}
	for _, s := range steps {
		if err := RecordReceipt(path, s.host, s.target, s.at); err != nil {
			t.Fatalf("RecordReceipt(%s, %s): %v", s.host, s.target, err)
		}
	}

	got, err := LoadReceipts(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{
		"venus/codex":                 newer,
		"mars/claude":                 older,
		"venus/" + UnattributedTarget: older,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d receipts, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		at, ok := want[r.Host+"/"+r.Target]
		if !ok || !r.LastAccepted.Equal(at) {
			t.Errorf("unexpected receipt %+v", r)
		}
	}
}

// TestNotifyRecordsAReceiptPerTarget runs real /notify requests: the Claude
// hook content type attributes itself, a generic payload names its target, and
// the host comes from the nonce binding, never the body.
func TestNotifyRecordsAReceiptPerTarget(t *testing.T) {
	tm := token.NewManager(time.Hour)
	_, _ = tm.Generate()
	srv := NewServer("127.0.0.1:0", &mockClipboard{}, tm, session.NewStore(12*time.Hour))
	path := filepath.Join(t.TempDir(), receiptStoreFile)
	srv.EnableDeliveryReceipts(path)
	if err := srv.RegisterNotificationNonceForHost("nonce-venus", "venus"); err != nil {
		t.Fatal(err)
	}

	requests := []struct{ ctype, body string }{
		{"application/x-claude-hook", `{"hook_event_name":"Stop"}`},
		{"application/json", `{"title":"Codex","body":"done","target":"codex","host":"spoofed"}`},
		{"application/json", `{"title":"hand-run","body":"no target"}`},
	}
	for _, rq := range requests {
		req := httptest.NewRequest("POST", "/notify", strings.NewReader(rq.body))
		req.Header.Set("Authorization", "Bearer nonce-venus")
		req.Header.Set("Content-Type", rq.ctype)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s: expected 204, got %d: %s", rq.body, w.Code, w.Body.String())
		}
	}

	got, err := LoadReceipts(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, r := range got {
		if r.Host != "venus" {
			t.Errorf("receipt host %q must come from the nonce binding", r.Host)
		}
		seen[r.Target] = true
	}
	for _, target := range []string{"claude", "codex", UnattributedTarget} {
		if !seen[target] {
			t.Errorf("no receipt recorded for %s; got %+v", target, got)
		}
	}
}
