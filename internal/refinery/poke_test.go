package refinery

import (
	"reflect"
	"testing"
)

func TestReadyWorkKeys(t *testing.T) {
	got := ReadyWorkKeys([]*MRInfo{
		{ID: "gs-wisp-b", CommitSHA: "222"},
		nil,
		{ID: ""},
		{ID: "gs-wisp-a", CommitSHA: "111"},
	})
	want := []string{"gs-wisp-a@111", "gs-wisp-b@222"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadyWorkKeys = %v, want %v", got, want)
	}
}

// TestShouldPokeRefineryOnlyForNewReadyWork walks the sequences that used to
// produce repeated "MERGE_READY received" pokes and checks that only new,
// unclaimed, ready work since the last check produces one.
func TestShouldPokeRefineryOnlyForNewReadyWork(t *testing.T) {
	townRoot := t.TempDir()
	const rig = "graystone"
	steps := []struct {
		name  string
		ready []string
		want  bool
	}{
		{"empty queue", nil, false},
		{"new MR A", []string{"A@1"}, true},
		{"gt done re-run on A (unchanged queue)", []string{"A@1"}, false},
		{"same again", []string{"A@1"}, false},
		{"new MR B arrives", []string{"A@1", "B@1"}, true},
		{"refinery claims A", []string{"B@1"}, false},
		{"queue drains", nil, false},
		{"empty again", nil, false},
		{"A re-pushed with a new commit", []string{"A@2"}, true},
		{"refinery claims A@2", nil, false},
		{"A@2 released back to the queue", []string{"A@2"}, true},
	}
	for _, s := range steps {
		if got := ShouldPokeRefinery(townRoot, rig, s.ready); got != s.want {
			t.Fatalf("%s: ShouldPokeRefinery(%v) = %v, want %v", s.name, s.ready, got, s.want)
		}
	}

	// Ledgers are per rig.
	if !ShouldPokeRefinery(townRoot, "otherrig", []string{"A@2"}) {
		t.Fatal("another rig's ledger suppressed a poke")
	}
}
