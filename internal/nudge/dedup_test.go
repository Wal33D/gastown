package nudge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveryLedgerRecordsAndDetectsRepeats(t *testing.T) {
	townRoot := t.TempDir()
	session := "gs-witness"

	if RecentlyDelivered(townRoot, session, "graystone/refinery", "held gs-wisp-x") {
		t.Fatal("fresh ledger reports a delivery")
	}
	RecordDelivered(townRoot, session, "graystone/refinery", "held gs-wisp-x\n")

	// Trailing whitespace does not make it a different nudge.
	if !RecentlyDelivered(townRoot, session, "graystone/refinery", "held gs-wisp-x") {
		t.Fatal("identical nudge not detected as delivered")
	}
	// Other sender, other text, other session: all distinct.
	if RecentlyDelivered(townRoot, session, "mayor", "held gs-wisp-x") {
		t.Error("different sender treated as duplicate")
	}
	if RecentlyDelivered(townRoot, session, "graystone/refinery", "held gs-wisp-y") {
		t.Error("different text treated as duplicate")
	}
	if RecentlyDelivered(townRoot, "gs-refinery", "graystone/refinery", "held gs-wisp-x") {
		t.Error("different session treated as duplicate")
	}
}

// TestDropDuplicatesAfterDelivery reproduces the requeue/second-drain path:
// a batch is drained and typed into the session, then the same nudge comes
// back through the queue (requeue after an unverified submit, a wait-idle
// fallback enqueue, or the sender repeating itself). The second drain must
// not hand it to the session again.
func TestDropDuplicatesAfterDelivery(t *testing.T) {
	townRoot := t.TempDir()
	session := "gs-witness"
	msg := QueuedNudge{Sender: "graystone/furiosa", Message: "gs-7lm23: verify running on head 48eb6382"}

	if err := Enqueue(townRoot, session, msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	first, err := Drain(townRoot, session)
	if err != nil || len(first) != 1 {
		t.Fatalf("first Drain = %d, %v; want 1 nudge", len(first), err)
	}
	first = DropDuplicates(townRoot, session, first)
	if len(first) != 1 {
		t.Fatalf("DropDuplicates dropped a never-delivered nudge")
	}
	RecordDeliveredNudges(townRoot, session, first) // typed into the pane

	// Same nudge arrives again, alongside a new one.
	if err := Enqueue(townRoot, session, msg); err != nil {
		t.Fatalf("re-Enqueue: %v", err)
	}
	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "mayor", Message: "new instruction"}); err != nil {
		t.Fatalf("Enqueue new: %v", err)
	}
	second, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	second = DropDuplicates(townRoot, session, second)
	if len(second) != 1 || second[0].Message != "new instruction" {
		t.Fatalf("second delivery = %#v, want only the new instruction", second)
	}
}

func TestDropDuplicatesCollapsesWithinBatch(t *testing.T) {
	townRoot := t.TempDir()
	batch := []QueuedNudge{
		{Sender: "deacon", Message: "HEALTH_CHECK from deacon"},
		{Sender: "mayor", Message: "a"},
		{Sender: "deacon", Message: "HEALTH_CHECK from deacon"},
		{Sender: "deacon", Message: "HEALTH_CHECK from deacon"},
	}
	got := DropDuplicates(townRoot, "gs-refinery", batch)
	if len(got) != 2 || got[0].Message != "HEALTH_CHECK from deacon" || got[1].Message != "a" {
		t.Fatalf("DropDuplicates = %#v, want [HEALTH_CHECK, a]", got)
	}
}

func TestDeliveryDedupeCanBeDisabled(t *testing.T) {
	townRoot := t.TempDir()
	settings := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settings, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"town-settings","version":1,"operational":{"nudge":{"dedup_window":"0s"}}}`
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	RecordDelivered(townRoot, "gs-witness", "deacon", "x")
	if RecentlyDelivered(townRoot, "gs-witness", "deacon", "x") {
		t.Fatal("dedup_window=0s should disable delivery dedupe")
	}
}
