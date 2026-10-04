package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

// TestWriteCheckpoint_SnapshotsTheWholeDirtyWorktree pins that every shadow
// snapshot captures the worktree as it is, whatever wrote it: a later snapshot
// with no file list still picks up a new untracked file and a changed tracked
// one, resets a file that was reverted since the previous snapshot, and
// reports which files changed since that snapshot.
func TestWriteCheckpoint_SnapshotsTheWholeDirtyWorktree(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	repo, dir := setupTestRepo(t)
	store := newEphemeralStore(repo, DefaultV1Refs())
	t.Chdir(dir)
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	base := head.Hash().String()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	snapshot := func(first bool) WriteEphemeralResult {
		t.Helper()
		result, err := store.Write(context.Background(), Step{
			SessionID:         "full-snapshot-session",
			BaseCommit:        base,
			CommitMessage:     "snapshot",
			AuthorName:        "Test",
			AuthorEmail:       "test@test.com",
			IsFirstCheckpoint: first,
		})
		if err != nil {
			t.Fatalf("write checkpoint: %v", err)
		}
		return result
	}
	fileAt := func(commit plumbing.Hash, name string) string {
		t.Helper()
		c, err := repo.CommitObject(commit)
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		f, err := c.File(name)
		if err != nil {
			return "<absent>"
		}
		content, err := f.Contents()
		if err != nil {
			t.Fatalf("contents %s: %v", name, err)
		}
		return content
	}

	write("file1.txt", "agent changed file1\n")
	first := snapshot(true)
	if got := fileAt(first.CommitHash, "file1.txt"); got != "agent changed file1\n" {
		t.Fatalf("first snapshot file1.txt = %q", got)
	}

	// A later snapshot is asked for no files at all: a shell command wrote
	// these, so no transcript names them.
	write("file1.txt", "initial content of file1.txt")
	write("file2.txt", "shell rewrote file2\n")
	write("shell.out", "written by a shell command\n")
	second := snapshot(false)
	if got := fileAt(second.CommitHash, "file2.txt"); got != "shell rewrote file2\n" {
		t.Errorf("file2.txt = %q, want the worktree content", got)
	}
	if got := fileAt(second.CommitHash, "shell.out"); got != "written by a shell command\n" {
		t.Errorf("shell.out = %q, want the untracked file captured", got)
	}
	if got := fileAt(second.CommitHash, "file1.txt"); got != "initial content of file1.txt" {
		t.Errorf("file1.txt = %q, want it reset to its base content after the revert", got)
	}
	changed := slices.Clone(second.ChangedFiles)
	slices.Sort(changed)
	if want := []string{"file1.txt", "file2.txt", "shell.out"}; !slices.Equal(changed, want) {
		t.Errorf("ChangedFiles = %v, want %v", changed, want)
	}
}
