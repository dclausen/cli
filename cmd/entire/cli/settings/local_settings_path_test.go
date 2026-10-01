package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// worktreePair returns a main worktree with one commit and a linked worktree
// of it. Neither has .entire/settings.local.json.
func worktreePair(t *testing.T) (mainRoot, linked string) {
	t.Helper()
	mainRoot = t.TempDir()
	testutil.InitRepo(t, mainRoot)
	require.NoError(t, os.MkdirAll(filepath.Join(mainRoot, ".entire"), 0o755))
	// Mirror the shipped .entire/.gitignore: the local file is never committed.
	require.NoError(t, os.WriteFile(filepath.Join(mainRoot, ".entire", ".gitignore"),
		[]byte("settings.local.json\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mainRoot, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, mainRoot, "add", ".")
	testutil.RunGit(t, mainRoot, "commit", "-q", "-m", "init")
	linked = filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainRoot, "worktree", "add", "-q", "-b", "feature", linked)

	var err error
	mainRoot, err = filepath.EvalSymlinks(mainRoot)
	require.NoError(t, err)
	linked, err = filepath.EvalSymlinks(linked)
	require.NoError(t, err)
	return mainRoot, linked
}

// A linked worktree is created without the gitignored local file, so every
// developer-only setting silently stopped applying in it (an external agent's
// hooks failed with "unknown agent" and its commits linked to another
// worktree's session).
func TestLocalSettingsPathIn(t *testing.T) {
	t.Parallel()

	t.Run("linked worktree without its own file uses the main worktree's", func(t *testing.T) {
		t.Parallel()
		mainRoot, linked := worktreePair(t)
		writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)

		path, inherited := localSettingsPathIn(linked)
		assert.Equal(t, filepath.Join(mainRoot, EntireSettingsLocalFile), path)
		assert.True(t, inherited)
	})

	t.Run("linked worktree with its own file keeps it", func(t *testing.T) {
		t.Parallel()
		mainRoot, linked := worktreePair(t)
		writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)
		writeSettingsFile(t, filepath.Join(linked, EntireSettingsLocalFile), `{"log_level":"debug"}`)

		path, inherited := localSettingsPathIn(linked)
		assert.Equal(t, filepath.Join(linked, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})

	t.Run("neither has one: the worktree's own path, so a writer creates it there", func(t *testing.T) {
		t.Parallel()
		_, linked := worktreePair(t)

		path, inherited := localSettingsPathIn(linked)
		assert.Equal(t, filepath.Join(linked, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})

	t.Run("the main worktree uses its own", func(t *testing.T) {
		t.Parallel()
		mainRoot, _ := worktreePair(t)

		path, inherited := localSettingsPathIn(mainRoot)
		assert.Equal(t, filepath.Join(mainRoot, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})

	t.Run("worktree of a bare repository has no main worktree to inherit from", func(t *testing.T) {
		t.Parallel()
		src, _ := worktreePair(t)
		bare := filepath.Join(t.TempDir(), "bare.git")
		testutil.RunGit(t, src, "clone", "-q", "--bare", src, bare)
		linked := filepath.Join(t.TempDir(), "wt")
		testutil.RunGit(t, bare, "worktree", "add", "-q", "-b", "other", linked)
		linked, err := filepath.EvalSymlinks(linked)
		require.NoError(t, err)

		path, inherited := localSettingsPathIn(linked)
		assert.Equal(t, filepath.Join(linked, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})

	t.Run("not a repository: own path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		path, inherited := localSettingsPathIn(dir)
		assert.Equal(t, filepath.Join(dir, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})
}

// Not parallel: t.Chdir.
func TestLocalSettingsPath_FromLinkedWorktreeCwd(t *testing.T) {
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)
	t.Chdir(linked)

	path, inherited, err := LocalSettingsPath(t.Context())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mainRoot, EntireSettingsLocalFile), path)
	assert.True(t, inherited)
}

func TestLocalSettingsPath_FromExplicitWorktreeRoot(t *testing.T) {
	t.Parallel()
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)

	path, inherited, err := LocalSettingsPath(WithWorktreeRoot(t.Context(), linked))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mainRoot, EntireSettingsLocalFile), path)
	assert.True(t, inherited)
}
