package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/nudge"
)

// TestShouldPokeRefineryGatesOnReadyWork checks the MERGE_READY gate used by
// gt done and gt mq submit: no poke for an empty queue, one poke per new
// ready MR, none for a repeat gt done on an MR the refinery already knows.
func TestShouldPokeRefineryGatesOnReadyWork(t *testing.T) {
	townRoot := t.TempDir()
	const rig, sess, msg = "graystone", "gs-refinery", "MERGE_READY received - check inbox for pending work"

	ready := []string{}
	orig := refineryReadyWorkKeys
	refineryReadyWorkKeys = func(string, string) ([]string, error) { return ready, nil }
	t.Cleanup(func() { refineryReadyWorkKeys = orig })

	if shouldPokeRefinery(townRoot, rig, sess, msg) {
		t.Fatal("poked for an empty queue")
	}
	ready = []string{"gs-wisp-a@111"}
	if !shouldPokeRefinery(townRoot, rig, sess, msg) {
		t.Fatal("did not poke for new ready MR")
	}
	if shouldPokeRefinery(townRoot, rig, sess, msg) {
		t.Fatal("poked again for an unchanged queue (gt done re-run)")
	}
	ready = []string{"gs-wisp-a@111", "gs-wisp-b@222"}
	if !shouldPokeRefinery(townRoot, rig, sess, msg) {
		t.Fatal("did not poke for a second new MR")
	}
}

// If the merge queue cannot be read, fall back to the content dedupe window:
// the first poke goes out, a repeat within the window does not.
func TestShouldPokeRefineryFallsBackToDedupeWindow(t *testing.T) {
	townRoot := t.TempDir()
	const rig, sess, msg = "graystone", "gs-refinery", "MERGE_READY received - check inbox for pending work"

	orig := refineryReadyWorkKeys
	refineryReadyWorkKeys = func(string, string) ([]string, error) { return nil, errors.New("dolt down") }
	t.Cleanup(func() { refineryReadyWorkKeys = orig })

	if !shouldPokeRefinery(townRoot, rig, sess, msg) {
		t.Fatal("first poke suppressed when the queue is unreadable")
	}
	nudge.RecordDelivered(townRoot, sess, "", msg)
	if shouldPokeRefinery(townRoot, rig, sess, msg) {
		t.Fatal("repeat poke within the dedupe window was not suppressed")
	}
}
