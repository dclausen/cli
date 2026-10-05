package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
)

// Session resolution and the snapshot write are `checkpoint create`'s, and are
// covered by its own tests; what follows is only what sharing adds on top.

// Without a remote there is nowhere to share to. Checked before the checkpoint
// is written, so the user is not left with a checkpoint and no explanation.
func TestResolveShareRemote_NoRemotesIsAnError(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	_, err := resolveShareRemote(context.Background(), "")
	if err == nil {
		t.Fatal("resolveShareRemote() = nil error, want a failure when no remote is configured")
	}
	if !strings.Contains(err.Error(), "--remote") {
		t.Errorf("the error should point at the escape hatch, got: %v", err)
	}
}

// An explicit --remote is honoured without consulting the elected sync remote,
// so a repo with no remotes can still share to one named on the command line.
func TestResolveShareRemote_ExplicitWins(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	got, err := resolveShareRemote(context.Background(), "upstream")
	if err != nil {
		t.Fatalf("resolveShareRemote() error = %v", err)
	}
	if got != "upstream" {
		t.Errorf("resolveShareRemote() = %q, want upstream", got)
	}
}

// The printed line is the whole point of the command: it is what the user
// hands over, so it must be a command the recipient can actually run.
func TestPrintShareResult_PrintsARunnableResumeCommand(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abc123def456")
	root := &cobra.Command{Use: "entire"}
	share := &cobra.Command{Use: "share"}
	root.AddCommand(share)

	var out bytes.Buffer
	printShareResult(&out, share, cpID, strategy.ShareCheckpointResult{Pushed: 1, Counted: true})

	want := "entire session resume " + cpID.String()
	if !strings.Contains(out.String(), want) {
		t.Errorf("share must print %q, got: %s", want, out.String())
	}
	if !strings.Contains(out.String(), "Pushed 1 checkpoint ref(s)") {
		t.Errorf("a counted push should report how many refs went out, got: %s", out.String())
	}
}

// push_sessions=false must not read as a successful share: the checkpoint is
// local, so the resume line has to come with that caveat attached.
func TestPrintShareResult_PushDisabledSaysNothingLeftTheMachine(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printShareResult(&out, nil, id.MustCheckpointID("abc123def456"),
		strategy.ShareCheckpointResult{PushDisabled: true})

	got := out.String()
	if !strings.Contains(got, "disabled in settings") {
		t.Errorf("a disabled push must say so, got: %s", got)
	}
	if strings.Contains(got, "Pushed") {
		t.Errorf("a disabled push must not claim it pushed, got: %s", got)
	}
}

// The git-branch backend pushes a branch, not counted refs, so reporting a
// count there would be a fiction — it says "Pushed." and nothing more.
func TestPrintShareResult_UncountedPushDoesNotInventANumber(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printShareResult(&out, nil, id.MustCheckpointID("abc123def456"),
		strategy.ShareCheckpointResult{Counted: false})

	got := out.String()
	if !strings.Contains(got, "Pushed.") {
		t.Errorf("an uncounted push should still report success, got: %s", got)
	}
	if strings.Contains(got, "ref(s)") {
		t.Errorf("an uncounted push must not print a ref count, got: %s", got)
	}
}

// Unparented commands fall back to the canonical binary name rather than
// printing a resume line nobody can type.
func TestShareRootName_FallsBackWhenUnparented(t *testing.T) {
	t.Parallel()

	if got := shareRootName(nil); got != cmdRoot {
		t.Errorf("shareRootName(nil) = %q, want %q", got, cmdRoot)
	}
	if got := shareRootName(&cobra.Command{Use: "share"}); got != cmdRoot {
		t.Errorf("shareRootName(unparented) = %q, want %q", got, cmdRoot)
	}
}

// The snapshot writer refuses a session with pending file changes. That is a
// decision, not a fault, so share must turn it into something actionable
// rather than surfacing the sentinel's bare condition.
func TestDescribeShareCheckpointError(t *testing.T) {
	t.Parallel()

	t.Run("pending files become commit-first guidance", func(t *testing.T) {
		t.Parallel()
		err := describeShareCheckpointError(strategy.ErrPendingFileChanges)
		if err == nil {
			t.Fatal("describeShareCheckpointError() = nil, want an error")
		}
		if !strings.Contains(err.Error(), "Commit first") {
			t.Errorf("the refusal should say what to do, got: %v", err)
		}
		if !strings.Contains(err.Error(), "attribution") {
			t.Errorf("the refusal should say why the commit's checkpoint is better, got: %v", err)
		}
	})

	t.Run("anything else passes through unchanged", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("disk on fire")
		if err := describeShareCheckpointError(sentinel); !errors.Is(err, sentinel) {
			t.Errorf("describeShareCheckpointError() = %v, want the original error", err)
		}
	})
}
