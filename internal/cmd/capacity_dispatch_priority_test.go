package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

func TestSortScheduledCandidatesPriorityThenFIFO(t *testing.T) {
	p1, p3 := 1, 3
	mk := func(ctxID, work, at string) scheduledContextAssessment {
		return scheduledContextAssessment{
			context: slingContextRecord{issue: &beads.Issue{ID: ctxID}},
			fields:  &capacity.SlingContextFields{WorkBeadID: work, EnqueuedAt: at},
		}
	}
	c := []scheduledContextAssessment{
		mk("ctx-a", "gs-old-p3", "2026-10-06T01:00:00Z"),
		mk("ctx-b", "gs-unknown", "2026-10-06T02:00:00Z"),
		mk("ctx-c", "gs-new-p1", "2026-10-06T03:00:00Z"),
	}
	info := map[string]beadStatusInfo{"gs-old-p3": {Priority: &p3}, "gs-new-p1": {Priority: &p1}}
	sortScheduledCandidates(c, info)
	got := []string{c[0].fields.WorkBeadID, c[1].fields.WorkBeadID, c[2].fields.WorkBeadID}
	want := []string{"gs-new-p1", "gs-unknown", "gs-old-p3"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestClosedDependencyWithOpenMR(t *testing.T) {
	deps := []beads.IssueDep{
		{ID: "gs-open", Status: "open", DependencyType: "blocks"},
		{ID: "gs-merged", Status: "closed", DependencyType: "blocks"},
		{ID: "gs-submitted", Status: "closed", DependencyType: "blocks"},
		{ID: "gs-parent", Status: "closed", DependencyType: "parent-child"},
	}
	open := map[string]bool{"gs-submitted": true, "gs-parent": true}
	if got := closedDependencyWithOpenMR(deps, func(id string) bool { return open[id] }); got != "gs-submitted" {
		t.Fatalf("got %q, want gs-submitted", got)
	}
	if got := closedDependencyWithOpenMR(deps[:2], func(id string) bool { return open[id] }); got != "" {
		t.Fatalf("got %q, want none", got)
	}
}
