//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestSubagentCheckpoints_CommittedMidTurn_LeavesNoShadowBranch reproduces the
// orphaned shadow branch behind the e2e failure of
// TestSingleSessionSubagentCommitInTurn.
//
// The subagent writes a file and commits it itself, mid-turn. That commit condenses
// the session and deletes the shadow branch. post-task then fires with nothing left
// to snapshot — the file is already in HEAD — so it must skip the task checkpoint.
// Creating one instead mints a *new* shadow branch after condensation has already
// run, and nothing ever condenses it away: turn-end sees no file modifications and
// skips, so the branch outlives the session.
//
// The trap is that the subagent's transcript still records the Write. Deciding from
// the transcript alone conflates "the subagent wrote this at some point" with "there
// is an uncommitted change here" — see filterToUncommittedFiles, which the turn-end
// path already applies for exactly this reason.
func TestSubagentCheckpoints_CommittedMidTurn_LeavesNoShadowBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("use a subagent to write docs/red.md and commit it", nil)

	const (
		taskToolUseID = "toolu_01CommitInTurn"
		subagentID    = "a0011223344556677"
		editedFile    = "docs/red.md"
	)
	// The subagent's own transcript records the Write; the main transcript does not.
	session.CreateSubagentTranscript(subagentID, []FileChange{{Path: editedFile, Content: "Red is warm.\n"}})

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// The subagent writes the file and commits it itself, still inside the turn.
	env.WriteFile(editedFile, "Red is a warm colour.\n")
	env.GitCommitWithShadowHooksAsAgent("Add red.md", editedFile)

	// Condensation ran on that commit and cleaned up the shadow branch.
	if got := shadowBranches(env); len(got) != 0 {
		t.Fatalf("precondition: shadow branch should be gone after the mid-turn commit, got %v", got)
	}

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	if got := shadowBranches(env); len(got) != 0 {
		t.Errorf("post-task created a shadow branch for already-committed work: %v\n"+
			"nothing will condense it away — turn-end skips when no files changed", got)
	}
}

// TestSubagentCheckpoints_UncommittedWork_StillCheckpoints is the companion guard:
// filtering already-committed paths must not stop a subagent whose work is still
// uncommitted from getting its task checkpoint.
func TestSubagentCheckpoints_UncommittedWork_StillCheckpoints(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("use a subagent to write docs/blue.md", nil)

	const (
		taskToolUseID = "toolu_01UncommittedWork"
		subagentID    = "a7766554433221100"
		editedFile    = "docs/blue.md"
	)
	session.CreateSubagentTranscript(subagentID, []FileChange{{Path: editedFile, Content: "Blue is cool.\n"}})

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Left uncommitted, unlike the test above.
	env.WriteFile(editedFile, "Blue is a cool colour.\n")

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	rec := state.FindTaskRecord(taskToolUseID)
	if rec == nil || rec.CompletedAt.IsZero() || !containsFile(rec.Files, editedFile) {
		t.Errorf("expected a completed task record carrying uncommitted subagent work, got %+v", rec)
	}
}

// shadowBranches returns the per-base-commit shadow branches, excluding the
// permanent committed-checkpoint branch which is not session-scoped.
func shadowBranches(env *TestEnv) []string {
	var out []string
	for _, b := range env.ListBranchesWithPrefix("entire/") {
		if b == paths.MetadataBranchName {
			continue
		}
		out = append(out, b)
	}
	return out
}

// TestSubagentCheckpoints_CommitWhileIdleWithTaskRecord_LinksAndCondensesContent
// drives the real prepare-commit-msg + post-commit hooks for a background
// subagent's commit landing while the parent session is IDLE. On top of #2032's
// slow path the commit already linked via HasTaskContent; what this pins is that
// the link is backed by content — the commit's own condensation materializes the
// task record's transcript-so-far under the checkpoint's tasks/ subtree, so the
// trailer resolves to the subagent's real work rather than dangling. That is
// reachable only because idleWithLiveTaskRecord bypasses
// shouldCondenseWithOverlapCheck's overlap requirement for this record-bearing
// IDLE session, whose FilesTouched carries no evidence tying it to editedFile.
func TestSubagentCheckpoints_CommitWhileIdleWithTaskRecord_LinksAndCondensesContent(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	sess.CreateTranscript("delegate a background task", nil)

	const (
		taskToolUseID = "toolu_01IdleMarkerCommit"
		subagentID    = "d4444555566667777"
		editedFile    = "docs/idlemarker.md"
	)

	// Real Claude Code always sends a transcript_path on UserPromptSubmit; it
	// is what populates the persisted SessionState.TranscriptPath condensation
	// later stores as the parent transcript.
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Background launch: record created while the parent is still ACTIVE
	// (mid-turn).
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      sess.ID,
		TranscriptPath: sess.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
		Background:     true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
	}

	// Turn ends: the parent goes IDLE while the background subagent keeps
	// running. The record survives, still live. This is the shape
	// idleWithLiveTaskRecord exists for: an IDLE session whose background task is
	// still genuinely in flight.
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: %v (state=%v)", err, state)
	}
	if state.Phase != session.PhaseIdle {
		t.Fatalf("expected session to be IDLE after turn-end, got %+v", state)
	}
	if !hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("expected live task record to survive turn-end, state=%+v", state)
	}

	// The subagent does its actual work while the parent sits idle between
	// turns: a realistic transcript (the real Claude Code transcript
	// analyzer, not a stub) plus the resulting file.
	const editedContent = "# Idle marker\n\nWritten by a background subagent while the parent is idle.\n"
	sess.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: editedFile, Content: editedContent},
	})
	env.WriteFile(editedFile, editedContent)

	// The commit lands while the session is IDLE, through the real
	// prepare-commit-msg + post-commit hook chain, with no TTY (agent-mode
	// commit) — the exact shape of the incident.
	env.GitCommitWithShadowHooksAsAgent("Add idle-marker doc", editedFile)

	headHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(headHash)
	if checkpointID == "" {
		t.Fatalf("commit made while idle with a live task record should carry an Entire-Checkpoint trailer")
	}

	// THE content guarantee: the commit's condensation materialized the live
	// record's transcript-so-far into the permanent checkpoint's tasks/
	// subtree, so the trailer points at the subagent's real work.
	storedTranscript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, taskToolUseID, "agent-"+subagentID+".jsonl"))
	if !ok {
		t.Fatalf("subagent transcript not materialized under the checkpoint's tasks/ subtree")
	}
	if !strings.Contains(storedTranscript, editedFile) {
		t.Errorf("materialized subagent transcript does not reference %q: %q", editedFile, storedTranscript)
	}

	// The live record survives condensation: the task is still running, and
	// SubagentStop (not this commit) remains the authoritative completion
	// signal; the next condensation re-materializes it.
	state, err = env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: %v (state=%v)", err, state)
	}
	if !hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("expected live task record for %s to survive condensation, state=%+v", taskToolUseID, state)
	}
}

// TestSubagentCheckpoints_CommitAfterBackgroundTaskCompletes_LinksViaFiles pins
// that a completed task record still gets its work linked once it no longer
// bypasses the overlap check. The background subagent finishes after the
// parent's turn ended, leaving its edit uncommitted; completion merges the edit
// into FilesTouched, and that — not the record's presence — is what links the
// later commit and materializes the subagent's transcript.
func TestSubagentCheckpoints_CommitAfterBackgroundTaskCompletes_LinksViaFiles(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		tty  bool
	}{
		{name: "agent commit", tty: false},
		{name: "terminal commit", tty: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			sess := env.NewSession()
			sess.CreateTranscript("delegate a background task", nil)

			const (
				taskToolUseID = "toolu_01CompletedThenCommit"
				subagentID    = "e4444555566667777"
				editedFile    = "docs/completed.md"
				editedContent = "# Completed\n\nWritten by a background subagent that has since finished.\n"
			)

			if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
			}
			if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
				t.Fatalf("SimulatePreTask failed: %v", err)
			}
			if err := env.SimulatePostTask(PostTaskInput{
				SessionID:      sess.ID,
				TranscriptPath: sess.TranscriptPath,
				ToolUseID:      taskToolUseID,
				AgentID:        subagentID,
				Background:     true,
			}); err != nil {
				t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
			}
			if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateStop failed: %v", err)
			}

			subagentTranscript := sess.CreateSubagentTranscript(subagentID, []FileChange{
				{Path: editedFile, Content: editedContent},
			})
			env.WriteFile(editedFile, editedContent)
			if err := env.SimulateSubagentStop(SubagentStopInput{
				SessionID:           sess.ID,
				TranscriptPath:      sess.TranscriptPath,
				AgentID:             subagentID,
				AgentTranscriptPath: subagentTranscript,
			}); err != nil {
				t.Fatalf("SimulateSubagentStop failed: %v", err)
			}

			state, err := env.GetSessionState(sess.ID)
			if err != nil || state == nil {
				t.Fatalf("GetSessionState failed: %v (state=%v)", err, state)
			}
			if state.Phase != session.PhaseIdle || len(state.LiveTaskRecords()) != 0 || !containsFile(state.FilesTouched, editedFile) {
				t.Fatalf("precondition: want IDLE session with only a completed record and %s in FilesTouched, got phase=%s files=%v records=%+v",
					editedFile, state.Phase, state.FilesTouched, state.TaskRecords)
			}

			if tt.tty {
				env.GitCommitWithShadowHooks("Add completed doc", editedFile)
			} else {
				env.GitCommitWithShadowHooksAsAgent("Add completed doc", editedFile)
			}

			checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
			if checkpointID == "" {
				t.Fatalf("commit of a completed subagent's files should carry an Entire-Checkpoint trailer")
			}
			storedTranscript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
				CheckpointTaskFilePath(checkpointID, taskToolUseID, "agent-"+subagentID+".jsonl"))
			if !ok {
				t.Fatalf("subagent transcript not materialized under the checkpoint's tasks/ subtree")
			}
			if !strings.Contains(storedTranscript, editedFile) {
				t.Errorf("materialized subagent transcript does not reference %q", editedFile)
			}
		})
	}
}

// TestSubagentCheckpoints_BackgroundSubagentEdit_AttributedToAgent pins that a
// background subagent's lines count as agent lines. The subagent writes after
// the parent's turn ended, so the edit is in no shadow snapshot when Claude
// Code's task-notification starts the next turn. Turn-start prompt attribution
// must not count that edit as human work. The notification's UserPromptSubmit
// and SubagentStop arrive in the same instant, so both orders are covered.
func TestSubagentCheckpoints_BackgroundSubagentEdit_AttributedToAgent(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name                 string
		turnStartBeforeStop  bool
		parentEdits          bool
		commitBeforeNextTurn bool
	}{
		{name: "subagent stop first", turnStartBeforeStop: false},
		{name: "notification turn first", turnStartBeforeStop: true},
		{name: "subagent stop first after parent checkpoint", turnStartBeforeStop: false, parentEdits: true},
		{name: "notification turn first after parent checkpoint", turnStartBeforeStop: true, parentEdits: true},
		{name: "commit before the next turn", commitBeforeNextTurn: true},
		{name: "commit before the next turn after parent checkpoint", commitBeforeNextTurn: true, parentEdits: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			sess := env.NewSession()

			// With a parent edit, the first turn's Stop writes a shadow
			// snapshot, so the next turn's prompt attribution is no longer the
			// pre-session baseline and any misattributed line shows as human.
			var parentChanges []FileChange
			if tt.parentEdits {
				parentChanges = []FileChange{{Path: "docs/parent.md", Content: "# Parent\n"}}
			}
			sess.CreateTranscript("delegate a background task", parentChanges)

			const (
				taskToolUseID = "toolu_01BackgroundAttribution"
				subagentID    = "f5555666677778888"
				editedFile    = "docs/background.md"
				editedContent = "# Background\n\nWritten by a background subagent.\n"
				editedLines   = 3
			)

			if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
			}
			if tt.parentEdits {
				env.WriteFile("docs/parent.md", "# Parent\n")
			}
			if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
				t.Fatalf("SimulatePreTask failed: %v", err)
			}
			if err := env.SimulatePostTask(PostTaskInput{
				SessionID:      sess.ID,
				TranscriptPath: sess.TranscriptPath,
				ToolUseID:      taskToolUseID,
				AgentID:        subagentID,
				Background:     true,
			}); err != nil {
				t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
			}
			if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateStop failed: %v", err)
			}

			subagentTranscript := sess.CreateSubagentTranscript(subagentID, []FileChange{
				{Path: editedFile, Content: editedContent},
			})
			env.WriteFile(editedFile, editedContent)

			subagentStop := func() {
				t.Helper()
				if err := env.SimulateSubagentStop(SubagentStopInput{
					SessionID:           sess.ID,
					TranscriptPath:      sess.TranscriptPath,
					AgentID:             subagentID,
					AgentTranscriptPath: subagentTranscript,
				}); err != nil {
					t.Fatalf("SimulateSubagentStop failed: %v", err)
				}
			}
			notificationTurn := func() {
				t.Helper()
				if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
					t.Fatalf("SimulateUserPromptSubmit (task notification) failed: %v", err)
				}
			}
			switch {
			case tt.commitBeforeNextTurn:
				subagentStop()
			case tt.turnStartBeforeStop:
				notificationTurn()
				subagentStop()
			default:
				subagentStop()
				notificationTurn()
			}
			if !tt.commitBeforeNextTurn {
				if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
					t.Fatalf("SimulateStop (notification turn) failed: %v", err)
				}
			}

			committed := []string{editedFile}
			if tt.parentEdits {
				committed = append(committed, "docs/parent.md")
			}
			env.GitCommitWithShadowHooksAsAgent("Add background doc", committed...)

			checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
			if checkpointID == "" {
				t.Fatalf("commit of a background subagent's file should carry an Entire-Checkpoint trailer")
			}
			content, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
			if !ok {
				t.Fatalf("session metadata.json not found for checkpoint %s", checkpointID)
			}
			var metadata checkpoint.Metadata
			if err := json.Unmarshal([]byte(content), &metadata); err != nil {
				t.Fatalf("parse session metadata: %v", err)
			}
			if metadata.Attribution == nil {
				t.Fatal("session metadata has no attribution")
			}
			attr := metadata.Attribution
			wantAgent := editedLines
			if tt.parentEdits {
				wantAgent++
			}
			if attr.AgentLines != wantAgent || attr.HumanAdded != 0 {
				t.Errorf("attribution: agent_lines=%d human_added=%d, want agent_lines=%d human_added=0",
					attr.AgentLines, attr.HumanAdded, wantAgent)
			}
		})
	}
}

// TestSubagentCheckpoints_JointCommitWithRunningSubagent_KeepsBothSessions pins
// that the read-only gate does not drop a co-author. A background subagent is
// still running under an IDLE parent, so its edit is not in the parent's
// FilesTouched yet; a second session commits mid-turn, and the one commit
// carries both sessions' files. The second session's transcript claim arms the
// read-only gate, which must find the running subagent's edit in its own
// transcript before treating the parent as read-only.
func TestSubagentCheckpoints_JointCommitWithRunningSubagent_KeepsBothSessions(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	const (
		taskToolUseID = "toolu_01JointCommit"
		subagentID    = "f4444555566667777"
		subagentFile  = "docs/joint.md"
		subagentBody  = "# Joint\n\nWritten by a background subagent that is still running.\n"
		codingFile    = "feature.go"
		codingBody    = "package main\n\nfunc Feature() {}\n"
	)

	parent := env.NewSession()
	parent.CreateTranscript("delegate a background task", nil)
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(parent.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("parent user-prompt-submit failed: %v", err)
	}
	if err := env.SimulatePreTask(parent.ID, parent.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("parent pre-task failed: %v", err)
	}
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      parent.ID,
		TranscriptPath: parent.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
		Background:     true,
	}); err != nil {
		t.Fatalf("parent post-task (background stub) failed: %v", err)
	}
	if err := env.SimulateStop(parent.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("parent stop failed: %v", err)
	}
	// The subagent writes its file and keeps running: no SubagentStop.
	parent.CreateSubagentTranscript(subagentID, []FileChange{{Path: subagentFile, Content: subagentBody}})
	env.WriteFile(subagentFile, subagentBody)

	parentState, err := env.GetSessionState(parent.ID)
	if err != nil || parentState == nil {
		t.Fatalf("GetSessionState for parent failed: %v (state=%v)", err, parentState)
	}
	if parentState.Phase != session.PhaseIdle || len(parentState.FilesTouched) != 0 || !hasLiveTaskRecord(parentState, taskToolUseID) {
		t.Fatalf("precondition: want IDLE parent with a live record and empty FilesTouched, got phase=%s files=%v records=%+v",
			parentState.Phase, parentState.FilesTouched, parentState.TaskRecords)
	}

	coding := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(coding.ID, coding.TranscriptPath); err != nil {
		t.Fatalf("coding user-prompt-submit failed: %v", err)
	}
	env.WriteFile(codingFile, codingBody)
	coding.CreateTranscript("Add the feature and commit everything", []FileChange{{Path: codingFile, Content: codingBody}})

	env.GitCommitWithShadowHooksAsAgent("Add feature and joint doc", codingFile, subagentFile)

	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("joint commit should carry an Entire-Checkpoint trailer")
	}
	summaryContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(checkpointID))
	if !found {
		t.Fatalf("CheckpointSummary not found for %s", checkpointID)
	}
	var summary checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(summaryContent), &summary); err != nil {
		t.Fatalf("Failed to parse CheckpointSummary: %v", err)
	}
	env.AssertCheckpointContainsSession(t, summary, coding.ID)
	env.AssertCheckpointContainsSession(t, summary, parent.ID)

	storedTranscript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, taskToolUseID, "agent-"+subagentID+".jsonl"))
	if !ok {
		t.Fatalf("running subagent's transcript not materialized under the checkpoint's tasks/ subtree")
	}
	if !strings.Contains(storedTranscript, subagentFile) {
		t.Errorf("materialized subagent transcript does not reference %q", subagentFile)
	}
}

// TestSubagentCheckpoints_UserEditToSubagentFile_StaysHuman pins that a user
// edit to a background subagent's file keeps counting as user work. The
// subagent's edit is credited to the agent from the content it left behind,
// not from whatever the file holds at the next snapshot or commit, and an
// edit already snapshotted is not picked up again from the subagent's
// transcript while the subagent keeps running.
func TestSubagentCheckpoints_UserEditToSubagentFile_StaysHuman(t *testing.T) {
	t.Parallel()

	const (
		taskToolUseID = "toolu_01UserEditsSubagentFile"
		subagentID    = "a6666777788889999"
		editedFile    = "docs/background.md"
		editedContent = "# Background\n\nWritten by a background subagent.\n"
		userContent   = editedContent + "User line one.\nUser line two.\n"
		parentFile    = "docs/parent.md"
		// 3 subagent lines plus the parent's 1.
		wantAgentLines = 4
		wantHumanAdded = 2
	)

	for _, tt := range []struct {
		name string
		// subagentRunning keeps the subagent live: it wrote during the first
		// turn, that turn's Stop snapshotted its file, and the user edits the
		// file before the next prompt while the subagent is still running.
		subagentRunning bool
		// nextTurn runs a notification turn after the user edit; otherwise
		// the user commits before any later turn.
		nextTurn bool
	}{
		{name: "after completion, commit before the next turn"},
		{name: "after completion, then the notification turn", nextTurn: true},
		{name: "while the subagent is still running", subagentRunning: true, nextTurn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			sess := env.NewSession()
			sess.CreateTranscript("delegate a background task", []FileChange{{Path: parentFile, Content: "# Parent\n"}})

			if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
			}
			env.WriteFile(parentFile, "# Parent\n")
			if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
				t.Fatalf("SimulatePreTask failed: %v", err)
			}
			if err := env.SimulatePostTask(PostTaskInput{
				SessionID:      sess.ID,
				TranscriptPath: sess.TranscriptPath,
				ToolUseID:      taskToolUseID,
				AgentID:        subagentID,
				Background:     true,
			}); err != nil {
				t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
			}

			writeSubagentEdit := func() string {
				t.Helper()
				path := sess.CreateSubagentTranscript(subagentID, []FileChange{{Path: editedFile, Content: editedContent}})
				env.WriteFile(editedFile, editedContent)
				return path
			}
			if tt.subagentRunning {
				writeSubagentEdit()
			}
			if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateStop failed: %v", err)
			}
			if !tt.subagentRunning {
				subagentTranscript := writeSubagentEdit()
				if err := env.SimulateSubagentStop(SubagentStopInput{
					SessionID:           sess.ID,
					TranscriptPath:      sess.TranscriptPath,
					AgentID:             subagentID,
					AgentTranscriptPath: subagentTranscript,
				}); err != nil {
					t.Fatalf("SimulateSubagentStop failed: %v", err)
				}
			}

			env.WriteFile(editedFile, userContent)

			if tt.nextTurn {
				if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
					t.Fatalf("SimulateUserPromptSubmit (next turn) failed: %v", err)
				}
				if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
					t.Fatalf("SimulateStop (next turn) failed: %v", err)
				}
			}

			env.GitCommitWithShadowHooksAsAgent("Add docs", editedFile, parentFile)

			checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
			if checkpointID == "" {
				t.Fatalf("commit should carry an Entire-Checkpoint trailer")
			}
			content, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
			if !ok {
				t.Fatalf("session metadata.json not found for checkpoint %s", checkpointID)
			}
			var metadata checkpoint.Metadata
			if err := json.Unmarshal([]byte(content), &metadata); err != nil {
				t.Fatalf("parse session metadata: %v", err)
			}
			if metadata.Attribution == nil {
				t.Fatal("session metadata has no attribution")
			}
			attr := metadata.Attribution
			if attr.AgentLines != wantAgentLines || attr.HumanAdded != wantHumanAdded {
				t.Errorf("attribution: agent_lines=%d human_added=%d, want agent_lines=%d human_added=%d",
					attr.AgentLines, attr.HumanAdded, wantAgentLines, wantHumanAdded)
			}
		})
	}
}

// writeShellSubagentTranscript writes a Claude Code subagent transcript whose
// only file change is a Bash command, in Claude Code's real line shape
// (millisecond timestamps), with the call issued at start and its result
// recorded at end.
func writeShellSubagentTranscript(t *testing.T, sess *Session, agentID, command string, start, end time.Time) string {
	t.Helper()
	dir := paths.SubagentsDir(filepath.Dir(sess.TranscriptPath), sess.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create subagents dir: %v", err)
	}
	stamp := func(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z") }
	lines := []map[string]any{
		{"type": "user", "uuid": "u1", "timestamp": stamp(start.Add(-time.Second)), "message": map[string]any{"role": "user", "content": "write the result file"}},
		{"type": "assistant", "uuid": "a1", "timestamp": stamp(start), "message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "toolu_01ShellWriteCall0000000001", "name": "Bash", "input": map[string]any{"command": command}},
		}}},
		{"type": "user", "uuid": "u2", "timestamp": stamp(end), "message": map[string]any{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "toolu_01ShellWriteCall0000000001", "content": ""},
		}}},
		{"type": "assistant", "uuid": "a2", "timestamp": stamp(end.Add(time.Second)), "message": map[string]any{"role": "assistant", "content": "Done."}},
	}
	var b strings.Builder
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("marshal transcript line: %v", err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, paths.AgentTranscriptFileName(agentID))
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write subagent transcript: %v", err)
	}
	return path
}

// TestSubagentCheckpoints_BackgroundSubagentShellEdit_AttributedToAgent pins
// that a background subagent's shell-written file counts as agent work and is
// listed on its task record. The transcript analyzer only sees Edit/Write
// calls, so the file is matched to the subagent by its modification time
// falling inside the subagent's Bash call. A file the user writes while the
// subagent runs, outside any agent tool call, stays user work.
func TestSubagentCheckpoints_BackgroundSubagentShellEdit_AttributedToAgent(t *testing.T) {
	t.Parallel()

	const (
		taskToolUseID = "toolu_01BackgroundShellWrite"
		subagentID    = "b7777888899990000"
		shellFile     = "docs/shell.md"
		shellContent  = "# Shell\n\nWritten by a background subagent's shell command.\n"
		humanFile     = "docs/human.md"
		humanContent  = "Human line one, written while the subagent ran.\nHuman line two.\n"
		parentFile    = "docs/parent.md"
		// 3 shell lines plus the parent's 1.
		wantAgentLines = 4
		wantHumanAdded = 2
	)

	for _, tt := range []struct {
		name                 string
		turnStartBeforeStop  bool
		commitBeforeNextTurn bool
	}{
		{name: "subagent stop first"},
		{name: "notification turn first", turnStartBeforeStop: true},
		{name: "commit before the next turn", commitBeforeNextTurn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			sess := env.NewSession()
			sess.CreateTranscript("delegate a background task", []FileChange{{Path: parentFile, Content: "# Parent\n"}})

			if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
			}
			env.WriteFile(parentFile, "# Parent\n")
			if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
				t.Fatalf("SimulatePreTask failed: %v", err)
			}
			if err := env.SimulatePostTask(PostTaskInput{
				SessionID:      sess.ID,
				TranscriptPath: sess.TranscriptPath,
				ToolUseID:      taskToolUseID,
				AgentID:        subagentID,
				Background:     true,
			}); err != nil {
				t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
			}
			if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateStop failed: %v", err)
			}

			// The subagent's Bash call runs from callStart to callEnd and writes
			// the file inside it; the user writes their file well after it.
			callStart := time.Now().Add(-30 * time.Second)
			callEnd := callStart.Add(2 * time.Second)
			subagentTranscript := writeShellSubagentTranscript(t, sess, subagentID,
				"printf '# Shell\\n\\nWritten by a background subagent'\"'\"'s shell command.\\n' > "+shellFile, callStart, callEnd)
			env.WriteFile(shellFile, shellContent)
			setModTime(t, filepath.Join(env.RepoDir, shellFile), callStart.Add(time.Second))
			env.WriteFile(humanFile, humanContent)
			setModTime(t, filepath.Join(env.RepoDir, humanFile), callEnd.Add(15*time.Second))

			subagentStop := func() {
				t.Helper()
				if err := env.SimulateSubagentStop(SubagentStopInput{
					SessionID:           sess.ID,
					TranscriptPath:      sess.TranscriptPath,
					AgentID:             subagentID,
					AgentTranscriptPath: subagentTranscript,
				}); err != nil {
					t.Fatalf("SimulateSubagentStop failed: %v", err)
				}
			}
			notificationTurn := func() {
				t.Helper()
				if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
					t.Fatalf("SimulateUserPromptSubmit (task notification) failed: %v", err)
				}
			}
			switch {
			case tt.commitBeforeNextTurn:
				subagentStop()
			case tt.turnStartBeforeStop:
				notificationTurn()
				subagentStop()
			default:
				subagentStop()
				notificationTurn()
			}
			if !tt.commitBeforeNextTurn {
				if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
					t.Fatalf("SimulateStop (notification turn) failed: %v", err)
				}
			}

			env.GitCommitWithShadowHooksAsAgent("Add docs", shellFile, humanFile, parentFile)

			checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
			if checkpointID == "" {
				t.Fatalf("commit should carry an Entire-Checkpoint trailer")
			}
			content, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
			if !ok {
				t.Fatalf("session metadata.json not found for checkpoint %s", checkpointID)
			}
			var metadata checkpoint.Metadata
			if err := json.Unmarshal([]byte(content), &metadata); err != nil {
				t.Fatalf("parse session metadata: %v", err)
			}
			if metadata.Attribution == nil {
				t.Fatal("session metadata has no attribution")
			}
			attr := metadata.Attribution
			if attr.AgentLines != wantAgentLines || attr.HumanAdded != wantHumanAdded {
				t.Errorf("attribution: agent_lines=%d human_added=%d, want agent_lines=%d human_added=%d",
					attr.AgentLines, attr.HumanAdded, wantAgentLines, wantHumanAdded)
			}

			raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, taskToolUseID, "task.json"))
			if !ok {
				t.Fatalf("task.json not materialized for %s", taskToolUseID)
			}
			var task struct {
				Files []string `json:"files"`
			}
			if err := json.Unmarshal([]byte(raw), &task); err != nil {
				t.Fatalf("parse task.json: %v", err)
			}
			if !slices.Equal(task.Files, []string{shellFile}) {
				t.Errorf("task.json files = %v, want [%s]", task.Files, shellFile)
			}
		})
	}
}

func setModTime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("set mtime of %s: %v", path, err)
	}
}
