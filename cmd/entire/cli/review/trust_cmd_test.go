package review_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// clearAgentCallerEnv removes every variable the trust gate reads as "an agent
// is running this", so the developer's own agent session cannot leak in.
func clearAgentCallerEnv(t *testing.T) {
	t.Helper()
	names := append(agent.CallerSessionEnvVars(), interactive.AgentSubprocessEnvVars()...)
	names = append(names, "CLAUDECODE", "GIT_TERMINAL_PROMPT")
	for _, name := range names {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// setupForeignBranchRepo creates a repo on branch "feature" whose one commit
// above main was authored by someone else, and a claude-code review profile.
func setupForeignBranchRepo(t *testing.T) (reviewer *captureRunConfigReviewer, deps review.Deps, head string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	clearAgentCallerEnv(t)
	setupCmdTestRepo(t)
	runGitCmd(t, "branch", "-M", "main")
	runGitCmd(t, "checkout", "-b", "feature")
	testutil.WriteFile(t, ".", "g.txt", "y")
	testutil.GitAdd(t, ".", "g.txt")
	runGitCmd(t, "commit", "-m", "theirs", "--author", "Mallory <mallory@example.com>")
	head = strings.TrimSpace(runGitCmd(t, "rev-parse", "HEAD"))

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {Skills: []string{"/review"}},
	}); err != nil {
		t.Fatal(err)
	}
	reviewer = &captureRunConfigReviewer{name: "claude-code"}
	deps = review.Deps{
		GetAgentsWithHooksInstalled: func(context.Context) []types.AgentName {
			return []types.AgentName{"claude-code"}
		},
		NewSilentError:          func(err error) error { return err },
		HeadHasReviewCheckpoint: func(context.Context) (bool, string) { return false, "" },
		ReviewerFor: func(name string) reviewtypes.AgentReviewer {
			if name == "claude-code" {
				return reviewer
			}
			return nil
		},
		InspectTrust: func(_ context.Context, source review.TrustSource, agents []string) (review.TrustInventory, error) {
			if source.WorktreeRoot == "" || len(agents) != 1 || agents[0] != "claude-code" {
				t.Errorf("InspectTrust(%+v, %v): want the current worktree and the profile's agent", source, agents)
			}
			return review.TrustInventory{Entries: []review.TrustEntry{
				{Agent: "claude-code", Kind: review.TrustKindHook, Name: "Stop", Command: "npm test", Source: ".claude/settings.json"},
			}}, nil
		},
	}
	return reviewer, deps, head
}

func TestRunReview_ForeignCommitsRefusedWithoutApproval(t *testing.T) {
	reviewer, deps, head := setupForeignBranchRepo(t)

	var out, errOut bytes.Buffer
	cmd := review.NewCommand(deps)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"general"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("review of someone else's commits ran without approval")
	}
	if reviewer.called {
		t.Fatal("reviewer started before approval")
	}
	for _, want := range []string{
		"Not run: this review needs the user's approval.",
		"would run 1 command on this machine",
		"entire review general --trust-target " + head,
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut.String())
		}
	}
	for _, notWant := range []string{"Mallory", "npm test"} {
		if strings.Contains(errOut.String()+out.String(), notWant) {
			t.Errorf("refusal shows author-controlled %q:\n%s", notWant, errOut.String())
		}
	}
}

func TestRunReview_ForeignCommitsRunWithMatchingTrustTarget(t *testing.T) {
	reviewer, deps, head := setupForeignBranchRepo(t)

	var errOut bytes.Buffer
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"general", "--trust-target", head})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("approved review failed: %v\n%s", err, errOut.String())
	}
	if !reviewer.called {
		t.Fatal("reviewer did not start after approval")
	}
	if !strings.Contains(errOut.String(), "Running the review of "+head[:12]+" as approved (1 command).") {
		t.Errorf("stderr missing approval line:\n%s", errOut.String())
	}
}

func TestRunReview_TrustTargetForOtherCommitRefused(t *testing.T) {
	reviewer, deps, _ := setupForeignBranchRepo(t)

	var errOut bytes.Buffer
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"general", "--trust-target", strings.Repeat("0", 40)})
	if err := cmd.Execute(); err == nil {
		t.Fatal("mismatched --trust-target ran the review")
	}
	if reviewer.called {
		t.Fatal("reviewer started on a mismatched --trust-target")
	}
	if !strings.Contains(errOut.String(), "not the approved "+strings.Repeat("0", 40)) {
		t.Errorf("stderr:\n%s", errOut.String())
	}
}

// The re-run inside a target worktree carries the caller's worktree in its
// environment. That variable alone must not skip the gate.
func TestRunReview_TargetChildEnvAloneDoesNotSkipGate(t *testing.T) {
	reviewer, deps, _ := setupForeignBranchRepo(t)
	t.Setenv("ENTIRE_REVIEW_FINDINGS_WORKTREE", t.TempDir())

	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general"})
	if err := cmd.Execute(); err == nil || reviewer.called {
		t.Fatalf("env var alone skipped the gate (err=%v, called=%v)", err, reviewer.called)
	}
}

func TestRunReview_ShowConfigListsWithoutRunning(t *testing.T) {
	reviewer, deps, head := setupForeignBranchRepo(t)

	var out bytes.Buffer
	cmd := review.NewCommand(deps)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general", "--show-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--show-config: %v", err)
	}
	if reviewer.called {
		t.Fatal("--show-config started a reviewer")
	}
	for _, want := range []string{"feature @ " + head[:12] + " by Mallory (1 commit)", "npm test"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunReview_MalformedTrustTargetIsUsageError(t *testing.T) {
	reviewer, deps, _ := setupForeignBranchRepo(t)

	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general", "--trust-target", "yes"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "full commit SHA") || reviewer.called {
		t.Fatalf("err = %v, called = %v", err, reviewer.called)
	}
}

func runGitCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// --base sets the review scope only. Pointing it at the head must not make
// someone else's commits look like an empty, "yours" range.
func TestRunReview_BaseAtHeadDoesNotSkipGate(t *testing.T) {
	reviewer, deps, head := setupForeignBranchRepo(t)

	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general", "--base", head})
	if err := cmd.Execute(); err == nil || reviewer.called {
		t.Fatalf("--base at head skipped the gate (err=%v, called=%v)", err, reviewer.called)
	}
}

// Gate flags are validated before any mode runs, so they are never silently
// ignored by --list, --configure, and the other modes.
func TestRunReview_GateFlagsValidatedForEveryMode(t *testing.T) {
	_, deps, _ := setupForeignBranchRepo(t)
	for _, args := range [][]string{{"--list", "--json"}, {"--list", "--trust-target", "abc"}} {
		cmd := review.NewCommand(deps)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("review %v succeeded; want a flag error", args)
		}
	}
}
