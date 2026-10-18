package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	nudgePollerIntervalFlag string
	nudgePollerIdleFlag     string
)

func init() {
	rootCmd.AddCommand(nudgePollerCmd)
	nudgePollerCmd.Flags().StringVar(&nudgePollerIntervalFlag, "interval", nudge.DefaultPollInterval, "Poll interval (e.g., 10s, 30s)")
	nudgePollerCmd.Flags().StringVar(&nudgePollerIdleFlag, "idle-timeout", nudge.DefaultIdleTimeout, "How long to wait for agent idle before skipping")
}

var nudgePollerCmd = &cobra.Command{
	Use:    "nudge-poller <session>",
	Short:  "Background nudge queue poller for non-Claude agents",
	Hidden: true, // Internal command — launched by crew manager, not by users.
	Long: `Polls the nudge queue for a tmux session and drains it when the agent
is idle. This is the background equivalent of Claude's UserPromptSubmit hook
drain — it ensures queued nudges are delivered to agents that lack
turn-boundary hooks (Gemini, Codex, Cursor, etc.).

This command runs as a long-lived background process. It exits when:
  - The target tmux session dies
  - It receives SIGTERM (from StopPoller or session teardown)
  - The poll loop encounters an unrecoverable error

Normally launched automatically by 'gt crew start' for non-Claude agents.
Not intended for direct user invocation.`,
	Args: cobra.ExactArgs(1),
	RunE: runNudgePoller,
}

func runNudgePoller(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	pollInterval, err := time.ParseDuration(nudgePollerIntervalFlag)
	if err != nil {
		return fmt.Errorf("invalid --interval: %w", err)
	}

	idleTimeout, err := time.ParseDuration(nudgePollerIdleFlag)
	if err != nil {
		return fmt.Errorf("invalid --idle-timeout: %w", err)
	}

	t := tmux.NewTmux()

	// Verify session exists before starting the loop.
	if exists, _ := t.HasSession(sessionName); !exists {
		return fmt.Errorf("session %q not found", sessionName)
	}

	// One poller per session. StartPoller's PID-file check is not atomic, so
	// two concurrent `gt nudge` calls (or a manager restart racing a nudge)
	// can each launch a poller. Two pollers type into the same TUI on every
	// interval, doubling the turns the agent spends on nudges. Hold an
	// exclusive flock for the poller's lifetime and exit if another holds it.
	unlock, acquired, lockErr := nudge.AcquirePollerLock(townRoot, sessionName)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "nudge-poller: lock error for %s: %v (continuing)\n", sessionName, lockErr)
	} else if !acquired {
		return nil // another poller already serves this session
	} else {
		defer unlock()
	}

	// Resolve nudge options once at startup: if the target agent uses Escape
	// as cancel (e.g., Gemini CLI), skip the Escape keystroke during delivery
	// to avoid canceling in-flight generation. (GH#gt-wasn)
	nudgeOpts := tmux.NudgeOpts{}
	agentName := ""
	hasPromptDetection := false
	if name, err := t.GetEnvironment(sessionName, "GT_AGENT"); err == nil && name != "" {
		agentName = name
		if preset := config.GetAgentPresetByName(agentName); preset != nil {
			hasPromptDetection = preset.ReadyPromptPrefix != ""
			if preset.EscapeCancelsRequest {
				nudgeOpts.SkipEscape = true
			}
		}
	}

	// Set up signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			return nil // graceful shutdown

		case <-ticker.C:
			// Check if session still exists.
			if exists, _ := t.HasSession(sessionName); !exists {
				return nil // session gone, exit
			}

			// Check if there are queued nudges.
			if n, _ := nudge.Pending(townRoot, sessionName); n == 0 {
				continue
			}

			// For runtimes with prompt detection, defer delivery until the session
			// is actually idle. Runtimes without prompt detection preserve the old
			// best-effort behavior and drain on the poll interval.
			waitErr := t.WaitForIdle(sessionName, idleTimeout)
			if shouldSkipDrainUntilIdle(hasPromptDetection, waitErr) {
				continue
			}

			// Drain and inject.
			drained, err := nudge.Drain(townRoot, sessionName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "nudge-poller: drain error for %s: %v\n", sessionName, err)
				continue
			}
			if len(drained) == 0 {
				continue // someone else drained it
			}
			// Drop nudges this session already received within the dedupe
			// window (another drain path, a requeue, or a repeating sender).
			drained = nudge.DropDuplicates(townRoot, sessionName, drained)
			if len(drained) == 0 {
				continue
			}

			formatted := nudge.FormatForInjection(drained)
			err = t.NudgeSessionWithOpts(sessionName, formatted, nudgeOpts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "nudge-poller: injection error for %s: %v\n", sessionName, err)
			}
			settleDrainedDelivery(townRoot, sessionName, "nudge-poller", drained, err)
		}
	}
}

// nudgeTextTyped reports whether a delivery error (or success) means the
// nudge text reached the target pane. nil and ErrSubmitNotVerified both mean
// the text was typed and Enter was sent; TUIs such as Grok queue input while
// a turn runs, so the submit probe cannot see it leave the composer. Any other
// error happened before typing (lock timeout, session gone, send-keys failed).
func nudgeTextTyped(err error) bool {
	return err == nil || errors.Is(err, tmux.ErrSubmitNotVerified)
}

// settleDrainedDelivery finishes a drain-and-inject cycle. If the text was
// typed, the batch is recorded in the delivery ledger and never requeued:
// requeueing after an unverified submit retyped the same nudges every poll
// interval (hundreds of copies queued in the agent). Only a delivery that
// failed before typing is requeued for a later attempt.
func settleDrainedDelivery(townRoot, sessionName, source string, drained []nudge.QueuedNudge, err error) {
	if nudgeTextTyped(err) {
		nudge.RecordDeliveredNudges(townRoot, sessionName, drained)
		return
	}
	requeueDrainedNudges(townRoot, sessionName, source, drained)
}

func shouldSkipDrainUntilIdle(hasPromptDetection bool, waitErr error) bool {
	return hasPromptDetection && waitErr != nil
}
