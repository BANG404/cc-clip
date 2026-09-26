package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shunmei/cc-clip/internal/token"
)

// Delivery receipts record when the daemon last accepted a notification from a
// remote host and Target. They let doctor tell "the hook is wired" apart from
// "the hook actually fires" — the gap that leaves adapters no one can test on
// real hardware marked unverified.

// Each daemon owns one store file, named by its port. Two daemons can never
// share a port, so no file ever has two writers and no cross-process lock is
// needed; doctor merges every daemon's file when it reads (LoadAllReceipts).
const (
	receiptStorePrefix = "notify-receipts-"
	receiptStoreSuffix = ".json"
)

// UnattributedTarget is recorded when a sender names no Target, or one this
// daemon does not know (an older remote binary, a hand-rolled sender).
const UnattributedTarget = "unattributed"

// ReceiptTargets are the Targets a receipt can be attributed to. Anything else
// collapses to UnattributedTarget: the field is self-declared by the sender,
// so it must not grow the store with arbitrary keys.
var ReceiptTargets = []string{"claude", "codex", "cursor", "opencode", "agy"}

// Receipt is the last accepted notification for one (Host, Target).
type Receipt struct {
	Host         string    `json:"host"`
	Target       string    `json:"target"`
	LastAccepted time.Time `json:"last_accepted"`
}

type receiptStore struct {
	Version  int       `json:"version"`
	Receipts []Receipt `json:"receipts"`
}

// NormalizeReceiptTarget maps a sender-declared target onto a known Target.
func NormalizeReceiptTarget(target string) string {
	for _, known := range ReceiptTargets {
		if target == known {
			return target
		}
	}
	return UnattributedTarget
}

// ReceiptStorePath is where the daemon on port persists its receipts, beside
// the other daemon state.
func ReceiptStorePath(port int) (string, error) {
	dir, err := token.TokenDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s%d%s", receiptStorePrefix, port, receiptStoreSuffix)), nil
}

// LoadAllReceipts merges every daemon's store in dir, keeping the newest
// acceptance per (Host, Target). An unreadable store is skipped and reported
// in the error alongside whatever the others held.
func LoadAllReceipts(dir string) ([]Receipt, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list receipt stores: %w", err)
	}
	var paths []string
	for _, e := range entries {
		if isReceiptStoreName(e.Name()) {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	newest := make(map[[2]string]Receipt)
	var errs []error
	for _, path := range paths {
		receipts, err := LoadReceipts(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, r := range receipts {
			key := [2]string{r.Host, r.Target}
			if cur, ok := newest[key]; !ok || r.LastAccepted.After(cur.LastAccepted) {
				newest[key] = r
			}
		}
	}
	merged := make([]Receipt, 0, len(newest))
	for _, r := range newest {
		merged = append(merged, r)
	}
	return merged, errors.Join(errs...)
}

// isReceiptStoreName accepts exactly notify-receipts-<port>.json. Matching
// names instead of globbing the joined path keeps a token directory containing
// glob metacharacters readable, and keeps backups or temp files out of the
// merge.
func isReceiptStoreName(name string) bool {
	port, ok := strings.CutPrefix(name, receiptStorePrefix)
	if !ok {
		return false
	}
	port, ok = strings.CutSuffix(port, receiptStoreSuffix)
	if !ok || port == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == port
}

// LoadReceipts reads the receipt store. A missing file is no receipts, not an
// error: a daemon that has never accepted a notification has none.
func LoadReceipts(path string) ([]Receipt, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read receipt store: %w", err)
	}
	var store receiptStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("decode receipt store: %w", err)
	}
	return store.Receipts, nil
}

// RecordReceipt merges one acceptance into the store at path. Only the owning
// daemon's single receipt writer calls it (see Server.runReceiptWriter), so the
// read-merge-write has no concurrent writer. The newer timestamp wins per key.
// A store that cannot be decoded is replaced rather than blocking every later
// receipt.
func RecordReceipt(path, host, target string, at time.Time) error {
	existing, err := LoadReceipts(path)
	if err != nil {
		existing = nil
	}
	target = NormalizeReceiptTarget(target)
	merged := make([]Receipt, 0, len(existing)+1)
	found := false
	for _, r := range existing {
		if r.Host == host && r.Target == target {
			found = true
			if at.After(r.LastAccepted) {
				r.LastAccepted = at
			}
		}
		merged = append(merged, r)
	}
	if !found {
		merged = append(merged, Receipt{Host: host, Target: target, LastAccepted: at})
	}
	data, err := json.Marshal(receiptStore{Version: 1, Receipts: merged})
	if err != nil {
		return fmt.Errorf("encode receipt store: %w", err)
	}
	return writeFileAtomic(path, append(data, '\n'))
}
