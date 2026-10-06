package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
)

type leaseFakeGit struct{ inTarget map[string]bool }

func (f leaseFakeGit) VerifyPushedCommitReachableFromPushTarget(_, _, commit string) error {
	if f.inTarget[commit] {
		return nil
	}
	return errors.New("not reachable")
}
func (leaseFakeGit) PushRemoteBranchTip(_, _ string) (string, error) { return "", nil }
func (leaseFakeGit) HasOpenPullRequest(git.PullRequestRef) bool      { return false }
func (leaseFakeGit) Rev(string) (string, error)                      { return "", nil }
func (leaseFakeGit) DeleteRemoteBranchIfAt(_, _, _ string) error     { return nil }
func (leaseFakeGit) DeleteBranch(string, bool) error                 { return nil }

func TestMQPostMergeDeleteLease(t *testing.T) {
	mr := &refinery.MergeRequest{TargetBranch: "main"}
	g := leaseFakeGit{inTarget: map[string]bool{"resolved": true}}
	cases := []struct{ name, tip, want string }{
		{"unchanged branch leases the submitted head", "submitted", "submitted"},
		{"re-pushed tip merged into target leases the tip (gs-ltz)", "resolved", "resolved"},
		{"re-pushed tip NOT in target keeps the submitted head (delete refused)", "unmerged", "submitted"},
		{"branch already gone", "", "submitted"},
	}
	for _, tc := range cases {
		if got := mqPostMergeDeleteLease(g, mr, "submitted", tc.tip); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
