package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// snapshotAgentStop records the worktree as it stands when an agent stops
// without a turn-end step of its own: a turn whose transcript and change
// detection named no files (a shell command can still have written some), or
// a background subagent finishing after its parent's turn ended. Everything
// changed while an agent was busy is agent work, so the snapshot must exist
// before the next prompt's human diff or a commit reads it. The write is
// skipped when nothing changed since the previous snapshot, and nothing is
// written for a session whose state is gone or ended (no resurrection).
func snapshotAgentStop(ctx context.Context, ag agent.Agent, sessionID, commitMessage string) error {
	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session state: %w", err)
	}
	if state == nil || state.Phase == session.PhaseEnded || state.EndedAt != nil {
		return nil
	}
	author, err := GetGitAuthor(ctx)
	if err != nil {
		return fmt.Errorf("get git author: %w", err)
	}
	err = GetStrategy(ctx).SaveStep(ctx, strategy.StepContext{
		SessionID:     sessionID,
		MetadataDir:   paths.SessionMetadataDirFromSessionID(sessionID),
		CommitMessage: commitMessage,
		AuthorName:    author.Name,
		AuthorEmail:   author.Email,
		AgentType:     ag.Type(),
		// A stop with nothing changed since the last snapshot writes nothing.
		SkipWhenUnchanged: true,
	})
	if errors.Is(err, strategy.ErrStateNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("snapshot worktree: %w", err)
	}
	return nil
}
