package cmd

import (
	"reflect"
	"testing"
)

func TestReadyIssueIDsSkipsIssuesWithOpenMR(t *testing.T) {
	orig := issueHasOpenMRFn
	t.Cleanup(func() { issueHasOpenMRFn = orig })
	issueHasOpenMRFn = func(id string) bool { return id == "gs-inmq" }

	tracked := []trackedIssueInfo{
		{ID: "gs-ready", Status: "open", IssueType: "task"},
		{ID: "gs-inmq", Status: "open", IssueType: "task"},
		{ID: "gs-done", Status: "closed", IssueType: "task"},
	}
	got := readyIssueIDs(tracked, map[string]bool{}, func(string) bool { return true })
	if want := []string{"gs-ready"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("readyIssueIDs = %v, want %v", got, want)
	}
}
