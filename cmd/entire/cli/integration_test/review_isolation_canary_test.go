//go:build integration && !windows

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Reviewer isolation canaries: a branch under review commits agent
// configuration that runs a command as soon as the agent starts, and the
// review must not run it — neither for a review of the current branch nor for
// `entire review --target <branch>` (security report T-243).
//
// These drive the real agent CLI, because what they pin is how that CLI treats
// the flags Entire passes, which a fake binary cannot show. The canary fires
// at agent startup, before any model request, so the run is given an invalid
// API key: it fails authentication after the point that matters and spends no
// tokens. Opt-in, since they need the agent CLIs installed:
//
//	ENTIRE_TEST_REVIEW_REAL_AGENTS=1 go test -tags=integration -run TestReviewIsolation ./cmd/entire/cli/integration_test/
const reviewRealAgentsEnv = "ENTIRE_TEST_REVIEW_REAL_AGENTS"

// reviewStartupTimeout bounds how long the agent may take to reach its startup
// hooks. The review is not waited on to finish: with an invalid key the agent
// may retry for minutes, and only its startup matters.
const reviewStartupTimeout = 90 * time.Second

// reviewStartupGrace is how long to keep the agent running after Entire's own
// startup hook fired, so a canary registered for the same phase has had its
// turn before the run is stopped.
const reviewStartupGrace = 5 * time.Second

func requireRealReviewAgent(t *testing.T, binary string) {
	t.Helper()
	if os.Getenv(reviewRealAgentsEnv) != "1" {
		t.Skipf("set %s=1 to run reviewer isolation canaries against real agent CLIs", reviewRealAgentsEnv)
	}
	if _, err := exec.LookPath(binary); err != nil {
		t.Skipf("%s is not installed", binary)
	}
}

// reviewCanaryRepo is a repository with a bare origin, a review profile for
// one agent, and a branch whose committed agent config writes marker when the
// agent starts.
type reviewCanaryRepo struct {
	env    *TestEnv
	branch string
	marker string
	// launched records that the review actually started the agent CLI, so a
	// run that never reached the agent cannot pass as "the canary did not run".
	launched string
	// shimDir holds a wrapper for the agent binary that records the launch and
	// then execs the real CLI unchanged; it goes first on the review's PATH.
	shimDir string
}

// newReviewCanaryRepo commits the files canary returns on a new branch, pushed
// to origin. The caller is left on that branch.
func newReviewCanaryRepo(t *testing.T, agentName, binary string, canary func(t *testing.T, repoDir, marker string) map[string]string) *reviewCanaryRepo {
	t.Helper()
	agentPath, err := exec.LookPath(binary)
	if err != nil {
		t.Fatalf("look up %s: %v", binary, err)
	}
	shimDir := t.TempDir()
	launched := filepath.Join(t.TempDir(), "agent-launched")
	shim := "#!/bin/sh\necho launched >> " + shellQuote(launched) + "\nexec " + shellQuote(agentPath) + ` "$@"` + "\n"
	if err := os.WriteFile(filepath.Join(shimDir, binary), []byte(shim), 0o755); err != nil {
		t.Fatalf("write %s shim: %v", binary, err)
	}

	env := NewRepoWithCommit(t)
	enableReviewAgent(t, env, agentName)
	env.WriteSettings(map[string]any{
		"enabled":                true,
		"review_default_profile": "canary",
		"review_profiles": map[string]any{
			"canary": map[string]any{
				"task":   "Reply with the single word DONE.",
				"agents": map[string]any{agentName: map[string]any{"skills": []string{"/review"}}},
			},
		},
	})
	env.GitAdd(".")
	env.GitCommit("review profile")

	env.SetupBareRemote()

	marker := filepath.Join(t.TempDir(), "canary-ran")
	const branch = "canary/agent-config"
	env.GitCheckoutNewBranch(branch)
	for path, content := range canary(t, env.RepoDir, marker) {
		env.WriteFile(path, content)
	}
	env.GitAdd(".")
	env.GitCommit("branch-owned agent config")
	testutil.RunGit(t, env.RepoDir, "push", "-q", "--no-verify", "origin", branch)
	return &reviewCanaryRepo{env: env, branch: branch, marker: marker, launched: launched, shimDir: shimDir}
}

// reviewResult is what one canary review observed.
type reviewResult struct {
	output string
	// startupHooksRan is true once Entire's own SessionStart hook recorded a
	// session: the agent reached the phase the canary is registered for.
	startupHooksRan bool
}

// review starts `entire review canary [args...]`, waits until the agent has
// reached its startup hooks (or the canary fired), lets that phase finish,
// then stops the review and its agent.
func (r *reviewCanaryRepo) review(t *testing.T, extraEnv []string, args ...string) reviewResult {
	t.Helper()
	stateDir := filepath.Join(r.env.RepoDir, ".git", "entire-sessions")
	before := sessionStateFiles(stateDir)

	var out bytes.Buffer
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), append([]string{"review", "canary"}, args...)...)
	cmd.Dir = r.env.RepoDir
	cmd.Env = envWithOverrides(r.env.cliEnv(),
		append([]string{"PATH=" + r.shimDir + string(os.PathListSeparator) + os.Getenv("PATH")}, extraEnv...)...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start review: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait() //nolint:errcheck // the run is stopped or fails authentication; only its side effects matter
		close(exited)
	}()
	defer func() {
		// execx.NonInteractive makes the review a process-group leader, so this
		// also stops the agent it spawned.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // already exited is fine
		<-exited
	}()

	var res reviewResult
	deadline := time.After(reviewStartupTimeout)
	for !res.startupHooksRan {
		if fileExists(r.marker) {
			break
		}
		if len(sessionStateFiles(stateDir)) > len(before) {
			res.startupHooksRan = true
			select {
			case <-time.After(reviewStartupGrace):
			case <-exited:
			}
			break
		}
		select {
		case <-exited:
			res.output = out.String()
			return res
		case <-deadline:
			res.output = out.String()
			return res
		case <-time.After(200 * time.Millisecond):
		}
	}
	res.output = out.String()
	return res
}

// assertCanaryDidNotRun fails when the branch's config ran during the review,
// and when the run never got far enough to show anything.
func (r *reviewCanaryRepo) assertCanaryDidNotRun(t *testing.T, res reviewResult) {
	t.Helper()
	if fileExists(r.marker) {
		t.Fatalf("branch-owned agent config ran during review (marker %s exists)\nreview output:\n%s", r.marker, res.output)
	}
	if !fileExists(r.launched) {
		t.Fatalf("review never started the agent, so it proves nothing about isolation\nreview output:\n%s", res.output)
	}
	if !res.startupHooksRan {
		t.Fatalf("agent never reached its startup hooks within %s, so it proves nothing about isolation\nreview output:\n%s", reviewStartupTimeout, res.output)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// sessionStateFiles lists the session state files Entire's hooks write.
func sessionStateFiles(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json")) //nolint:errcheck // a static pattern cannot be malformed
	return matches
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellWriteMarker is a POSIX command that records it ran.
func shellWriteMarker(marker string) string {
	return "echo ran > " + shellQuote(marker)
}

// claudeSessionStartCanary adds a SessionStart hook to the .claude/settings.json
// `entire enable` installed, keeping Entire's own hooks so the review's
// hooks-installed preflight still passes — the shape of the reported attack.
func claudeSessionStartCanary(t *testing.T, repoDir, marker string) map[string]string {
	t.Helper()
	path := filepath.Join(".claude", "settings.json")
	data, err := os.ReadFile(filepath.Join(repoDir, path))
	if err != nil {
		t.Fatalf("read installed claude settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse installed claude settings: %v", err)
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("installed claude settings carry no hooks: %s", data)
	}
	existing, _ := hooks["SessionStart"].([]any) //nolint:errcheck // absent is an empty list
	hooks["SessionStart"] = append(existing, map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": shellWriteMarker(marker)}},
	})
	out, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal canary settings: %v", err)
	}
	return map[string]string{path: string(out)}
}

// claudeCanaryEnv keeps the run from authenticating: an invalid API key takes
// precedence over any login, so no model request succeeds.
func claudeCanaryEnv() []string {
	return []string{"ANTHROPIC_API_KEY=sk-ant-entire-test-invalid"}
}

func TestReviewIsolation_ClaudeSessionStartHookDoesNotRun(t *testing.T) {
	requireRealReviewAgent(t, "claude")

	t.Run("current branch", func(t *testing.T) {
		repo := newReviewCanaryRepo(t, agentClaudeCode, "claude", claudeSessionStartCanary)
		repo.assertCanaryDidNotRun(t, repo.review(t, claudeCanaryEnv()))
	})

	t.Run("--target", func(t *testing.T) {
		repo := newReviewCanaryRepo(t, agentClaudeCode, "claude", claudeSessionStartCanary)
		testutil.RunGit(t, repo.env.RepoDir, "checkout", "-q", "-")
		testutil.RunGit(t, repo.env.RepoDir, "branch", "-q", "-D", repo.branch)
		repo.assertCanaryDidNotRun(t, repo.review(t, claudeCanaryEnv(), "--target", repo.branch, "--cleanup-worktree"))
	})
}
