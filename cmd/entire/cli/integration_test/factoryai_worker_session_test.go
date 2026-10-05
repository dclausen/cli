//go:build integration

package integration

import (
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Factory AI Droid dispatches Workers as detached sessions: the parent's
// PostToolUse hook fires when the Worker is launched, before it has touched the
// worktree, and the Worker then runs its own SessionStart/UserPromptSubmit/Stop
// cycle. These tests pin the resulting attribution — a Worker's turn must land
// as a task record on the parent, not as an unrelated top-level session.

// TestFactoryDroidWorkerSessionBecomesTaskCheckpoint covers the regression that
// broke the E2E TestFactoryTaskRecordExistsBeforeCommit: a Worker's work reached
// the shadow branch, but under its own session ID, so the parent had nothing
// attributing it to the task.
func TestFactoryDroidWorkerSessionBecomesTaskCheckpoint(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// The parent turn: the user asks for a Worker, and Droid dispatches one.
	parent := env.NewFactoryDroidSession()
	parent.CreateDroidTranscript("Run a Worker that writes docs/summary.md", nil)
	if err := env.SimulateFactoryDroidUserPromptSubmit(parent.ID); err != nil {
		t.Fatalf("parent UserPromptSubmit failed: %v", err)
	}

	// The Worker turn: its own session, writing the file the parent asked for.
	const toolUseID = "toolu_worker_01"
	worker := env.NewFactoryDroidSession()
	if err := env.SimulateFactoryDroidUserPromptSubmit(worker.ID); err != nil {
		t.Fatalf("worker UserPromptSubmit failed: %v", err)
	}
	env.WriteFile("docs/summary.md", "A short summary.\n")
	worker.CreateDroidTranscript("# Task Tool Invocation", []FileChange{
		{Path: "docs/summary.md", Content: "A short summary.\n"},
	})
	worker.MarkAsWorkerSession(parent.ID, toolUseID, "worker: Summarize the repo")

	if err := env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath); err != nil {
		t.Fatalf("worker Stop failed: %v", err)
	}

	// The work belongs to the parent's task, not to the Worker's own session:
	// a COMPLETED task record on the PARENT, keyed by the launch tool use,
	// carrying the Worker's files and declaring its transcript path for the
	// materializer (create-if-missing parent state is part of the guarantee).
	state, err := env.GetSessionState(parent.ID)
	if err != nil {
		t.Fatalf("GetSessionState(parent) failed: %v", err)
	}
	if state == nil {
		t.Fatal("parent session state must exist after the Worker's turn (create-if-missing guarantee)")
	}
	rec := state.FindTaskRecord(toolUseID)
	if rec == nil || rec.CompletedAt.IsZero() {
		t.Fatalf("expected a completed task record on the parent for %s, got %+v", toolUseID, rec)
	}
	if rec.AgentID != worker.ID {
		t.Errorf("the Worker's session ID must double as the record's agent ID: got %q, want %q", rec.AgentID, worker.ID)
	}
	if rec.DeclaredTranscriptPath != worker.TranscriptPath {
		t.Errorf("the record must declare the Worker's transcript path: got %q, want %q",
			rec.DeclaredTranscriptPath, worker.TranscriptPath)
	}
	if !containsFile(rec.Files, "docs/summary.md") {
		t.Errorf("the record must carry the Worker's files, got %v", rec.Files)
	}
	if !containsFile(state.FilesTouched, "docs/summary.md") {
		t.Errorf("the Worker's files must merge into the parent's FilesTouched, got %v", state.FilesTouched)
	}

	// A shadow write for the Worker session would misattribute the work to a
	// session the user never drove — the exact shape of the regression. The
	// Worker's stop snapshots the worktree for the parent only.
	for _, branch := range shadowBranches(env) {
		if env.FileExistsInBranch(branch, ".entire/metadata/"+worker.ID+"/full.jsonl") {
			t.Errorf("a Worker's turn must write a task record, not shadow data for its own session: %s", branch)
		}
	}

	// Multi-turn Workers upsert into the SAME record: a second turn must MERGE
	// its files with turn 1's (not overwrite them) and re-declare the
	// transcript path, with the record staying completed throughout.
	t.Run("second worker turn merges files", func(t *testing.T) {
		t.Parallel()
		if err := env.SimulateFactoryDroidUserPromptSubmit(worker.ID); err != nil {
			t.Fatalf("worker second UserPromptSubmit failed: %v", err)
		}
		env.WriteFile("docs/details.md", "More detail.\n")
		// Turn 2's transcript names ONLY the new file, so turn 1's file can
		// reappear on the record only via the upsert's merge.
		worker.CreateDroidTranscript("# Task Tool Invocation", []FileChange{
			{Path: "docs/details.md", Content: "More detail.\n"},
		})
		worker.MarkAsWorkerSession(parent.ID, toolUseID, "worker: Summarize the repo")
		if err := env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath); err != nil {
			t.Fatalf("worker second Stop failed: %v", err)
		}

		state, err := env.GetSessionState(parent.ID)
		if err != nil {
			t.Fatalf("GetSessionState(parent) failed: %v", err)
		}
		rec := state.FindTaskRecord(toolUseID)
		if rec == nil || rec.CompletedAt.IsZero() {
			t.Fatalf("the record must stay completed across Worker turns, got %+v", rec)
		}
		if !containsFile(rec.Files, "docs/summary.md") || !containsFile(rec.Files, "docs/details.md") {
			t.Errorf("a second Worker turn must merge files with turn 1's, got %v", rec.Files)
		}
		if !containsFile(state.FilesTouched, "docs/summary.md") || !containsFile(state.FilesTouched, "docs/details.md") {
			t.Errorf("both turns' files must be in the parent's FilesTouched, got %v", state.FilesTouched)
		}
		if rec.DeclaredTranscriptPath != worker.TranscriptPath {
			t.Errorf("turn 2 must re-declare the Worker transcript path, got %q", rec.DeclaredTranscriptPath)
		}
	})
}

// TestFactoryDroidWorkerFileCommittedAfterParentSnapshot covers the E2E
// TestFactoryCommittedCheckpointExcludesPreExistingUntrackedFiles: the
// parent's turn end snapshots the worktree (a human's untracked file differs
// from HEAD) before its Worker writes, so a shadow branch exists without the
// Worker's file. The Worker's stop must snapshot the parent's worktree;
// otherwise the commit's content overlap finds the Worker's new file missing
// from the shadow tree and nothing condenses into the commit.
func TestFactoryDroidWorkerFileCommittedAfterParentSnapshot(t *testing.T) {
	t.Parallel()

	const (
		workerFile    = "docs/worker.md"
		workerContent = "Findings.\n\n- one\n- two\n- three\n"
	)
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.WriteFile("docs/human-note.md", "human-owned sentinel\n")

	parent := env.NewFactoryDroidSession()
	parent.CreateDroidTranscript("Run a Worker that writes "+workerFile, nil)
	if err := env.SimulateFactoryDroidUserPromptSubmit(parent.ID); err != nil {
		t.Fatalf("parent UserPromptSubmit failed: %v", err)
	}
	if err := env.SimulateFactoryDroidStop(parent.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("parent Stop failed: %v", err)
	}

	const toolUseID = "toolu_worker_after_parent"
	worker := env.NewFactoryDroidSession()
	if err := env.SimulateFactoryDroidUserPromptSubmit(worker.ID); err != nil {
		t.Fatalf("worker UserPromptSubmit failed: %v", err)
	}
	env.WriteFile(workerFile, workerContent)
	worker.CreateDroidTranscript("# Task Tool Invocation", []FileChange{{Path: workerFile, Content: workerContent}})
	worker.MarkAsWorkerSession(parent.ID, toolUseID, "worker: write findings")
	if err := env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath); err != nil {
		t.Fatalf("worker Stop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add worker findings", workerFile)

	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("commit should carry an Entire-Checkpoint trailer")
	}
	content, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	if !ok {
		t.Fatalf("checkpoint %s was not condensed: no session metadata.json", checkpointID)
	}
	var metadata checkpoint.Metadata
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("parse session metadata: %v", err)
	}
	if metadata.SessionID != parent.ID {
		t.Errorf("checkpoint session = %q, want the parent %q", metadata.SessionID, parent.ID)
	}
	if !containsFile(metadata.FilesTouched, workerFile) {
		t.Errorf("files_touched = %v, want it to include %s", metadata.FilesTouched, workerFile)
	}
	if containsFile(metadata.FilesTouched, "docs/human-note.md") {
		t.Errorf("files_touched = %v, must not include the human's untracked file", metadata.FilesTouched)
	}
}

// TestFactoryDroidTopLevelSessionStillCheckpoints guards the fallback: an
// ordinary Droid session has no calling IDs and must keep its own checkpoint.
func TestFactoryDroidTopLevelSessionStillCheckpoints(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	session := env.NewFactoryDroidSession()
	if err := env.SimulateFactoryDroidUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("UserPromptSubmit failed: %v", err)
	}
	env.WriteFile("feature.go", "package main\n")
	session.CreateDroidTranscript("Add a feature", []FileChange{
		{Path: "feature.go", Content: "package main\n"},
	})

	if err := env.SimulateFactoryDroidStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	shadowBranch := env.GetShadowBranchName()
	sessionCheckpoint := ".entire/metadata/" + session.ID + "/" + paths.TranscriptFileName
	if !env.FileExistsInBranch(shadowBranch, sessionCheckpoint) {
		t.Errorf("expected an ordinary session checkpoint at %s", sessionCheckpoint)
	}

	// It must not be diverted into a task checkpoint under some other session.
	if env.FileExistsInBranch(shadowBranch, ".entire/metadata/"+session.ID+"/tasks") {
		t.Errorf("a top-level session must not be recorded as a task")
	}
}
