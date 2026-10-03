package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/validation"

	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Layout of the subagent task records at the checkpoint root, shared by the
// writer (writeTaskRecordEntry) and the reader below:
//
//	tasks/<tool_use_id>/task.json             TaskRecord
//	tasks/<tool_use_id>/agent-<agent_id>.jsonl  subagent transcript (optional)
const (
	taskRecordsDirName   = "tasks"
	taskRecordFileName   = "task.json"
	taskTranscriptStem   = "agent-"
	taskTranscriptSuffix = ".jsonl"
)

// taskTranscriptFileName names a task record's transcript blob.
func taskTranscriptFileName(agentID string) string {
	return taskTranscriptStem + agentID + taskTranscriptSuffix
}

// ListTasks implements TaskReader for the git-branch store.
func (s *GitStore) ListTasks(ctx context.Context, checkpointID id.CheckpointID) ([]TaskEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	checkpointTree, err := s.getCheckpointFetchingTree(ctx, checkpointID)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}
	return listTasksFromCheckpointTree(checkpointTree)
}

// ReadTaskTranscript implements TaskReader for the git-branch store.
func (s *GitStore) ReadTaskTranscript(ctx context.Context, checkpointID id.CheckpointID, toolUseID string) ([]byte, error) {
	if err := validateTaskSelector(toolUseID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	checkpointTree, err := s.getCheckpointFetchingTree(ctx, checkpointID)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}
	return readTaskTranscriptFromCheckpointTree(checkpointTree, toolUseID)
}

// ListTasks implements TaskReader for the git-refs store.
func (s *gitRefsStore) ListTasks(ctx context.Context, checkpointID id.CheckpointID) ([]TaskEntry, error) {
	checkpointTree, err := s.checkpointTree(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	return listTasksFromCheckpointTree(checkpointTree)
}

// ReadTaskTranscript implements TaskReader for the git-refs store.
func (s *gitRefsStore) ReadTaskTranscript(ctx context.Context, checkpointID id.CheckpointID, toolUseID string) ([]byte, error) {
	if err := validateTaskSelector(toolUseID); err != nil {
		return nil, err
	}
	checkpointTree, err := s.checkpointTree(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	return readTaskTranscriptFromCheckpointTree(checkpointTree, toolUseID)
}

// validateTaskSelector rejects an empty or path-unsafe tool_use_id before it is
// used as a tree path. ValidateToolUseID alone accepts "" (the field is
// optional on payloads), which here would address the tasks/ directory itself.
func validateTaskSelector(toolUseID string) error {
	if toolUseID == "" {
		return errors.New("tool use ID is required")
	}
	if err := validation.ValidateToolUseID(toolUseID); err != nil {
		return fmt.Errorf("invalid task selector: %w", err)
	}
	return nil
}

// tasksTree returns the checkpoint's tasks/ subtree, or (nil, nil) when the
// checkpoint has none — every checkpoint written before task records existed,
// and every session without subagent work.
func tasksTree(checkpointTree *FetchingTree) (*FetchingTree, error) {
	tree, err := checkpointTree.Tree(taskRecordsDirName)
	if err != nil {
		if errors.Is(err, object.ErrDirectoryNotFound) {
			return nil, nil //nolint:nilnil // no task records is not an error
		}
		return nil, fmt.Errorf("read %s/: %w", taskRecordsDirName, err)
	}
	return tree, nil
}

// listTasksFromCheckpointTree reads every tasks/<tool_use_id>/ record from a
// checkpoint tree. Shared by both git backends, which differ only in how they
// navigate to the checkpoint tree. task.json is pushed checkpoint data that
// anyone with push access can author, so each record's identifiers are
// validated before they name a tree path.
func listTasksFromCheckpointTree(checkpointTree *FetchingTree) ([]TaskEntry, error) {
	tasks, err := tasksTree(checkpointTree)
	if err != nil {
		return nil, err
	}
	if tasks == nil {
		return []TaskEntry{}, nil
	}

	entries := make([]TaskEntry, 0, len(tasks.RawEntries()))
	for _, raw := range tasks.RawEntries() {
		if raw.Mode != filemode.Dir {
			continue
		}
		entry := TaskEntry{ToolUseID: raw.Name}
		if err := validateTaskSelector(raw.Name); err != nil {
			entry.Err = err
			entries = append(entries, entry)
			continue
		}
		taskDir, err := tasks.Tree(raw.Name)
		if err != nil {
			entry.Err = err
			entries = append(entries, entry)
			continue
		}
		record, err := readTaskRecord(taskDir)
		if err != nil {
			entry.Err = err
			entries = append(entries, entry)
			continue
		}
		entry.Record = *record
		entry.TranscriptStored = hasRawEntry(taskDir, taskTranscriptFileName(record.AgentID))
		entries = append(entries, entry)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].Record.StartedAt, entries[j].Record.StartedAt
		if !a.Equal(b) {
			return a.Before(b)
		}
		return entries[i].ToolUseID < entries[j].ToolUseID
	})
	return entries, nil
}

// readTaskTranscriptFromCheckpointTree returns the stored transcript of the
// task record named toolUseID (already validated by the caller).
func readTaskTranscriptFromCheckpointTree(checkpointTree *FetchingTree, toolUseID string) ([]byte, error) {
	tasks, err := tasksTree(checkpointTree)
	if err != nil {
		return nil, err
	}
	if tasks == nil || !hasRawEntry(tasks, toolUseID) {
		return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, toolUseID)
	}
	taskDir, err := tasks.Tree(toolUseID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrTaskNotFound, toolUseID, err)
	}
	record, err := readTaskRecord(taskDir)
	if err != nil {
		return nil, err
	}
	name := taskTranscriptFileName(record.AgentID)
	if !hasRawEntry(taskDir, name) {
		reason := record.TranscriptUnavailableReason
		if reason == "" {
			reason = "no transcript stored"
		}
		return nil, fmt.Errorf("task %s: %w: %s", toolUseID, ErrNoTranscript, reason)
	}
	file, err := taskDir.File(name)
	if err != nil {
		return nil, fmt.Errorf("task %s: read %s: %w", toolUseID, name, err)
	}
	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("task %s: read %s: %w", toolUseID, name, err)
	}
	return []byte(content), nil
}

// readTaskRecord parses and validates one task directory's task.json.
func readTaskRecord(taskDir *FetchingTree) (*TaskRecord, error) {
	file, err := taskDir.File(taskRecordFileName)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", taskRecordFileName, err)
	}
	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", taskRecordFileName, err)
	}
	var record TaskRecord
	if err := json.Unmarshal([]byte(content), &record); err != nil {
		return nil, fmt.Errorf("parse %s: %w", taskRecordFileName, err)
	}
	if err := validation.ValidateAgentID(record.AgentID); err != nil {
		return nil, fmt.Errorf("%s: %w", taskRecordFileName, err)
	}
	return &record, nil
}

// hasRawEntry reports whether tree directly contains name, without reading any
// blob (so a missing transcript blob in a partial clone is not fetched just to
// learn that it exists).
func hasRawEntry(tree *FetchingTree, name string) bool {
	for _, e := range tree.RawEntries() {
		if e.Name == name {
			return true
		}
	}
	return false
}
