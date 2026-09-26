package daemon

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/session"
	"github.com/shunmei/cc-clip/internal/token"
)

func newReceiptTestServer(t *testing.T) *Server {
	t.Helper()
	tm := token.NewManager(time.Hour)
	_, _ = tm.Generate()
	return NewServer("127.0.0.1:0", &mockClipboard{}, tm, session.NewStore(12*time.Hour))
}

func storePath(dir string, port int) string {
	return filepath.Join(dir, fmt.Sprintf("%s%d%s", receiptStorePrefix, port, receiptStoreSuffix))
}

// TestRecordReceiptKeepsTheNewerTimeAndKnownTargets pins one store's merge: an
// out-of-order older write never wins, and a self-declared unknown target
// cannot grow the store with new keys.
func TestRecordReceiptKeepsTheNewerTimeAndKnownTargets(t *testing.T) {
	path := storePath(t.TempDir(), 18339)
	older := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(48 * time.Hour)

	for _, s := range []struct {
		target string
		at     time.Time
	}{{"codex", newer}, {"codex", older}, {"made-up-cli", older}} {
		if err := RecordReceipt(path, "venus", s.target, s.at); err != nil {
			t.Fatal(err)
		}
	}

	got, err := LoadReceipts(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{"codex": newer, UnattributedTarget: older}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want targets %v", got, want)
	}
	for _, r := range got {
		if at, ok := want[r.Target]; !ok || !r.LastAccepted.Equal(at) {
			t.Errorf("unexpected receipt %+v", r)
		}
	}
}

// TestConcurrentDaemonsLoseNoReceipts reproduces two daemons accepting at the
// same moment. One shared read-merge-write file lost a receipt in every round;
// with a store per daemon nothing is shared, and the read side merges.
func TestConcurrentDaemonsLoseNoReceipts(t *testing.T) {
	dir := t.TempDir()
	const rounds = 30
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		for i, host := range []string{"host-a", "host-b"} {
			wg.Add(1)
			go func(port int, host string) {
				defer wg.Done()
				if err := RecordReceipt(storePath(dir, port), host, "codex", time.Now().UTC()); err != nil {
					t.Error(err)
				}
			}(18339+i, host)
		}
		wg.Wait()
	}

	got, err := LoadAllReceipts(dir)
	if err != nil {
		t.Fatal(err)
	}
	hosts := make(map[string]bool)
	for _, r := range got {
		hosts[r.Host] = true
	}
	if !hosts["host-a"] || !hosts["host-b"] || len(got) != 2 {
		t.Fatalf("expected one receipt per daemon's host, got %+v", got)
	}
}

// TestNotifyRecordsAReceiptPerTarget runs real /notify requests: the Claude
// hook content type attributes itself, a generic payload names its target, and
// only a nonce-bound host ever names the receipt — a body host sent with an
// unbound nonce must not create a receipt for that host.
func TestNotifyRecordsAReceiptPerTarget(t *testing.T) {
	srv := newReceiptTestServer(t)
	dir := t.TempDir()
	path := storePath(dir, 18339)
	srv.EnableDeliveryReceipts(path)
	if err := srv.RegisterNotificationNonceForHost("nonce-venus", "venus"); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterNotificationNonce("nonce-unbound"); err != nil {
		t.Fatal(err)
	}

	requests := []struct{ nonce, ctype, body string }{
		{"nonce-venus", "application/x-claude-hook", `{"hook_event_name":"Stop"}`},
		{"nonce-venus", "application/json", `{"title":"Codex","body":"done","target":"codex","host":"spoofed"}`},
		{"nonce-venus", "application/json", `{"title":"hand-run","body":"no target"}`},
		{"nonce-unbound", "application/json", `{"title":"x","body":"y","target":"cursor","host":"victim"}`},
	}
	for _, rq := range requests {
		req := httptest.NewRequest("POST", "/notify", strings.NewReader(rq.body))
		req.Header.Set("Authorization", "Bearer "+rq.nonce)
		req.Header.Set("Content-Type", rq.ctype)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s: expected 204, got %d: %s", rq.body, w.Code, w.Body.String())
		}
	}

	want := map[[2]string]bool{
		{"venus", "claude"}: true, {"venus", "codex"}: true,
		{"venus", UnattributedTarget}: true, {"", "cursor"}: true,
	}
	var got []Receipt
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = LoadReceipts(path)
		if len(got) == len(want) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != len(want) {
		t.Fatalf("got receipts %+v, want keys %v", got, want)
	}
	for _, r := range got {
		if !want[[2]string{r.Host, r.Target}] {
			t.Errorf("unexpected receipt %+v (a body host must never name a receipt)", r)
		}
	}
}

// blockingWriter never returns from Write until released, like a log sink on a
// stalled disk or an unread pipe.
type blockingWriter struct{ release chan struct{} }

func (w blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

// TestNotifyAnswersWhileTheReceiptWriterIsStuck pins that receipts are off the
// response path: with the writer blocked and the log sink blocked too (a full
// queue must not log from the handler), a real HTTP client still gets its 204
// well inside the 5s timeout the notify runner uses.
func TestNotifyAnswersWhileTheReceiptWriterIsStuck(t *testing.T) {
	sink := blockingWriter{release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(sink.release) }) }
	prevOut := log.Writer()
	log.SetOutput(sink)
	defer func() {
		release()
		log.SetOutput(prevOut)
	}()

	srv := newReceiptTestServer(t)
	stuck := make(chan receiptEvent) // no writer: every send would block
	srv.receiptCh = stuck
	if err := srv.RegisterNotificationNonceForHost("nonce-venus", "venus"); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// Runs before ts.Close: if a regression makes the handler block on the
	// send, unblock it so the test fails on the timeout instead of hanging.
	defer func() {
		release()
		go func() {
			for range stuck {
			}
		}()
	}()

	client := &http.Client{Timeout: time.Second}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", ts.URL+"/notify", strings.NewReader(`{"title":"t","body":"b","target":"codex"}`))
		req.Header.Set("Authorization", "Bearer nonce-venus")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d blocked on the receipt writer: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}
}

// TestLoadAllReceiptsReadsOnlyDaemonStores pins store discovery by exact name:
// a token directory with glob metacharacters stays readable, and backups or
// temp files never join the merge.
func TestLoadAllReceiptsReadsOnlyDaemonStores(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "user[1]")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	if err := RecordReceipt(storePath(dir, 18339), "venus", "codex", at); err != nil {
		t.Fatal(err)
	}
	for _, stray := range []string{"notify-receipts-backup.json", "notify-receipts-0.json", "notify-receipts-018339.json"} {
		if err := RecordReceipt(filepath.Join(dir, stray), "stray", "claude", at); err != nil {
			t.Fatal(err)
		}
	}

	got, err := LoadAllReceipts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Host != "venus" {
		t.Fatalf("want only the daemon store's receipt, got %+v", got)
	}
}
