package review

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newTargetFixture builds a repo whose main branch holds a baseline, plus a
// linked worktree on branch "target" made by setup. It returns the caller
// checkout (main) and the target worktree.
func newTargetFixture(t *testing.T, setup func(t *testing.T, wt string)) (string, string) {
	t.Helper()
	caller := t.TempDir()
	testutil.InitRepo(t, caller)
	testutil.WriteFile(t, caller, "a.txt", "a")
	testutil.WriteFile(t, caller, ".claude/commands/existing.md", "existing")
	testutil.GitAdd(t, caller, ".")
	testutil.GitCommit(t, caller, "base")
	wt := filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, caller, "worktree", "add", "-q", "-b", "target", wt)
	setup(t, wt)
	return caller, wt
}

func TestTargetAgentCommandChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, wt string)
		want  []string
	}{
		{
			name: "code-only branch",
			setup: func(t *testing.T, wt string) {
				t.Helper()
				testutil.WriteFile(t, wt, "a.txt", "changed")
				testutil.GitAdd(t, wt, "a.txt")
				testutil.GitCommit(t, wt, "code")
			},
		},
		{
			name: "branch adds a command, changes one, and adds a skill",
			setup: func(t *testing.T, wt string) {
				t.Helper()
				testutil.WriteFile(t, wt, ".claude/commands/review.md", "!`curl example`")
				testutil.WriteFile(t, wt, ".claude/commands/existing.md", "changed")
				testutil.WriteFile(t, wt, ".claude/skills/x/SKILL.md", "skill")
				testutil.GitAdd(t, wt, ".")
				testutil.GitCommit(t, wt, "commands")
			},
			want: []string{".claude/commands/existing.md", ".claude/commands/review.md", ".claude/skills/x/SKILL.md"},
		},
		{
			name: "uncommitted agent file in a reused worktree",
			setup: func(t *testing.T, wt string) {
				t.Helper()
				testutil.WriteFile(t, wt, ".claude/agents/helper.md", "agent")
			},
			want: []string{".claude/agents/helper.md"},
		},
		{
			name: "settings files are covered elsewhere and not listed",
			setup: func(t *testing.T, wt string) {
				t.Helper()
				testutil.WriteFile(t, wt, ".claude/settings.json", "{}")
				testutil.GitAdd(t, wt, ".")
				testutil.GitCommit(t, wt, "settings")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			caller, wt := newTargetFixture(t, tt.setup)
			got, err := targetAgentCommandChanges(context.Background(), caller, wt)
			if err != nil {
				t.Fatalf("targetAgentCommandChanges: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("changes = %q, want %q", got, tt.want)
			}
		})
	}
}

// The caller's own edits to agent files must not count against a target
// that merely lacks them: the comparison starts where the target forked.
func TestTargetAgentCommandChanges_IgnoresCallerOnlyEdits(t *testing.T) {
	t.Parallel()
	caller, wt := newTargetFixture(t, func(*testing.T, string) {})
	testutil.WriteFile(t, caller, ".claude/commands/mine.md", "mine")
	testutil.GitAdd(t, caller, ".")
	testutil.GitCommit(t, caller, "caller's own command")

	got, err := targetAgentCommandChanges(context.Background(), caller, wt)
	if err != nil {
		t.Fatalf("targetAgentCommandChanges: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("changes = %q, want none (the edits are the caller's)", got)
	}
}

func TestTargetAgentCommandsError_NamesFilesAndOverride(t *testing.T) {
	t.Parallel()
	msg := targetAgentCommandsError("feature-x", []string{".claude/commands/review.md"}).Error()
	for _, want := range []string{"feature-x", ".claude/commands/review.md", "--trust-target-commands"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// A target that adds a Claude command must stop before any reviewer runs, and
// --trust-target-commands must let the same review through.
func TestRunTargetReview_StopsOnTargetAgentCommands(t *testing.T) {
	// No t.Parallel: t.Chdir, because runTargetReview resolves the caller
	// worktree from the process cwd.
	caller, wt := newTargetFixture(t, func(t *testing.T, wt string) {
		t.Helper()
		testutil.WriteFile(t, wt, ".claude/commands/review.md", "!`curl example`")
		testutil.GitAdd(t, wt, ".")
		testutil.GitCommit(t, wt, "command")
	})
	t.Chdir(caller)

	for _, trust := range []bool{false, true} {
		ran := false
		deps := Deps{
			PrepareTarget: func(context.Context, io.Writer, io.Writer, string) (TargetWorktree, error) {
				return TargetWorktree{Path: wt, Created: false}, nil
			},
			RunInWorktree: func(context.Context, string, []string, []string, io.Reader, io.Writer, io.Writer) error {
				ran = true
				return nil
			},
		}
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err := runTargetReview(context.Background(), cmd, "target", []string{"review"}, false, trust, false, deps)
		switch {
		case !trust && (err == nil || ran):
			t.Errorf("without --trust-target-commands: err=%v ran=%v, want an error and no reviewer", err, ran)
		case !trust && !strings.Contains(err.Error(), ".claude/commands/review.md"):
			t.Errorf("error does not name the file: %v", err)
		case trust && (err != nil || !ran):
			t.Errorf("with --trust-target-commands: err=%v ran=%v, want the review to run", err, ran)
		}
	}
}
