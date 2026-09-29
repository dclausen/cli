//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// freshRepoEnv builds a repo with an initial commit but WITHOUT Entire enabled,
// so `entire enable` runs its real first-time flow.
func freshRepoEnv(t *testing.T) *TestEnv {
	t.Helper()
	env := NewTestEnv(t)
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	return env
}

// History import is withdrawn until it is redesigned. --import-history stays
// accepted so scripts that pass it keep working: a first-time enable with
// discoverable history must complete, say the flag is deprecated, and write no
// checkpoints.
func TestEnableImportHistoryFlagIsDeprecatedNoOp(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := freshRepoEnv(t)
		env.CheckpointStore = backend
		require.NoError(t, os.WriteFile(filepath.Join(env.ClaudeProjectDir, "sess1.jsonl"), []byte(
			`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`+"\n"), 0o644))

		out := env.RunCLI("enable", "--agent", agentClaudeCode, "--import-history", "--telemetry=false")
		require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)
		require.Contains(t, out, "--import-history has been deprecated", "the flag should say it is deprecated; got: %s", out)
		if env.usingGitRefs() {
			require.False(t, env.CheckpointsPresentLocally(), "a deprecated --import-history must not import history")
		} else {
			// enable creates the v1 branch itself, so check its tree for checkpoints.
			tree := gitOutput(t, env.RepoDir, "ls-tree", "-r", "--name-only", paths.MetadataBranchName)
			require.NotContains(t, tree, "metadata.json", "a deprecated --import-history must not import history")
		}
	})
}
