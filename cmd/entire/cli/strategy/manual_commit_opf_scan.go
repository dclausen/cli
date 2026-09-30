// Detached OPF scan worker, shared by both checkpoint backends.
//
// OPF inference is slow (about 1.14 s per KB of prose on CPU), so a real
// session takes minutes to hours of model time. The pre-push hook therefore
// never runs the model: it rewrites checkpoints from the OPF span cache and
// holds back anything not scanned yet. This worker fills the cache in the
// background and then delivers what it scanned, so the user's `git push`
// returns immediately and their checkpoints follow as soon as OPF finishes.
//
// The worker only acts on the decision the user's push made. It is spawned
// only when that push resolved OPFRun, it delivers only to the remote that
// push named, and delivery goes through the same pre-push code, which ships a
// checkpoint only once its exact commit carries Entire-OPF-Applied. If the
// worker cannot reach the remote, the checkpoints stay queued and the next
// push delivers them.
package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/spawnmarker"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

const opfScanComponent = "opf-scan"

// opfScanSpawnThrottle collapses a burst of pushes into one spawn. The worker
// lock already keeps a second worker from doing anything, so this only saves
// the fork.
const opfScanSpawnThrottle = time.Minute

// opfScanMaxPasses bounds the worker's loop. A pass repeats only while new
// unscanned content keeps appearing (a session still writing checkpoints), so
// this exists to stop a detached process nobody watches from spinning.
const opfScanMaxPasses = 10

// opfScanDeliveryTimeout bounds one delivery attempt. Delivery is a push of
// already-rewritten refs; a push that takes longer than this is treated as
// failed and left for the next user push.
const opfScanDeliveryTimeout = 10 * time.Minute

// opfScanWorkerLockName is held for a worker's whole run, in the shared git
// common dir so workers spawned from different worktrees exclude each other.
const opfScanWorkerLockName = "entire/opf-scan-worker.lock"

// opfScanWorkerLockWait turns the blocking flock into a try-lock: a worker that
// cannot take the lock almost immediately leaves the work to the one holding
// it, which re-collects its work after every pass.
const opfScanWorkerLockWait = 100 * time.Millisecond

// opfScanSpawn is the process-spawn seam, swapped in tests: execx.SpawnDetached
// is a no-op under `go test`, which would make the spawn decision unobservable.
var opfScanSpawn = spawnDetachedOPFScanProcess

func spawnDetachedOPFScanProcess(worktreeRoot, remote string) {
	execx.SpawnDetached(worktreeRoot, "__opf_scan", remote)
}

type opfScanWorkerKey struct{}

// withinOPFScanWorker marks ctx as running inside the scan worker. Pre-push
// code consults it to take the decision the spawning push already made, and to
// never spawn another worker from inside one.
func withinOPFScanWorker(ctx context.Context) context.Context {
	return context.WithValue(ctx, opfScanWorkerKey{}, true)
}

func inOPFScanWorker(ctx context.Context) bool {
	v, _ := ctx.Value(opfScanWorkerKey{}).(bool) //nolint:errcheck // absent means false
	return v
}

// maybeSpawnOPFScan starts the scan worker for remote unless this already is
// the worker or a worker was spawned within opfScanSpawnThrottle. Callers
// invoke it only when the push resolved OPFRun and something was held back as
// not yet scanned.
func maybeSpawnOPFScan(ctx context.Context, remote string) {
	if inOPFScanWorker(ctx) || !redact.OPFEnabled() {
		return
	}
	logCtx := logging.WithComponent(ctx, opfScanComponent)
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(logCtx, "skipping OPF scan spawn: could not resolve worktree root",
			slog.String("error", err.Error()))
		return
	}
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		logging.Debug(logCtx, "skipping OPF scan spawn: could not resolve git common dir",
			slog.String("error", err.Error()))
		return
	}
	if spawnmarker.RecentlySpawned(commonDir, "opf-scan-spawn", opfScanSpawnThrottle, time.Now()) {
		return
	}
	opfScanSpawn(root, remote)
	logging.Info(logCtx, "checkpoints held for OPF; spawned background scan", slog.String("remote", remote))
}

// RunOPFScan is the body of the detached `entire __opf_scan <remote>` worker.
// Each pass collects the content waiting for OPF, scans what the span cache
// lacks, and then runs the pre-push delivery for remote, which rewrites from
// the cache and pushes whatever is now covered. It stops when nothing is left,
// when a pass finds exactly the work the previous one did (nothing will change
// without new input), or after opfScanMaxPasses.
//
// Best-effort by construction: every failure is logged, never returned as a
// process error, because nothing watches this child's exit code.
func RunOPFScan(ctx context.Context, remote string) error {
	logCtx := logging.WithComponent(ctx, opfScanComponent)
	// OPFEnabled reads process-global config that only EnsureRedactionConfigured
	// sets; without it the worker would read "OPF off" and do nothing. Unlike
	// the hook, a scanner-config error stops the worker: stamping commits with a
	// scanner set the settings did not choose is not a call a background
	// process gets to make.
	if err := EnsureRedactionConfigured(ctx); err != nil {
		logging.Warn(logCtx, "opf scan skipped: redaction could not be configured",
			slog.String("error", err.Error()))
		return nil
	}
	if !redact.OPFEnabled() {
		logging.Debug(logCtx, "opf scan skipped: OPF is not enabled")
		return nil
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(logCtx, "opf scan skipped: could not open repo", slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()

	release, held := acquireOPFScanWorkerLock(ctx)
	if held {
		logging.Debug(logCtx, "opf scan skipped: another worker is already running")
		return nil
	}
	defer release()

	cache, err := checkpoint.OPFSpanCacheForRepo(repo)
	if err != nil {
		logging.Warn(logCtx, "opf scan skipped: span cache unavailable", slog.String("error", err.Error()))
		return nil
	}
	if commonDir, cdErr := gitdir.CommonDir(ctx); cdErr == nil {
		if _, pruneErr := checkpoint.PruneOPFSpanCache(commonDir, time.Now(), checkpoint.OPFSpanCacheMaxAge); pruneErr != nil {
			logging.Debug(logCtx, "opf scan: cache prune failed", slog.String("error", pruneErr.Error()))
		}
	}

	ctx = withinOPFScanWorker(ctx)
	var previous []string
	for range opfScanMaxPasses {
		blobs, collectErr := collectOPFScanWork(ctx, repo, remote)
		if collectErr != nil {
			logging.Warn(logCtx, "opf scan: could not collect pending checkpoints",
				slog.String("error", collectErr.Error()))
			return nil
		}
		if len(blobs) == 0 {
			return nil
		}
		ids := opfScanBlobIDs(blobs)
		if slices.Equal(ids, previous) {
			// The last pass scanned and delivered this exact set and it is
			// still pending, so delivery failed for a reason a retry in this
			// process will not fix. The next user push tries again.
			return nil
		}
		previous = ids

		if scanErr := redact.ScanBlobsWithPrivacyFilter(ctx, blobs, cache); scanErr != nil {
			logging.Warn(logCtx, "opf scan failed; checkpoints stay held", slog.String("error", scanErr.Error()))
			return nil
		}
		deliverCtx, cancel := context.WithTimeout(ctx, opfScanDeliveryTimeout)
		deliverErr := NewManualCommitStrategy().PrePushFromGitHook(deliverCtx, remote)
		cancel()
		if deliverErr != nil {
			logging.Warn(logCtx, "opf scan: delivery failed; checkpoints stay queued for the next push",
				slog.String("error", deliverErr.Error()))
			return nil
		}
	}
	logging.Warn(logCtx, "opf scan: stopping after the maximum number of passes",
		slog.Int("passes", opfScanMaxPasses))
	return nil
}

// collectOPFScanWork returns the blobs of every checkpoint commit that still
// lacks the OPF trailer on the primary backend, each unit capped the way the
// rewrite caps it. A unit over a cap is left out and logged: the rewrite would
// refuse it anyway, and scanning pathological content only burns model time.
func collectOPFScanWork(ctx context.Context, repo *git.Repository, remote string) ([]redact.NamedBlob, error) {
	logCtx := logging.WithComponent(ctx, opfScanComponent)
	batchLimit := resolveBatchLimit()
	rawCap := rawByteCapForBatchLimit(batchLimit)

	var units [][]*object.Commit
	if primaryIsGitRefs(ctx) {
		queue, err := checkpoint.PushQueueForRepo(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("resolve push queue: %w", err)
		}
		queued, err := queue.Peek()
		if err != nil {
			return nil, fmt.Errorf("peek push queue: %w", err)
		}
		existing, _ := partitionLocalRefs(repo, queued)
		for _, refName := range existing {
			ref, refErr := repo.Reference(refName, true)
			if refErr != nil {
				continue
			}
			chain, _, chainErr := unappliedAncestry(repo, ref.Hash())
			if chainErr != nil {
				return nil, fmt.Errorf("walk ancestry of %s: %w", refName, chainErr)
			}
			if len(chain) > 0 {
				units = append(units, chain)
			}
		}
	} else {
		chain, err := v1CommitsAwaitingOPF(ctx, repo, remote)
		if err != nil {
			return nil, err
		}
		if len(chain) > 0 {
			units = append(units, chain)
		}
	}

	var blobs []redact.NamedBlob
	for _, unit := range units {
		unitBlobs, err := collectCommitBlobsForOPF(repo, unit, rawCap)
		if err != nil {
			var rawTooLarge *OPFRawBytesTooLargeError
			if errors.As(err, &rawTooLarge) {
				logging.Warn(logCtx, "opf scan: skipping checkpoint over the raw-byte cap", slog.String("error", err.Error()))
				continue
			}
			return nil, err
		}
		if leafBytes := redact.SumProseLeafBytes(unitBlobs); leafBytes > batchLimit {
			logging.Warn(logCtx, "opf scan: skipping checkpoint over the prose-leaf cap",
				slog.Int("leaf_bytes", leafBytes), slog.Int("limit", batchLimit))
			continue
		}
		blobs = append(blobs, unitBlobs...)
	}
	return blobs, nil
}

// v1CommitsAwaitingOPF returns the unpushed v1 commits that lack the OPF
// trailer, oldest first, measured against remote's live v1 tip the same way
// RewriteUnpushedV1WithOPF measures them.
func v1CommitsAwaitingOPF(ctx context.Context, repo *git.Repository, remote string) ([]*object.Commit, error) {
	localTip, err := readV1Tip(repo, plumbing.NewBranchReferenceName(paths.MetadataBranchName))
	if err != nil {
		return nil, fmt.Errorf("read local v1: %w", err)
	}
	if localTip.IsZero() {
		return nil, nil
	}
	ps := resolvePushSettings(ctx, remote)
	remoteTip, err := resolveRemoteV1Tip(ctx, repo, ps.pushTarget())
	if err != nil {
		return nil, fmt.Errorf("read remote v1: %w", err)
	}
	unpushed, err := listUnpushedV1Commits(repo, localTip, remoteTip)
	if err != nil {
		return nil, fmt.Errorf("list unpushed v1 commits: %w", err)
	}
	pending := unpushed[:0]
	for _, c := range unpushed {
		if !trailers.HasOPFApplied(c.Message) {
			pending = append(pending, c)
		}
	}
	return pending, nil
}

func collectCommitBlobsForOPF(repo *git.Repository, commits []*object.Commit, rawCap int) ([]redact.NamedBlob, error) {
	budget := newOPFRawByteBudget(rawCap)
	var blobs []redact.NamedBlob
	var treePaths []string
	for _, c := range commits {
		tree, err := repo.TreeObject(c.TreeHash)
		if err != nil {
			return nil, fmt.Errorf("load tree for %s: %w", c.Hash.String()[:7], err)
		}
		if err := collectTreeBlobsWithinBudget(repo, tree, "", &blobs, &treePaths, budget); err != nil {
			return nil, fmt.Errorf("collect blobs %s: %w", c.Hash.String()[:7], err)
		}
	}
	return blobs, nil
}

func opfScanBlobIDs(blobs []redact.NamedBlob) []string {
	ids := make([]string, 0, len(blobs))
	for _, b := range blobs {
		ids = append(ids, b.ID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// acquireOPFScanWorkerLock takes the per-repo worker lock. held reports that
// another worker has it. Any other failure returns a no-op release with held
// false: running unguarded is never worse than two workers racing, which the
// ref CAS already makes safe.
func acquireOPFScanWorkerLock(ctx context.Context) (release func(), held bool) {
	noop := func() {}
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		return noop, false
	}
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return noop, false
	}
	if err := osroot.MkdirAllNoSymlink(root, path.Dir(opfScanWorkerLockName), 0o750); err != nil {
		return noop, false
	}
	lockCtx, cancel := context.WithTimeout(ctx, opfScanWorkerLockWait)
	defer cancel()
	release, err = flock.AcquireContextIn(lockCtx, root, opfScanWorkerLockName)
	if err != nil {
		return noop, errors.Is(err, context.DeadlineExceeded)
	}
	return release, false
}
