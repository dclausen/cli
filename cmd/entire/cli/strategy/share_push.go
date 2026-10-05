package strategy

import (
	"context"

	"github.com/go-git/go-git/v6"
)

// ShareCheckpointResult reports what PushSharedCheckpoints delivered.
type ShareCheckpointResult struct {
	// Pushed counts the checkpoint refs that reached the remote. It is
	// meaningful only for the git-refs backend, whose push queue names exactly
	// the refs it sent; the git-branch backend pushes one branch carrying
	// however many checkpoints it accumulated, and reports Counted=false.
	Pushed int
	// Counted records whether Pushed is a real count, so a caller does not
	// print "pushed 0 checkpoints" for a branch push that in fact succeeded.
	Counted bool
	// PushDisabled reports push_sessions=false in settings: nothing left the
	// machine, and the checkpoint stays local. Distinct from Pushed==0, which
	// with pushing enabled means the queue was already empty.
	PushDisabled bool
}

// PushSharedCheckpoints delivers locally-written checkpoints to the checkpoint
// remote for `entire session share`, dispatching on the configured primary
// backend the same way the pre-push hook does.
//
// The backend fork lives here rather than in the CLI because pushSettings
// already resolves which primary is configured, and because the two paths are
// not interchangeable: git-refs pushes the per-checkpoint refs named in the
// push queue, git-branch pushes the single entire/checkpoints/v1 branch.
//
// git-refs goes through PushQueuedCheckpointRefs, which surfaces failures
// rather than swallowing them — sharing is a foreground command whose whole
// purpose is delivery, so "it didn't actually reach the remote" has to be an
// error and not a log line. The git-branch path reuses PrePush, which is the
// only implementation of that push and is fail-soft by design; a caller that
// needs certainty there should verify the branch separately.
//
// Both paths run the OPF gate before anything is sent. Callers must have run
// EnsureRedactionConfigured first: unconfigured redaction reads as "OPF off"
// and would wave un-OPF'd checkpoint content through.
func (s *ManualCommitStrategy) PushSharedCheckpoints(ctx context.Context, repo *git.Repository, remote string) (ShareCheckpointResult, error) {
	ps := resolvePushSettings(ctx, remote)
	if ps.pushDisabled {
		return ShareCheckpointResult{PushDisabled: true}, nil
	}

	if ps.primaryIsRefs {
		pushed, pushDisabled, err := PushQueuedCheckpointRefs(ctx, repo, remote)
		return ShareCheckpointResult{Pushed: pushed, Counted: true, PushDisabled: pushDisabled}, err
	}

	if err := s.PrePush(ctx, remote); err != nil {
		return ShareCheckpointResult{}, err
	}
	return ShareCheckpointResult{Counted: false}, nil
}
