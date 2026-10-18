package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestShouldSkipDrainUntilIdle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		hasPromptDetection bool
		waitErr            error
		want               bool
	}{
		{"prompt aware idle", true, nil, false},
		{"prompt aware busy", true, errors.New("timeout"), true},
		{"no prompt detection busy", false, errors.New("timeout"), false},
		{"no prompt detection idle", false, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipDrainUntilIdle(tt.hasPromptDetection, tt.waitErr); got != tt.want {
				t.Errorf("shouldSkipDrainUntilIdle(%v, %v) = %v, want %v", tt.hasPromptDetection, tt.waitErr, got, tt.want)
			}
		})
	}
}

// TestSettleDrainedDeliveryNoDuplicateAfterTypedSubmit reproduces the
// duplicate path seen in Grok witness/refinery logs: a drained batch is typed
// into the pane but the submit probe cannot verify it (Grok queues input while
// a turn runs, or the pane did not change after Enter). That batch must not be
// requeued, and if the same nudge comes back through the queue it must not be
// delivered again.
func TestSettleDrainedDeliveryNoDuplicateAfterTypedSubmit(t *testing.T) {
	townRoot := t.TempDir()
	session := "gs-witness"
	drained := []nudge.QueuedNudge{{Sender: "graystone/refinery", Message: "gs-wisp-13l1 held"}}

	// Post-typing failure, including the plain "Enter not processed" error
	// that tmux now classifies as ErrSubmitNotVerified.
	typedErr := fmt.Errorf("nudge to session %q: %w (nudge Enter not processed)", session, tmux.ErrSubmitNotVerified)
	settleDrainedDelivery(townRoot, session, "test", drained, typedErr)

	if n, _ := nudge.Pending(townRoot, session); n != 0 {
		t.Fatalf("Pending = %d after typed-but-unverified delivery, want 0 (no requeue)", n)
	}

	// The same text is queued again (a wait-idle fallback, a repeat sender).
	if err := nudge.Enqueue(townRoot, session, drained[0]); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	again, err := nudge.Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := nudge.DropDuplicates(townRoot, session, again); len(got) != 0 {
		t.Fatalf("duplicate delivered again: %#v", got)
	}
}

// A failure before anything was typed (lock timeout, session gone) keeps the
// batch deliverable: it is requeued and not recorded as delivered.
func TestSettleDrainedDeliveryRequeuesUntypedFailure(t *testing.T) {
	townRoot := t.TempDir()
	session := "gs-witness"
	drained := []nudge.QueuedNudge{{Sender: "mayor", Message: "resume the queue"}}

	settleDrainedDelivery(townRoot, session, "test", drained, errors.New("nudge lock timeout"))

	got, err := nudge.Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	got = nudge.DropDuplicates(townRoot, session, got)
	if len(got) != 1 || got[0].Message != "resume the queue" {
		t.Fatalf("requeued batch = %#v, want the original nudge", got)
	}
}

func TestNudgeTextTyped(t *testing.T) {
	t.Parallel()
	if !nudgeTextTyped(nil) {
		t.Error("nil should count as typed")
	}
	if !nudgeTextTyped(fmt.Errorf("x: %w", tmux.ErrSubmitNotVerified)) {
		t.Error("ErrSubmitNotVerified should count as typed")
	}
	if nudgeTextTyped(errors.New("session not found")) {
		t.Error("pre-typing error should not count as typed")
	}
}
