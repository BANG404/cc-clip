package doctor

import (
	"fmt"
	"strings"
	"time"

	"github.com/shunmei/cc-clip/internal/daemon"
	"github.com/shunmei/cc-clip/internal/token"
)

// ReceiptStaleAfter is how old a delivery receipt may get before doctor calls
// it stale. Stale is a reminder, not a failure: not using one CLI for a week is
// normal, and doctor must not go red over it.
const ReceiptStaleAfter = 7 * 24 * time.Hour

const receiptCheckName = "delivery-receipt"

// DeliveryReceipts reports, per Target, when the local daemon last accepted a
// notification from host. It answers what the wiring checks cannot: whether a
// hook that is configured has ever actually fired. Every result is OK; the
// three states (recent, stale, never received) are information, kept distinct
// rather than folded into one pass/fail.
func DeliveryReceipts(host string) []CheckResult {
	dir, err := token.TokenDir()
	if err != nil {
		return []CheckResult{{receiptCheckName, true, fmt.Sprintf("receipt store unavailable: %v", err)}}
	}
	// Every daemon (one per port) keeps its own store; read them all.
	receipts, err := daemon.LoadAllReceipts(dir)
	results := classifyReceipts(host, receipts, time.Now())
	if err != nil {
		results = append(results, CheckResult{receiptCheckName, true, fmt.Sprintf("some receipt stores were unreadable: %v", err)})
	}
	return results
}

func classifyReceipts(host string, receipts []daemon.Receipt, now time.Time) []CheckResult {
	last := make(map[string]time.Time)
	for _, r := range receipts {
		if r.Host == host {
			last[r.Target] = r.LastAccepted
		}
	}

	targets := make([]string, 0, len(daemon.ReceiptTargets)+1)
	targets = append(targets, daemon.ReceiptTargets...)
	targets = append(targets, daemon.UnattributedTarget)

	var out []CheckResult
	var never []string
	for _, target := range targets {
		at, ok := last[target]
		if !ok {
			if target != daemon.UnattributedTarget {
				never = append(never, target)
			}
			continue
		}
		out = append(out, CheckResult{receiptCheckName + ":" + target, true, receiptAge(now.Sub(at))})
	}
	if len(never) > 0 {
		out = append(out, CheckResult{receiptCheckName, true,
			"never received from: " + strings.Join(never, ", ") + " (fine for any you do not use on this host)"})
	}
	return out
}

func receiptAge(age time.Duration) string {
	msg := fmt.Sprintf("last notification accepted %s ago", formatDuration(age))
	if age > ReceiptStaleAfter {
		return fmt.Sprintf("stale: %s (older than %s)", msg, formatDuration(ReceiptStaleAfter))
	}
	return msg
}
