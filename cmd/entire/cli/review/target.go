package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const envReviewFindingsWorktree = "ENTIRE_REVIEW_FINDINGS_WORKTREE"

// TargetWorktree describes the checkout prepared for a targeted review.
// Created is false when the branch was already checked out and that existing
// worktree is being reused.
type TargetWorktree struct {
	Path    string
	Created bool
}

// ResolvedTarget is a review target resolved to a local branch whose head is
// pinned before anything is checked out.
type ResolvedTarget struct {
	Branch  string
	HeadSHA string
	// ExistingWorktree is the worktree the branch is already checked out in,
	// or "" when the review will create one.
	ExistingWorktree string
}

// ErrTargetCancelled is returned by ResolveTarget when the user declines a
// prompt while the target is resolved (for example, fetching the branch).
var ErrTargetCancelled = errors.New("review target cancelled")

type reviewWorktreeRunner func(context.Context, string, []string, []string, io.Reader, io.Writer, io.Writer) error

// targetReviewRequest carries the --target invocation.
type targetReviewRequest struct {
	Target          string
	Positional      []string
	ProfileOverride string
	CleanupWorktree bool
	ShowConfig      bool
	ShowConfigJSON  bool
	// ModeSelected is set when a non-run mode (--configure, --list, ...) was
	// also passed; --target only applies to running a review.
	ModeSelected bool
	Gate         reviewGateOptions
}

func runTargetReview(ctx context.Context, cmd *cobra.Command, req targetReviewRequest, deps Deps) error {
	if req.ModeSelected {
		return errors.New("--target can only be used when running a review")
	}
	if err := req.Gate.validate(); err != nil {
		return err
	}
	if deps.ResolveTarget == nil || deps.CheckoutTarget == nil {
		return errors.New("review target checkout is unavailable")
	}
	callerWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve caller worktree: %w", err)
	}
	errOut := cmd.ErrOrStderr()

	profileName := req.ProfileOverride
	if len(req.Positional) == 1 {
		profileName = req.Positional[0]
	}
	var agents []string
	forwardProfile := ""
	if req.ShowConfig {
		agents = showConfigAgents(ctx, profileName, req.Gate.AgentOverride)
	} else {
		// Resolve the profile here, in the user's checkout, so first-run setup
		// and the chooser never run inside (or save into) the branch's worktree,
		// and so the gate knows which agents' configuration to inspect.
		selection, selErr := resolveReviewProfile(ctx, cmd, profileName, deps)
		if selErr != nil || selection.done {
			return selErr
		}
		agents = profileAgentNames(selection.profile, req.Gate.AgentOverride)
		if profileName == "" {
			forwardProfile = selection.name
		}
	}

	resolved, err := deps.ResolveTarget(ctx, cmd.OutOrStdout(), errOut, req.Target)
	if errors.Is(err, ErrTargetCancelled) {
		fmt.Fprintln(errOut, "Review cancelled. Nothing was checked out or run.")
		return nil
	}
	if err != nil {
		return err
	}

	source := TrustSource{RepoRoot: callerWorktree, Commit: resolved.HeadSHA}
	if resolved.ExistingWorktree != "" {
		// A reused worktree runs what is on its disk, including uncommitted and
		// ignored files, so inspect that rather than the commit.
		worktreeHead, headErr := gitexec.HeadSHA(ctx, resolved.ExistingWorktree)
		if headErr != nil || worktreeHead != resolved.HeadSHA {
			cmd.SilenceUsage = true
			fmt.Fprintf(errOut, "Not run: the worktree for %s at %s is not at %s.\n",
				sanitizeDisplay(req.Target), resolved.ExistingWorktree, shortSHA(resolved.HeadSHA))
			return wrapReviewSilentError(deps.NewSilentError, errors.New("reused worktree out of sync"))
		}
		source = TrustSource{WorktreeRoot: resolved.ExistingWorktree}
	}
	subject, inv, err := inspectReview(ctx, callerWorktree, resolved.HeadSHA, source, agents, req.ShowConfig, deps)
	if err != nil {
		return trustInspectionFailed(cmd, deps, err)
	}
	subject.Label = req.Target
	subject.Branch = resolved.Branch

	if req.ShowConfig {
		return printTrustConfig(cmd.OutOrStdout(), subject, inv, req.ShowConfigJSON)
	}
	if err := runTrustGate(ctx, cmd, req.Gate, subject, inv, deps); err != nil {
		if errors.Is(err, errTrustCancelled) {
			return nil
		}
		return err
	}

	prepared, err := deps.CheckoutTarget(ctx, cmd.OutOrStdout(), errOut, resolved, !subject.Yours)
	if err != nil {
		return err
	}
	// The branch could have moved between the gate and the checkout. Nothing
	// has run in the worktree yet, so a mismatch is caught before it matters.
	if checkedOut, headErr := gitexec.HeadSHA(ctx, prepared.Path); headErr != nil || checkedOut != resolved.HeadSHA {
		if prepared.Created && deps.RemoveTarget != nil {
			_ = deps.RemoveTarget(ctx, prepared.Path) //nolint:errcheck // best effort; the abort below is what matters
		}
		cmd.SilenceUsage = true
		fmt.Fprintf(errOut, "Not run: %s moved to a different commit while it was being checked out. Run the review again.\n", sanitizeDisplay(req.Target))
		return wrapReviewSilentError(deps.NewSilentError, errors.New("review target moved"))
	}

	env := []string{envReviewFindingsWorktree + "=" + callerWorktree}
	childArgs := reviewTargetChildArgs(cmd, req.Positional, resolved.HeadSHA, forwardProfile)
	if err := runReviewInWorktree(ctx, deps.RunInWorktree, prepared.Path, childArgs, env, cmd.InOrStdin(), cmd.OutOrStdout(), errOut); err != nil {
		return wrapReviewSilentError(deps.NewSilentError, err)
	}
	return finishTargetReview(ctx, cmd, prepared, req.CleanupWorktree, deps.RemoveTarget)
}

// reviewTargetChildFlagsDropped are handled by the parent and never reach
// the re-run inside the worktree.
var reviewTargetChildFlagsDropped = []string{"target", "cleanup-worktree", "show-config", "json", "trust-target"}

// reviewTargetChildArgs builds the re-run's arguments. The parent forwards the
// pinned head as --trust-target so the child skips the gate it already passed,
// and the profile it chose, so the child never prompts for one.
func reviewTargetChildArgs(cmd *cobra.Command, positional []string, headSHA, profile string) []string {
	args := make([]string, 0, len(positional)+cmd.Flags().NFlag()+3)
	args = append(args, "review")
	args = append(args, positional...)
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if slices.Contains(reviewTargetChildFlagsDropped, flag.Name) {
			return
		}
		args = append(args, "--"+flag.Name+"="+flag.Value.String())
	})
	if profile != "" {
		args = append(args, "--profile="+profile)
	}
	if headSHA != "" {
		args = append(args, "--trust-target="+headSHA)
	}
	return args
}

func finishTargetReview(ctx context.Context, cmd *cobra.Command, target TargetWorktree, cleanupWorktree bool, removeTarget func(context.Context, string) error) error {
	out := cmd.OutOrStdout()
	if !target.Created {
		if cleanupWorktree {
			fmt.Fprintf(out, "Kept reused worktree at %s.\n", target.Path)
		}
		return nil
	}

	remove := cleanupWorktree
	if !remove && reviewCommandIsInteractive(cmd) {
		form := newAccessibleForm(huh.NewGroup(
			huh.NewConfirm().
				Title("Remove the temporary review worktree?").
				Description(target.Path).
				Value(&remove),
		))
		// The review itself already completed. Cancelling this optional prompt
		// leaves remove=false, keeping the worktree without turning success into
		// failure.
		runOptionalCleanupPrompt(ctx, form)
	}
	if !remove {
		fmt.Fprintf(out, "Kept worktree at %s.\n", target.Path)
		return nil
	}
	if removeTarget == nil {
		return errors.New("review target cleanup is unavailable")
	}
	if err := removeTarget(ctx, target.Path); err != nil {
		return fmt.Errorf("remove review worktree %s: %w", target.Path, err)
	}
	fmt.Fprintf(out, "Removed temporary review worktree %s.\n", target.Path)
	return nil
}

func runOptionalCleanupPrompt(ctx context.Context, form *huh.Form) {
	if err := form.RunWithContext(ctx); err != nil {
		return
	}
}

func runReviewInWorktree(ctx context.Context, runner reviewWorktreeRunner, worktreeRoot string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if strings.TrimSpace(worktreeRoot) == "" {
		return errors.New("review target checkout returned an empty worktree path")
	}
	if runner != nil {
		return runner(ctx, worktreeRoot, args, env, stdin, stdout, stderr)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve entire executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = worktreeRoot
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("target review context: %w", ctx.Err())
		}
		// The child writes its own command error to stderr. The caller wraps this
		// as SilentError so the parent preserves failure without printing a
		// duplicate "exit status 1" line.
		return fmt.Errorf("target review process: %w", err)
	}
	return nil
}
