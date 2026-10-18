// dedup.go makes nudge delivery idempotent per target session.
//
// Several independent paths can hand the same text to one session: the
// nudge-poller, the wait-idle watcher, the Claude UserPromptSubmit drain, a
// requeue after a delivery error, a sender that repeats itself while the
// target is busy (deacon HEALTH_CHECK every patrol), and the direct "wake up"
// pokes (POLECAT_DONE, "Polecat dispatched"). Each copy costs the target a
// full model turn. Grok session logs from 2026-10-15..17 showed witness and
// refinery sessions receiving the exact same nudge text again within minutes,
// one witness session spending 492 of 506 turns on repeats.
//
// The delivery ledger records a content key (sender + message) for every
// nudge that reached a session. Any path that is about to type a nudge into a
// session checks the ledger first and drops copies delivered within the
// dedupe window (operational.nudge.dedup_window, default 10m).
//
// Ledger location: <townRoot>/.runtime/nudge_delivered/<session>.json
package nudge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/lock"
)

// deliveryLedgerDir returns the directory holding per-session delivery ledgers.
func deliveryLedgerDir(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_delivered")
}

// deliveryLedgerPath returns the ledger file for one session.
func deliveryLedgerPath(townRoot, session string) string {
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(deliveryLedgerDir(townRoot), safe+".json")
}

// DeliveryKey returns the content key used to dedupe a nudge for one target.
// Whitespace at the ends is ignored so a trailing newline from --stdin does
// not make two otherwise identical nudges look different.
func DeliveryKey(sender, message string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(sender) + "\x00" + strings.TrimSpace(message)))
	return hex.EncodeToString(h[:16])
}

// dedupWindow returns the configured dedupe window for the town.
func dedupWindow(townRoot string) time.Duration {
	return nudgeConfig(townRoot).DedupWindowD()
}

// withLedger runs fn with the session's ledger loaded under an exclusive
// flock. If fn returns true the (pruned) ledger is written back. Ledger
// errors are swallowed: dedupe is an optimisation and must never block or
// drop a delivery on its own.
func withLedger(townRoot, session string, fn func(ledger map[string]int64, now time.Time, window time.Duration) bool) {
	if townRoot == "" || session == "" {
		return
	}
	window := dedupWindow(townRoot)
	if window <= 0 {
		return
	}
	dir := deliveryLedgerDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	path := deliveryLedgerPath(townRoot, session)
	unlock, err := lock.FlockAcquire(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()

	ledger := map[string]int64{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &ledger)
	}
	now := time.Now()
	// Prune entries that have aged out so the file stays small.
	for k, ts := range ledger {
		if now.Sub(time.Unix(0, ts)) > window {
			delete(ledger, k)
		}
	}
	if fn(ledger, now, window) {
		_ = atomicfile.WriteJSON(path, ledger)
	}
}

// RecentlyDelivered reports whether an identical nudge (same sender and text)
// reached the session within the dedupe window.
func RecentlyDelivered(townRoot, session, sender, message string) bool {
	seen := false
	key := DeliveryKey(sender, message)
	withLedger(townRoot, session, func(ledger map[string]int64, _ time.Time, _ time.Duration) bool {
		_, seen = ledger[key]
		return false
	})
	return seen
}

// RecordDelivered marks a nudge as delivered to the session. Call it after the
// text was typed into the session (including an unverified submit, where the
// text and Enter were sent but the probe could not prove it left the composer).
// Do not call it when delivery failed before anything was typed: the nudge is
// requeued in that case and must stay deliverable.
func RecordDelivered(townRoot, session, sender, message string) {
	key := DeliveryKey(sender, message)
	withLedger(townRoot, session, func(ledger map[string]int64, now time.Time, _ time.Duration) bool {
		ledger[key] = now.UnixNano()
		return true
	})
}

// RecordDeliveredNudges marks every queued nudge in a batch as delivered.
func RecordDeliveredNudges(townRoot, session string, nudges []QueuedNudge) {
	if len(nudges) == 0 {
		return
	}
	withLedger(townRoot, session, func(ledger map[string]int64, now time.Time, _ time.Duration) bool {
		for _, n := range nudges {
			ledger[DeliveryKey(n.Sender, n.Message)] = now.UnixNano()
		}
		return true
	})
}

// DropDuplicates filters a drained batch down to the nudges the session has
// not already seen: copies delivered within the dedupe window are dropped,
// and repeated entries inside the batch itself (e.g. four queued
// "HEALTH_CHECK from deacon") collapse to the first one. Order is preserved.
// It only reads the ledger; call RecordDeliveredNudges after delivery.
func DropDuplicates(townRoot, session string, nudges []QueuedNudge) []QueuedNudge {
	if len(nudges) == 0 {
		return nudges
	}
	var ledger map[string]int64
	withLedger(townRoot, session, func(l map[string]int64, _ time.Time, _ time.Duration) bool {
		ledger = make(map[string]int64, len(l))
		for k, v := range l {
			ledger[k] = v
		}
		return false
	})

	seen := make(map[string]bool, len(nudges))
	kept := make([]QueuedNudge, 0, len(nudges))
	for _, n := range nudges {
		key := DeliveryKey(n.Sender, n.Message)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, delivered := ledger[key]; delivered {
			continue
		}
		kept = append(kept, n)
	}
	return kept
}
