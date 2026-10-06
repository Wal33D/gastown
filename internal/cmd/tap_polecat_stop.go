package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/workspace"
)

var tapPolecatStopCmd = &cobra.Command{
	Use:   "polecat-stop-check",
	Short: "Auto-run gt done on session Stop if polecat has pending work",
	Long: `Safety net for the "idle polecat" problem: polecats that finish work
but forget to call gt done before the session ends.

This command is designed to run from a Claude Code Stop hook. It checks:
1. Whether this is a polecat session (GT_POLECAT env var)
2. Whether gt done has already run (heartbeat state is "exiting" or "idle")
3. Whether the polecat has commits, stashes, or non-runtime dirty work

If the polecat has pending work that wasn't submitted, this command
runs gt done to submit it. If gt done already ran or there's nothing
to submit, it exits silently.

Exit codes:
  0 - No action needed (not a polecat, already done, or gt done succeeded)
  1 - gt done was attempted but failed`,
	RunE:         runTapPolecatStop,
	SilenceUsage: true,
}

func init() {
	tapCmd.AddCommand(tapPolecatStopCmd)
}

func runTapPolecatStop(cmd *cobra.Command, args []string) error {
	// Only applies to polecats
	polecatName := os.Getenv("GT_POLECAT")
	if polecatName == "" {
		return nil // Not a polecat session — nothing to do
	}

	sessionName := os.Getenv("GT_SESSION")
	if sessionName == "" {
		return nil // No session tracking — can't check state
	}

	// Find town root for heartbeat check
	townRoot, _, _ := workspace.FindFromCwdWithFallback()
	if townRoot == "" {
		townRoot = os.Getenv("GT_TOWN_ROOT")
	}
	if townRoot == "" {
		return nil // Can't find workspace — exit quietly
	}

	// Check heartbeat state: if already "exiting" or "idle", gt done already ran
	hb := polecat.ReadSessionHeartbeat(townRoot, sessionName)
	if hb != nil {
		state := hb.EffectiveState()
		if state == polecat.HeartbeatExiting || state == polecat.HeartbeatIdle {
			return nil // gt done already ran or polecat is idle — nothing to do
		}
	}

	// Check if the polecat is on a feature branch with work to submit.
	rigName := os.Getenv("GT_RIG")
	if rigName == "" {
		return nil
	}

	// Reconstruct polecat worktree path
	polecatDir := filepath.Join(townRoot, rigName, "polecats", polecatName)
	// Try the nested clone layout first (polecats/<name>/<rig>/)
	cloneDir := filepath.Join(polecatDir, rigName)
	if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
		// Fall back to flat layout
		cloneDir = polecatDir
		if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
			return nil // No git repo found — exit quietly
		}
	}

	// Check current branch — skip if on main/master
	branchCmd := exec.Command("git", "-C", cloneDir, "rev-parse", "--abbrev-ref", "HEAD")
	branchOut, err := branchCmd.Output()
	if err != nil {
		return nil // Can't determine branch — exit quietly
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "main" || branch == "master" || branch == "HEAD" {
		return nil // On default branch — nothing to submit
	}

	pending, reason, err := polecatStopPendingWork(cloneDir, branch)
	if err != nil || !pending {
		return nil // Can't check, or no work to submit — don't block session stop
	}

	// Claude Code fires Stop at the end of every turn, including a turn that
	// ends to wait for a background task (e.g. a long verify run). Submitting
	// then pushes unverified work and kills the session mid-gate.
	if polecatHasBackgroundShells() {
		return nil
	}

	// Polecat has pending work! Run gt done as a safety net.
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "⚠️  Polecat %s has pending work on branch %s (%s)\n", polecatName, branch, reason)
	fmt.Fprintf(os.Stderr, "   Auto-running gt done as safety net...\n")
	fmt.Fprintf(os.Stderr, "\n")

	// Find gt binary path
	gtBin, err := os.Executable()
	if err != nil {
		gtBin = "gt"
	}

	// Run gt done in the polecat's worktree context
	doneCmd := exec.Command(gtBin, "done")
	doneCmd.Dir = cloneDir
	doneCmd.Stdout = os.Stdout
	doneCmd.Stderr = os.Stderr
	// Inherit environment (GT_POLECAT, GT_RIG, etc. are already set)
	doneCmd.Env = os.Environ()

	if err := doneCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Auto gt done failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Witness will handle cleanup.\n")
		// Don't return error — don't block session stop
		return nil
	}

	return nil
}

func polecatStopPendingWork(cloneDir, branch string) (bool, string, error) {
	g := git.NewGit(cloneDir)
	workStatus, err := g.CheckUncommittedWork()
	if err != nil {
		return false, "", err
	}

	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
		return true, fmt.Sprintf("%d non-runtime dirty file(s)", len(workStatus.NonRuntimePaths())), nil
	}
	if workStatus.StashCount > 0 {
		return true, fmt.Sprintf("%d branch stash(es)", workStatus.StashCount), nil
	}

	targetStatus, err := g.BranchTargetStatus(branch, "origin", nil)
	if err != nil {
		return false, "", err
	}
	if !targetStatus.Preserved && targetStatus.UnpreservedPatchCount > 0 {
		return true, fmt.Sprintf("%d unsubmitted commit(s)", targetStatus.UnpreservedPatchCount), nil
	}

	return false, "", nil
}

type stopCheckProc struct {
	ppid int
	comm string
}

// polecatHasBackgroundShells reports whether the agent process that ran this
// Stop hook still has shell children outside the hook's own process chain —
// background commands the polecat ended its turn to wait on.
func polecatHasBackgroundShells() bool {
	out, err := exec.Command("ps", "-eo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return false
	}
	return agentHasBackgroundShells(parseStopCheckPs(string(out)), os.Getpid())
}

func parseStopCheckPs(out string) map[int]stopCheckProc {
	procs := make(map[int]stopCheckProc)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		procs[pid] = stopCheckProc{ppid: ppid, comm: strings.Join(fields[2:], " ")}
	}
	return procs
}

func stopCheckIsShell(comm string) bool {
	switch strings.TrimPrefix(filepath.Base(comm), "-") {
	case "sh", "bash", "zsh", "dash", "fish":
		return true
	}
	return false
}

// agentHasBackgroundShells walks up from self past the hook's gt and shell
// wrappers to the agent process, then looks for shell children of the agent
// that are not part of the hook's own chain.
func agentHasBackgroundShells(procs map[int]stopCheckProc, self int) bool {
	chain := map[int]bool{self: true}
	agent := 0
	for pid := procs[self].ppid; pid > 1; pid = procs[pid].ppid {
		p, ok := procs[pid]
		if !ok {
			return false
		}
		chain[pid] = true
		if base := filepath.Base(p.comm); base != "gt" && !stopCheckIsShell(p.comm) {
			agent = pid
			break
		}
	}
	if agent == 0 {
		return false
	}
	for pid, p := range procs {
		if p.ppid == agent && !chain[pid] && stopCheckIsShell(p.comm) {
			return true
		}
	}
	return false
}
