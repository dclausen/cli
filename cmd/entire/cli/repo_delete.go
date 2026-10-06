package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// repoDeleteOptions are the cascade flags of `repo delete`.
type repoDeleteOptions struct {
	cascade     bool
	noWait      bool
	waitTimeout time.Duration
}

func newRepoDeleteCmd() *cobra.Command {
	var (
		project string
		opts    repoDeleteOptions
	)
	cmd := &cobra.Command{
		Use:   "delete <repo>",
		Short: "Delete a repository by /et/<project>/<repo> path, name, or ULID",
		Long: "Delete a repository.\n\n" +
			"A native repo with copies on other clusters (see `entire repo mirror add`) " +
			"is refused until the copies are removed. Pass --cascade to delete the repo " +
			"and every copy in one command: the server removes the copies first, and " +
			"the command waits until the repo is gone. --no-wait returns as soon as the " +
			"server accepts the request.",
		Example: "  entire repo delete /et/acme/web\n" +
			"  entire repo delete /et/acme/web --cascade\n" +
			"  entire repo delete /et/acme/web --cascade --no-wait",
		Args: cobra.ExactArgs(1),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if opts.noWait && !opts.cascade {
				return errors.New("--no-wait requires --cascade")
			}
			if opts.waitTimeout <= 0 {
				return errors.New("--wait-timeout must be positive")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoDelete(cmd, args[0], project, opts)
		},
	}
	bindRepoProjectFlag(cmd, &project)
	addForceFlag(cmd)
	cmd.Flags().BoolVar(&opts.cascade, "cascade", false, "Also delete the repo's copies on other clusters")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "Return once the server accepts a cascade delete")
	cmd.Flags().DurationVar(&opts.waitTimeout, "wait-timeout", 10*time.Minute, "Time limit for a cascade delete to finish")
	return cmd
}

// runRepoDelete is `repo delete`. It is not runControlPlaneDelete because a
// cascade can answer 202: the server then owns completion, and the command
// polls until the repo is gone instead of reporting a finished delete.
func runRepoDelete(cmd *cobra.Command, ref, project string, opts repoDeleteOptions) error {
	force := forceRequested(cmd)
	out := cmd.OutOrStdout()
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		resolved, err := resolveRepoRefResolved(ctx, c, ref, project)
		if err != nil {
			return err
		}
		label := "repo " + resolvedRefLabel(ref, resolved)
		copies := ""
		if opts.cascade {
			copies = copiesSuffix(ctx, c, resolved.ID)
		}
		proceed, err := confirmControlPlaneDeletion(ctx, out, label+copies, force, interactive.CanPromptInteractively())
		if err != nil || !proceed {
			return err
		}
		params := coreapi.DeleteRepoParams{RepoId: resolved.ID}
		if opts.cascade {
			params.Cascade = coreapi.NewOptBool(true)
		}
		res, err := c.DeleteRepo(ctx, params)
		switch {
		case isCoreNotFound(err):
			fmt.Fprintf(out, "%s not found; nothing to delete\n", label)
			return nil
		case err != nil && opts.cascade:
			return err
		case err != nil:
			return hintCascadeOnMirrorConflict(err)
		}
		if _, accepted := res.(*coreapi.DeleteRepoAccepted); !accepted {
			fmt.Fprintf(out, "✓ Deleted %s\n", label)
			return nil
		}
		if opts.noWait {
			fmt.Fprintf(out, "Deleting %s%s in the background.\n", label, copies)
			return nil
		}
		fmt.Fprintf(out, "Deleting %s%s…\n", label, copies)
		waitCtx, cancel := context.WithTimeout(ctx, opts.waitTimeout)
		defer cancel()
		if err := awaitRepoDeleted(waitCtx, c, resolved.ID); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Deleted %s\n", label)
		return nil
	})
}

// copiesSuffix names the copies a cascade removes, for the prompt and the
// progress line. Best-effort: the server decides what the cascade removes,
// so a failed count degrades the wording rather than the command.
func copiesSuffix(ctx context.Context, c *coreapi.Client, repoID string) string {
	mirrors, err := listNativeMirrors(ctx, c, repoID)
	switch {
	case err != nil:
		return " and its copies"
	case len(mirrors) == 0:
		return ""
	case len(mirrors) == 1:
		return " and its copy"
	default:
		return fmt.Sprintf(" and its %d copies", len(mirrors))
	}
}

// hintCascadeOnMirrorConflict adds the --cascade hint to the one refusal
// whose remedy is a flag of this command: the server declines to delete a
// native primary while its copies exist. The hint rides on a plain error.
// renderCoreError replaces a wrapped *ErrorModelStatusCode with the server's
// detail alone, which would drop the hint (see renderNativeMirrorCreateError).
func hintCascadeOnMirrorConflict(err error) error {
	var problem *coreapi.ErrorModelStatusCode
	if !errors.As(err, &problem) || problem.StatusCode != http.StatusConflict {
		return err
	}
	detail := coreapi.APIError(err)
	if !strings.Contains(strings.ToLower(detail), "native mirror") {
		return err
	}
	return fmt.Errorf("%s; add --cascade to delete its copies too", detail)
}

// awaitRepoDeleted polls until the repo read answers 404. A 202 hands
// completion to the server, so every 200 keeps the wait going whatever
// state it reports: the server removes the row once the last copy is gone.
func awaitRepoDeleted(ctx context.Context, c repoLifecycleGetter, repoID string) error {
	ticker := time.NewTicker(mirrorPollInterval)
	defer ticker.Stop()

	var consecutiveErrs int
	for {
		_, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
		switch {
		case isCoreNotFound(err):
			return nil
		case err != nil:
			if ctx.Err() != nil {
				return classifyWaitContextErr(ctx.Err(), "waiting for the repository to be deleted")
			}
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutivePollErrors {
				return fmt.Errorf("poll repository: %w", err)
			}
		default:
			consecutiveErrs = 0
		}
		select {
		case <-ctx.Done():
			return classifyWaitContextErr(ctx.Err(), "waiting for the repository to be deleted")
		case <-ticker.C:
		}
	}
}
