package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// Detach statuses a real detach and GET /repos/{repoId}/detach answer with.
// The wire fields are open strings (see readModelEnumFields), so anything else
// renders verbatim rather than taking one of these branches.
const (
	detachStatusComplete   = "complete"
	detachStatusInProgress = "in_progress"
	detachStatusStalled    = "stalled"
)

type mirrorDetachOptions struct {
	project string
	name    string
	dryRun  bool
	noWait  bool
	timeout time.Duration
}

func newRepoMirrorDetachCmd() *cobra.Command {
	var opts mirrorDetachOptions
	cmd := &cobra.Command{
		Use:   "detach <repo>",
		Short: "Convert a GitHub mirror into a native Entire repository",
		Long: "Converts a GitHub mirror into an Entire-native repository owned by " +
			"--project. The repository keeps its history and its cluster; its " +
			"/gh/<owner>/<repo> address is released and answers \"moved\" from then on, " +
			"and the GitHub repository itself stays live and is no longer synced.\n\n" +
			"Every run first asks the server for a plan: each precondition the " +
			"detach needs, and every account, team, automation and project with " +
			"access today, marked by whether --project still grants it. Access the " +
			"project does not cover is removed by the detach. --dry-run prints the " +
			"plan and changes nothing.\n\n" +
			"A real detach freezes writes, waits for the mirror to match GitHub, " +
			"and rewires the repository. It asks for confirmation first; pass " +
			"--yes to skip it, which a non-interactive run must. When the rewire " +
			"cannot finish in one call the repository stays frozen, and the command " +
			"waits for it to complete — through a stall the server resumes on its " +
			"own — up to --timeout. Pass --no-wait to return as soon as the " +
			"repository is native.\n\n" +
			"The mirror must have exactly one placement: remove the others with " +
			"`entire repo mirror remove` first.",
		Example: "  entire repo mirror detach /gh/octocat/hello-world --project acme --dry-run\n" +
			"  entire repo mirror detach /gh/octocat/hello-world --project acme\n" +
			"  entire repo mirror detach /gh/octocat/hello-world --project acme --name hello --yes",
		Args: cobra.ExactArgs(1),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			// Zero is an unbounded wait, matching `mirror add`.
			if opts.timeout < 0 {
				return errors.New("--timeout must be zero or positive")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMirrorDetach(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.project, projectFlagName, "", "Project that owns the repository after the detach (name or ULID) (required)")
	cmd.Flags().StringVar(&opts.name, "name", "", "Name of the native repository (defaults to the GitHub repository name)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Print the preconditions and the access changes, and change nothing")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "Return once the repository is native, without waiting for the rewire to complete")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 30*time.Minute, "How long to wait for the rewire to complete (0 waits indefinitely)")
	markRequired(cmd, projectFlagName)
	// Only --yes: --force/-f reads as overriding a refusal, and an ineligible
	// plan cannot be overridden — the flag only answers the prompt.
	cmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	addJSONFlag(cmd)
	return cmd
}

// runMirrorDetach resolves the mirror and the target project, asks the server
// for the plan, and — unless this is a dry run — confirms and runs the detach.
//
// The plan is always fetched first, even for a real detach the server would
// check again on its own: it is what the confirmation shows (who loses access)
// and it turns a refusal into the list of failed preconditions instead of one
// problem message.
func runMirrorDetach(cmd *cobra.Command, repoRef string, opts mirrorDetachOptions) error {
	cmd.SilenceUsage = true
	ref, err := parseMirrorRepoRef(repoRef, mirrorCloneForge)
	if err != nil {
		return err
	}
	yes, _ := cmd.Flags().GetBool("yes") //nolint:errcheck // registered above
	// An unanswerable prompt must not cost a request: settle it from the
	// command line before anything is resolved.
	if !opts.dryRun && !yes && !detachCanPrompt() {
		return fmt.Errorf("refusing to detach %s without confirmation; pass --yes, or --dry-run to only see the plan", ref.qualified())
	}

	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		projectID, projectName, err := resolveProjectRefNamed(ctx, c, opts.project)
		if err != nil {
			return err
		}
		repoID, err := resolveDetachRepoID(ctx, c, ref)
		if err != nil {
			return err
		}
		body := coreapi.DetachRepoBody{TargetProject: projectID, DryRun: true}
		if opts.name != "" {
			body.Name = coreapi.NewOptString(opts.name)
		}
		params := coreapi.DetachRepoParams{RepoId: repoID}

		plan, err := c.DetachRepo(ctx, &body, params)
		if err != nil {
			return fmt.Errorf("plan the detach of %s: %w", ref.qualified(), err)
		}
		target := nativeRepoPath(projectName + "/" + plan.Name)
		// Names are for people reading tables: the plan, the prompt's copy of
		// it, and the removed-access list. Read them now, while the repo's
		// people still include everyone the detach is about to remove.
		var names detachNames
		if !jsonRequested(cmd) || (!opts.dryRun && plan.Eligible && !yes) {
			names = lookupDetachNames(ctx, c, repoID)
		}
		if opts.dryRun || !plan.Eligible {
			if err := renderDetachPlan(cmd, ref, target, plan, names); err != nil {
				return err
			}
			if !plan.Eligible && !opts.dryRun {
				return fmt.Errorf("%s cannot be detached; failed: %s", ref.qualified(), strings.Join(failedDetachPreconditions(plan), ", "))
			}
			return nil
		}

		if !yes {
			proceed, err := detachConfirmed(cmd, ref, target, plan, names)
			if err != nil || !proceed {
				return err
			}
		}

		body.DryRun = false
		res, err := c.DetachRepo(ctx, &body, params)
		if err != nil {
			if !detachRefusedOutright(err) {
				// No answer, a 5xx, or an interruption: the server may have
				// frozen and rewired the repo all the same, and the /gh/ ref
				// may already answer moved.
				fmt.Fprintf(cmd.ErrOrStderr(), "The detach may have started. %s\n", detachFollowUpHint(repoID))
			}
			return fmt.Errorf("detach %s: %w", ref.qualified(), err)
		}
		return finishDetach(cmd, c, ref, repoID, projectID, res, names, opts)
	})
}

// finishDetach waits on a detach the server answered as unfinished, renders
// where it ended, and decides the exit. The write has happened by now, so
// every way out that leaves the repository frozen says how to follow it up:
// the /gh/ ref this command takes answers "moved" from here on, so re-running
// it cannot reach the detach again.
func finishDetach(cmd *cobra.Command, c detachStateGetter, ref mirrorRepoRef, repoID, projectID string, res *coreapi.DetachRepoResult, names detachNames, opts mirrorDetachOptions) error {
	errW := cmd.ErrOrStderr()
	var (
		state   *coreapi.RepoDetachState
		waitErr error
	)
	if !opts.noWait && detachUnfinished(res.Status.Or("")) {
		fmt.Fprintf(errW, "Waiting for the detach of %s to complete…\n", ref.qualified())
		state, waitErr = awaitDetach(cmd.Context(), c, repoID, opts.timeout, func(step int64, name string) {
			fmt.Fprintf(errW, "  step %d finished (%s)\n", step, cmp.Or(name, "-"))
		})
		if state != nil {
			mergeDetachState(res, state)
		}
	}
	needsResume := state != nil && state.Status == detachStatusStalled && !state.Resumable
	// Only a state read says whether the server resumes a stall on its own;
	// the detach's own answer does not.
	var resumable *bool
	if state != nil {
		resumable = &state.Resumable
	}

	var err error
	if jsonRequested(cmd) {
		err = printJSON(cmd.OutOrStdout(), res)
	} else {
		err = renderDetachResult(cmd.OutOrStdout(), ref, res, names, resumable)
	}
	if err != nil {
		return err
	}

	status := res.Status.Or("")
	switch {
	case waitErr != nil:
		fmt.Fprintln(errW, detachFollowUpHint(repoID))
		var silent *SilentError
		if errors.As(waitErr, &silent) {
			// An interruption: main re-raises the signal it recorded.
			return waitErr
		}
		// Rendered here rather than by runCore: renderCoreError keeps only an
		// API problem's detail, which would drop that the detach ran.
		return fmt.Errorf("%s; the detach of %s carries on on the server", renderCoreError(waitErr).Error(), ref.qualified())
	case needsResume:
		fmt.Fprintln(errW, detachResumeHint(repoID, projectID))
		return fmt.Errorf("the detach of %s stalled and needs an admin of the target project to resume it", ref.qualified())
	case status == detachStatusComplete:
		return nil
	case opts.noWait && detachUnfinished(status):
		fmt.Fprintln(errW, detachFollowUpHint(repoID))
		return nil
	default:
		fmt.Fprintln(errW, detachFollowUpHint(repoID))
		return fmt.Errorf("the detach of %s answered an unexpected status %s", ref.qualified(), strconv.Quote(status))
	}
}

// detachRefusedOutright reports a 4xx answer to the real call: the server
// refused before changing anything. Anything else leaves it unknown.
func detachRefusedOutright(err error) bool {
	var se *coreapi.ErrorModelStatusCode
	return errors.As(err, &se) && se.StatusCode >= 400 && se.StatusCode < 500
}

func detachFollowUpHint(repoID string) string {
	return "Follow the detach with: entire api /api/v1/repos/" + repoID + "/detach"
}

func detachResumeHint(repoID, projectID string) string {
	return "An admin of the target project can resume it with: entire api -X POST /api/v1/repos/" + repoID +
		"/detach -f targetProject=" + projectID + " -F dryRun=false"
}

func detachUnfinished(status string) bool {
	return status == detachStatusInProgress || status == detachStatusStalled
}

// errDetachNotRecorded is a state read that contradicts the detach's own
// answer: it said the repository is native, and the state says no detach
// exists.
var errDetachNotRecorded = errors.New("the server reports no detach recorded")

// detachStateGetter is the one call awaitDetach makes, so a test can script it.
type detachStateGetter interface {
	GetRepoDetach(ctx context.Context, params coreapi.GetRepoDetachParams) (*coreapi.RepoDetachState, error)
}

// awaitDetach polls the detach's state until the rewire completes, or stalls
// in a way the server will not resume on its own. A resumable stall keeps the
// wait going: the core's sweep picks it back up. It returns the last state
// read, including on a timeout, so the caller can report how far it got, and
// calls progress once for each newly reported finished step; a state without
// a step (before the group rewrite) reports nothing.
func awaitDetach(ctx context.Context, c detachStateGetter, repoID string, timeout time.Duration, progress func(step int64, name string)) (*coreapi.RepoDetachState, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(mirrorPollInterval)
	defer ticker.Stop()

	var last *coreapi.RepoDetachState
	var consecutiveErrs int
	reported := int64(-1)
	for {
		state, err := c.GetRepoDetach(ctx, coreapi.GetRepoDetachParams{RepoId: repoID})
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return last, classifyWaitContextErr(ctx.Err(), "waiting for the detach")
			}
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutivePollErrors {
				return last, fmt.Errorf("poll detach status: %w", err)
			}
		case state.Status == "none":
			return last, errDetachNotRecorded
		default:
			consecutiveErrs = 0
			if step, ok := state.Step.Get(); ok && step != reported && progress != nil {
				reported = step
				progress(step, state.StepName.Or(""))
			}
			last = state
			switch state.Status {
			case detachStatusInProgress:
			case detachStatusStalled:
				if !state.Resumable {
					return state, nil
				}
			default:
				// complete, or a status this client does not know: either way
				// there is nothing a wait can add, and the caller decides.
				return state, nil
			}
		}
		select {
		case <-ctx.Done():
			return last, classifyWaitContextErr(ctx.Err(), "waiting for the detach")
		case <-ticker.C:
		}
	}
}

// mergeDetachState folds the polled state into the detach's answer, so both
// renderings report where the detach ended rather than where it started. A
// completed detach has nothing left to poll, so its statusUrl goes.
func mergeDetachState(res *coreapi.DetachRepoResult, state *coreapi.RepoDetachState) {
	res.Status = coreapi.NewOptString(state.Status)
	if name, ok := state.NativeName.Get(); ok {
		res.NativeName = coreapi.NewOptString(name)
	}
	if len(state.ReleasedAddresses) > 0 {
		res.ReleasedAddresses = state.ReleasedAddresses
	}
	if state.Status == detachStatusComplete {
		res.StatusUrl = coreapi.OptString{}
	}
}

// resolveDetachRepoID finds the mirror's placement ID, which is what the
// detach route is keyed by. A detach needs exactly one placement; with several
// the first is sent anyway, so the server's single-placement precondition is
// the one that explains the refusal rather than a client-side guess at it.
func resolveDetachRepoID(ctx context.Context, c *coreapi.Client, ref mirrorRepoRef) (string, error) {
	placements, err := resolvePullablePlacements(ctx, c, ref.owner, ref.repo)
	if err != nil {
		return "", err
	}
	if len(placements) == 0 {
		return "", fmt.Errorf("%s is not mirrored on any cluster you can read; see `entire repo mirror list`", ref.qualified())
	}
	return placements[0].MirrorId, nil
}

// detachCanPrompt is a seam so a test can reach the confirmation, which
// `go test`'s missing terminal otherwise refuses before any request.
var detachCanPrompt = interactive.CanPromptInteractively

// detachConfirmed is the seam the confirmation sits behind, as revokeConfirmed
// is for grants: the form needs a terminal, which `go test` does not have.
//
// The plan is written on the prompt's own writer, ahead of the form: it is
// what the question asks about, so it follows the prompt, and a redirected
// stdout keeps only what the command did. The consequences also go in the
// Title, not a Description: huh's accessible mode renders only the title.
var detachConfirmed = func(cmd *cobra.Command, ref mirrorRepoRef, target string, plan *coreapi.DetachRepoResult, names detachNames) (bool, error) {
	if err := detachInterrupted(cmd); err != nil {
		return false, err
	}
	confirmed := false
	prompt := huh.NewConfirm().Title(detachConfirmTitle(ref, target, plan)).Value(&confirmed)
	render, err := runPromptFormAfter(cmd, NewAccessibleForm(huh.NewGroup(prompt)), func(w io.Writer) error {
		if err := writeDetachPlan(w, ref, target, plan, names); err != nil {
			return err
		}
		fmt.Fprintln(w)
		return nil
	})
	// Before the form error is looked at: handleFormCancellation treats
	// context.Canceled as a clean abort and would report a signal as an answer.
	if ierr := detachInterrupted(cmd); ierr != nil {
		return false, ierr
	}
	if err != nil {
		if cerr := handleFormCancellation(render, "Detach", err); cerr != nil {
			return false, cerr
		}
		return false, nil
	}
	if !confirmed {
		fmt.Fprintln(render, "Detach cancelled.")
		return false, nil
	}
	return true, nil
}

// detachInterrupted reports a command context cancelled out from under the
// confirmation, which is an interruption and not an answer (see
// revocationInterrupted).
func detachInterrupted(cmd *cobra.Command) error {
	if err := cmd.Context().Err(); err != nil {
		return fmt.Errorf("detach cancelled: %w", err)
	}
	return nil
}

func detachConfirmTitle(ref mirrorRepoRef, target string, plan *coreapi.DetachRepoResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Detach %s into %s? Writes freeze until the rewire finishes", ref.qualified(), target)
	if lost := len(detachLostAccess(plan.Access)); lost > 0 {
		fmt.Fprintf(&b, ", and %d access %s the project does not cover will be removed", lost, pluralize("source", lost))
	}
	b.WriteString(".")
	return b.String()
}

func failedDetachPreconditions(plan *coreapi.DetachRepoResult) []string {
	var failed []string
	for _, p := range plan.Preconditions {
		if !p.Passed {
			failed = append(failed, p.Precondition)
		}
	}
	return failed
}

func detachKeptAccess(access []coreapi.DetachAccessEntry) []coreapi.DetachAccessEntry {
	var kept []coreapi.DetachAccessEntry
	for _, a := range access {
		if a.CoveredByTargetProject {
			kept = append(kept, a)
		}
	}
	return kept
}

func detachLostAccess(access []coreapi.DetachAccessEntry) []coreapi.DetachAccessEntry {
	var lost []coreapi.DetachAccessEntry
	for _, a := range access {
		if !a.CoveredByTargetProject {
			lost = append(lost, a)
		}
	}
	return lost
}

var (
	detachPreconditionColumns = []string{"PRECONDITION", "RESULT", "DETAIL"}
	detachAccessColumns       = []string{"SUBJECT", colHeaderName, colHeaderRole, colHeaderSource}
)

func detachPreconditionRow(p coreapi.DetachPrecondition) []string {
	result := "fail"
	if p.Passed {
		result = "pass"
	}
	return []string{p.Precondition, result, p.Detail.Or("")}
}

// detachNames maps an account ID to the repo's people entry for it. The
// detach API names every subject by ID only, and a column of ULIDs is not
// something a reader can check before removing access.
type detachNames map[string]coreapi.ResourcePerson

// lookupDetachNames reads the repo's people once. It is best-effort: a failed
// or truncated read leaves the subjects it missed shown by ID, which is less
// readable but never wrong.
func lookupDetachNames(ctx context.Context, c *coreapi.Client, repoID string) detachNames {
	people, _, err := fetchPagesBounded(ctx, coreListFetchBudget, func(ctx context.Context, cursor string) ([]coreapi.ResourcePerson, string, error) {
		params := coreapi.ListRepoPeopleParams{RepoId: repoID, PageSize: coreapi.NewOptInt32(500)}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := c.ListRepoPeople(ctx, params)
		if err != nil {
			return nil, "", fmt.Errorf("list repo people: %w", err)
		}
		return out.Items, out.NextPageToken.Or(""), nil
	})
	if err != nil {
		logging.Debug(ctx, "repo mirror detach: people lookup failed; showing subject IDs", "error", err)
	}
	names := make(detachNames, len(people))
	for _, p := range people {
		names[p.AccountId] = p
	}
	return names
}

// subject names an access entry the way the grant commands name a grantee:
// an account by its provider:handle, anything else (or an account the people
// listing did not return) by type and ID.
func (n detachNames) subject(a coreapi.DetachAccessEntry) string {
	if p, ok := n[a.SubjectId]; ok && a.SubjectType == granteeTypeAccount {
		if handle := p.Handle.Or(""); handle != "" {
			return handle
		}
	}
	return a.SubjectType + ":" + a.SubjectId
}

func (n detachNames) row(a coreapi.DetachAccessEntry) []string {
	name := "-"
	if p, ok := n[a.SubjectId]; ok && a.SubjectType == granteeTypeAccount {
		name = cmp.Or(p.DisplayName.Or(""), "-")
	}
	return []string{n.subject(a), name, a.Role, a.Source}
}

// sorted orders entries by how they read, so a long list can be scanned for
// a name.
func (n detachNames) sorted(entries []coreapi.DetachAccessEntry) []coreapi.DetachAccessEntry {
	out := slices.Clone(entries)
	slices.SortStableFunc(out, func(a, b coreapi.DetachAccessEntry) int {
		return cmp.Compare(strings.ToLower(n.subject(a)), strings.ToLower(n.subject(b)))
	})
	return out
}

// writeDetachAccess prints one titled group of access entries; an empty group
// prints nothing.
func writeDetachAccess(w io.Writer, title string, names detachNames, entries []coreapi.DetachAccessEntry) error {
	if len(entries) == 0 {
		return nil
	}
	fmt.Fprintf(w, "%s (%d):\n", title, len(entries))
	if err := printTable(w, detachAccessColumns, names.sorted(entries), names.row); err != nil {
		return err
	}
	fmt.Fprintln(w)
	return nil
}

// renderDetachPlan prints the plan as the command's output: --json as the
// wire result, the human view as the two tables a reader decides on.
func renderDetachPlan(cmd *cobra.Command, ref mirrorRepoRef, target string, plan *coreapi.DetachRepoResult, names detachNames) error {
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), plan)
	}
	return writeDetachPlan(cmd.OutOrStdout(), ref, target, plan, names)
}

// writeDetachPlan prints the preconditions, then the access split by what the
// detach does to it: who loses access is what a reader has to check, so it
// comes first and is not interleaved with who keeps it.
func writeDetachPlan(w io.Writer, ref mirrorRepoRef, target string, plan *coreapi.DetachRepoResult, names detachNames) error {
	fmt.Fprintf(w, "Detach plan: %s → %s\n\n", ref.qualified(), target)
	if err := printTable(w, detachPreconditionColumns, plan.Preconditions, detachPreconditionRow); err != nil {
		return err
	}
	fmt.Fprintln(w)
	if len(plan.Access) == 0 {
		fmt.Fprintln(w, "No access sources.")
		fmt.Fprintln(w)
	}
	lost := detachLostAccess(plan.Access)
	if err := writeDetachAccess(w, "Loses access", names, lost); err != nil {
		return err
	}
	if err := writeDetachAccess(w, "Keeps access", names, detachKeptAccess(plan.Access)); err != nil {
		return err
	}
	if plan.Eligible {
		fmt.Fprintf(w, "Eligible. %d access %s would be removed.\n", len(lost), pluralize("source", len(lost)))
		return nil
	}
	failed := failedDetachPreconditions(plan)
	fmt.Fprintf(w, "Not eligible: %d %s failed.\n", len(failed), pluralize("precondition", len(failed)))
	return nil
}

// renderDetachResult prints a real detach's answer. Every status but complete
// leaves the repository frozen, so each says so and what happens next.
//
// resumable is what a state read said about a stall, nil when none was made.
func renderDetachResult(w io.Writer, ref mirrorRepoRef, res *coreapi.DetachRepoResult, names detachNames, resumable *bool) error {
	native := ref.qualified()
	if name := res.NativeName.Or(""); name != "" {
		native = "/" + strings.TrimPrefix(name, "/")
	}
	switch status := res.Status.Or(""); status {
	case detachStatusComplete:
		fmt.Fprintf(w, "✓ Detached %s into %s\n", ref.qualified(), native)
	case detachStatusInProgress:
		fmt.Fprintf(w, "Detach of %s is in progress: it is now %s, and writes stay frozen until the rewire finishes.\n", ref.qualified(), native)
	case detachStatusStalled:
		resumer := "the rewire is resumed"
		switch {
		case resumable == nil:
		case *resumable:
			resumer = "the server's sweep resumes the rewire"
		default:
			resumer = "an admin of the target project resumes the rewire"
		}
		fmt.Fprintf(w, "Detach of %s stalled: it is now %s, and writes stay frozen until %s.\n", ref.qualified(), native, resumer)
	default:
		fmt.Fprintf(w, "Detach of %s answered status %s; %s is its native address.\n", ref.qualified(), strconv.Quote(status), native)
	}
	if len(res.ReleasedAddresses) > 0 {
		released := make([]string, len(res.ReleasedAddresses))
		for i, a := range res.ReleasedAddresses {
			released[i] = "/" + strings.TrimPrefix(a, "/")
		}
		fmt.Fprintf(w, "Released: %s\n", strings.Join(released, ", "))
	}
	// lostAccess is absent on a resume, which does not know who lost access;
	// say nothing rather than claim nobody did.
	if res.LostAccess != nil {
		if len(res.LostAccess) == 0 {
			fmt.Fprintln(w, "No access was removed.")
		} else {
			fmt.Fprintln(w)
			if err := writeDetachAccess(w, "Removed access", names, res.LostAccess); err != nil {
				return err
			}
		}
	}
	for _, n := range res.Notices {
		fmt.Fprintf(w, "Note: %s\n", n)
	}
	return nil
}
