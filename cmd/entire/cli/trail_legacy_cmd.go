package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
)

// Legacy collection and deletion use repository-local identities. They are not
// fallbacks for a failed project request: only the legacy command tree exposes them.
func newTrailListCmd() *cobra.Command {
	var opts trailListOptions
	cmd := &cobra.Command{
		Use: cmdList, Short: "List recent trails", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.InsecureHTTP = trailInsecureHTTP(cmd)
			opts.Repo = trailRepoFlag(cmd)
			return runTrailListAll(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.Author, "author", "", "Filter by author login (case-insensitive); use '"+trailListAuthorMe+"' for yourself (requires gh CLI); omit for any author")
	cmd.Flags().StringVar(&opts.Status, "status", defaultTrailListStatus, "Filter by comma-separated status(es): "+formatValidStatuses()+"; use '"+trailListStatusAny+"' for all statuses")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON (respects --author, --status, and --limit)")
	cmd.Flags().IntVarP(&opts.Limit, "limit", "n", defaultTrailListLimit, "Maximum number of trails to show")
	return cmd
}

func newTrailDeleteCmd() *cobra.Command {
	var branch string
	var force bool
	cmd := &cobra.Command{
		Use: "delete [<number>]", Short: "Delete a trail",
		Long: `Delete a trail by number, or the trail for a branch.

If <number> is omitted, the trail for --branch (or the current branch) is used.
Deletion is permanent; you are prompted to confirm unless --force is passed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := parseTrailNumberArg(args)
			if err != nil {
				return err
			}
			if number > 0 && cmd.Flags().Changed("branch") {
				return errors.New("cannot combine a trail <number> with --branch")
			}
			if err := ensureTrailRepoHasTarget(cmd, number > 0 || strings.TrimSpace(branch) != "", "pass a trail number or --branch"); err != nil {
				return err
			}
			return runTrailDelete(cmd, number, branch, force)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch whose trail to delete (defaults to current)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip the confirmation prompt")
	return cmd
}

func runTrailDelete(cmd *cobra.Command, number int, branch string, force bool) error {
	ctx, w := cmd.Context(), cmd.OutOrStdout()
	return runAuthenticatedTrailAPI(ctx, cmd.ErrOrStderr(), trailInsecureHTTP(cmd), trailRepoFlag(cmd), func(ctx context.Context, client *api.Client, repoID string) error {
		forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, trailRepoFlag(cmd))
		if err != nil {
			return err
		}
		basePath, err := trailRepoBasePath(forge, owner, repo, repoID)
		if err != nil {
			return err
		}
		title := ""
		if number == 0 {
			branch, err = resolveTrailBranch(ctx, branch)
			if err != nil {
				return err
			}
			found, err := findTrailByBranchAtPath(ctx, client, basePath, branch)
			if err != nil {
				return err
			}
			if found == nil {
				return fmt.Errorf("no trail found for branch %q", branch)
			}
			if found.Number <= 0 {
				return fmt.Errorf("trail for branch %q has no number yet; cannot delete", branch)
			}
			number, title = found.Number, found.Title
		} else if found, err := findTrailByNumberAtPath(ctx, client, basePath, number); err == nil && found != nil {
			title = found.Title
		}
		proceed, err := confirmTrailDeletion(ctx, w, number, title, force, interactive.CanPromptInteractively())
		if err != nil {
			return err
		}
		if !proceed {
			return nil
		}
		if err := deleteTrailByNumberAtPath(ctx, client, basePath, number); err != nil {
			return err
		}
		fmt.Fprintf(w, "Deleted trail #%d\n", number)
		return nil
	})
}

// Shared operations use the same review/session/checkout code, but legacy
// selection never resolves or follows a project parent (even when one is sent).
func resolveLegacyTrailContext(cmd *cobra.Command, selector, branch string, localOnly bool) (*trailWorkingContext, error) {
	if selector != "" && strings.TrimSpace(branch) != "" && !localOnly {
		return nil, errors.New("pass a trail selector or --branch, not both")
	}
	repoOverride := trailRepoFlag(cmd)
	if localOnly {
		repoOverride = ""
	}
	if err := requireTrailWorkingTarget(repoOverride, selector, branch); err != nil {
		return nil, err
	}
	var selected *trailWorkingContext
	err := runAuthenticatedTrailAPI(cmd.Context(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), repoOverride, func(ctx context.Context, client *api.Client, repoID string) error {
		forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, repoOverride)
		if err != nil {
			return err
		}
		base, err := trailRepoBasePath(forge, owner, repo, repoID)
		if err != nil {
			return err
		}
		found, err := resolveNumberedTrailAtPath(ctx, client, base, forge, owner, repo, selector, branch)
		if err != nil {
			return err
		}
		found.Parent = nil
		client.SetTrailRoute(found.ID, trailNumberPathForBase(base, found.Number))
		selected = &trailWorkingContext{Client: client, BasePath: base, Host: forge, Owner: owner, Repo: repo, Work: *found}
		return nil
	})
	return selected, err
}

// Preserve the legacy finding resolver's missing-default sentinel: a bare
// finding dashboard with no trail is an empty view, not a project lookup error.
func authenticatedLegacyTrailReviewTarget(cmd *cobra.Command, selector string) (*api.Client, trailReviewTarget, error) {
	repo, branch := trailRepoFlag(cmd), trailBranchFlag(cmd)
	if selector != "" && branch != "" {
		return nil, trailReviewTarget{}, errors.New("pass a trail selector or --branch, not both")
	}
	if err := requireTrailWorkingTarget(repo, selector, branch); err != nil {
		return nil, trailReviewTarget{}, err
	}
	var client *api.Client
	var target trailReviewTarget
	err := runAuthenticatedTrailAPI(cmd.Context(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), repo, func(ctx context.Context, c *api.Client, repoID string) error {
		var err error
		client = c
		target, err = resolveTrailReviewTarget(ctx, c, repoID, selector, repo, branch)
		target.Trail.Parent = nil
		return err
	})
	return client, target, err
}

func configureLegacyTrailHelp(root *cobra.Command) {
	const findingCommand = "finding"
	for _, name := range []string{"checkout", "resume", findingCommand, "watch", "approve", "request-changes", "approvals"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			panic(err)
		}
		// Keep operational details (SSE events, checkout/resume safety) while
		// replacing the project-selector paragraphs with the legacy contract.
		start, end := strings.Index(cmd.Long, "The trail may be given"), -1
		if start >= 0 {
			end = strings.Index(cmd.Long[start:], "\n\n")
		}
		if start < 0 {
			start = strings.Index(cmd.Long, "<trail> is a project")
			if start >= 0 {
				end = strings.Index(cmd.Long[start:], "\n\n")
			}
		}
		if start >= 0 {
			if end < 0 {
				end = len(cmd.Long) - start
			}
			cmd.Long = cmd.Long[:start] + "The trail may be a repository-local number, ID, or branch. Without a selector,\nthe current branch (or --branch) is used." + cmd.Long[start+end:]
		}
		if name == findingCommand {
			cmd.Long = "Manage a trail's agent-native findings. Pass a repository-local number, ID, or branch, or omit the selector for the current branch. Use 'entire trail list --status any' to discover trails."
		}
		if name == "watch" {
			cmd.Short = "Tail a trail's events live"
		}
		if f := cmd.Flags().Lookup("branch"); f != nil && name != "resume" {
			f.Usage = "Resolve the trail for this branch instead of the current branch"
		}
		if f := cmd.Flags().Lookup("trail"); f != nil {
			f.Usage = "Trail selector (number, ID, or branch); defaults to the current branch's trail"
		}
		if f := cmd.PersistentFlags().Lookup("trail"); f != nil {
			f.Usage = "Trail selector (number, ID, or branch); defaults to the current branch's trail"
		}
		if f := cmd.PersistentFlags().Lookup("branch"); f != nil {
			f.Usage = "Resolve the trail for this branch instead of the current branch; cannot be combined with a trail selector"
		}
	}
}
