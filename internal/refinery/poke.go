// poke.go gates the "MERGE_READY received - check inbox for pending work"
// tmux pokes that wake the refinery.
//
// Every `gt done` with an MR and every new `gt mq submit` used to poke the
// refinery, as did the witness when it saw a pending MR. A polecat re-running
// gt done against an MR that already existed, or several completions landing
// while the refinery was busy with the head of the queue, produced a stream of
// identical pokes (45 in the refinery's Grok logs between 2026-10-16 12:00 and
// 2026-10-17 12:00, median gap ~4.5 minutes). The refinery is a Claude session
// and each poke costs it a full model turn even when the queue is empty or it
// already holds the work.
//
// The gate keeps, per rig, the set of ready+unclaimed MRs that existed at the
// last check. A poke goes out only when the current set contains an MR (or a
// new commit on an MR) that was not in that set. An empty or unchanged queue
// sends nothing. The channel event that unblocks the refinery's await-event
// loop is emitted by the callers regardless; only the tmux poke is gated.
//
// Ledger location: <townRoot>/.runtime/refinery_poke/<rig>.json
package refinery

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/rig"
)

// pokeLedger is the persisted state of the refinery poke gate for one rig.
type pokeLedger struct {
	// Ready holds the ready-work keys seen at the last gate check.
	Ready []string `json:"ready"`
	// CheckedAt is when the gate last ran (diagnostics only).
	CheckedAt time.Time `json:"checked_at"`
	// PokedAt is when the gate last allowed a poke (diagnostics only).
	PokedAt time.Time `json:"poked_at,omitempty"`
}

func pokeLedgerPath(townRoot, rigName string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "refinery_poke", rigName+".json")
}

// ReadyWorkKeys turns ready MRs into stable keys. The commit SHA is part of
// the key so a polecat re-pushing new work onto the same MR counts as new.
func ReadyWorkKeys(mrs []*MRInfo) []string {
	keys := make([]string, 0, len(mrs))
	for _, mr := range mrs {
		if mr == nil || mr.ID == "" {
			continue
		}
		keys = append(keys, mr.ID+"@"+mr.CommitSHA)
	}
	sort.Strings(keys)
	return keys
}

// ReadyWorkKeysForRig lists the rig's ready, unclaimed MRs (the same filter
// the refinery's own queue uses: open, unblocked, unassigned or stale claim)
// and returns their keys.
func ReadyWorkKeysForRig(townRoot, rigName string) ([]string, error) {
	e := NewEngineer(&rig.Rig{Name: rigName, Path: filepath.Join(townRoot, rigName)})
	e.SetOutput(io.Discard)
	mrs, err := e.ListReadyMRs()
	if err != nil {
		return nil, err
	}
	return ReadyWorkKeys(mrs), nil
}

// hasNewReadyWork reports whether cur holds any key that prev does not.
func hasNewReadyWork(prev, cur []string) bool {
	seen := make(map[string]bool, len(prev))
	for _, k := range prev {
		seen[k] = true
	}
	for _, k := range cur {
		if !seen[k] {
			return true
		}
	}
	return false
}

// ShouldPokeRefinery records the current ready-work set for the rig and
// reports whether it contains work that was not ready at the last check.
// Returns false for an empty queue and for a queue that only holds work the
// refinery was already poked about (or has since claimed and released into
// the same state). The decision and the ledger update happen under one flock
// so two concurrent `gt done` runs cannot both poke for the same MR.
//
// If the ledger cannot be locked or written, it fails open (poke whenever
// anything is ready) so a filesystem problem never strands ready work.
func ShouldPokeRefinery(townRoot, rigName string, readyKeys []string) bool {
	path := pokeLedgerPath(townRoot, rigName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return len(readyKeys) > 0
	}
	unlock, err := lock.FlockAcquire(path + ".lock")
	if err != nil {
		return len(readyKeys) > 0
	}
	defer unlock()

	var prev pokeLedger
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &prev)
	}
	// An empty queue never pokes, but its (empty) set is still recorded so
	// work that shows up later, or is released back, counts as new.
	poke := hasNewReadyWork(prev.Ready, readyKeys)
	next := pokeLedger{Ready: readyKeys, CheckedAt: time.Now(), PokedAt: prev.PokedAt}
	if poke {
		next.PokedAt = next.CheckedAt
	}
	if err := atomicfile.WriteJSON(path, next); err != nil {
		return len(readyKeys) > 0
	}
	return poke
}
