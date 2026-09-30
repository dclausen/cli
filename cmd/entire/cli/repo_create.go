package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// repoCreateRequest is everything `repo create` sends, however it was
// gathered: from the command line in one shot, or from the wizard. An empty
// objectFormat or visibility leaves that setting to the server's default.
type repoCreateRequest struct {
	projectID string
	// projectName is the server's name for the project, used for display and
	// to spell the /et/<project>/<repo> ref in the next steps. Empty when the
	// project was given as a ULID and never looked up.
	projectName  string
	name         string
	objectFormat coreapi.CreateRepoInputBodyObjectFormat
	visibility   coreapi.SetRepoVisibilityInputBodyVisibility
}

// repoCreateOptions are the readiness settings shared by both entry points.
type repoCreateOptions struct {
	noWait      bool
	waitTimeout time.Duration
}

// errRepoCreateNeedsInput refuses a run that cannot be prompted and was not
// given everything a create needs, naming the flag form instead.
var errRepoCreateNeedsInput = errors.New("a repository name and --project are required without an interactive terminal: " +
	"entire repo create <name> --project <project>")

// createRepo sends the create request and returns the created repo in the
// shape the readiness wait and the report share.
func createRepo(ctx context.Context, c *coreapi.Client, req repoCreateRequest) (*coreapi.Repo, error) {
	body := &coreapi.CreateRepoInputBody{Name: req.name, ProjectId: req.projectID}
	if req.objectFormat != "" {
		body.ObjectFormat = coreapi.NewOptCreateRepoInputBodyObjectFormat(req.objectFormat)
	}
	response, err := c.CreateRepo(ctx, body)
	if err != nil {
		return nil, err
	}
	return createdRepoAsRepo(&response.Response)
}

// isRepoNameConflict reports the server refusing a create because the name is
// already taken in the project.
func isRepoNameConflict(err error) bool {
	var se *coreapi.ErrorModelStatusCode
	return errors.As(err, &se) && se.StatusCode == http.StatusConflict
}

// finishRepoCreate runs everything after a successful create: the readiness
// wait, the visibility change, and the report. The repo exists by now, so
// every failure from here on is reported with the repository preserved rather
// than as a failed create — creating again is never the recovery.
//
// Visibility is set after the wait so it lands on a provisioned repo; with
// --no-wait it is set straight away.
func finishRepoCreate(ctx context.Context, cmd *cobra.Command, c *coreapi.Client, req repoCreateRequest, created *coreapi.Repo, opts repoCreateOptions) error {
	var waitErr error
	if !opts.noWait {
		var finish func(bool)
		waitErr = awaitRepoActive(ctx, c, created, func() {
			finish = startSpinner(cmd.ErrOrStderr(), "Waiting for repository "+created.Name+" to become active")
		})
		if finish != nil {
			finish(waitErr == nil)
		}
	}
	ref := repoCreateRef(created, req.projectName)
	visErr := applyRepoVisibility(ctx, c, created, req.visibility)
	if visErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "The repository was created, but setting its visibility to %s failed: %v\nSet it with: entire repo edit %s --visibility %s\n",
			req.visibility, renderCoreError(visErr), cmp.Or(ref, created.ID), req.visibility)
	}
	reportErr := reportRepoCreation(cmd, created, ref, opts.noWait, waitErr)
	if visErr != nil {
		return NewSilentError(errors.Join(visErr, reportErr))
	}
	if reportErr == nil && !jsonRequested(cmd) {
		printRepoCreateNextSteps(cmd.OutOrStdout(), ref)
	}
	return reportErr
}

// applyRepoVisibility sets the requested visibility on a freshly created repo.
// The create endpoint takes no visibility, so this is a second call. It is
// skipped when nothing was asked for, or when the create response already
// reports the requested value — the common case of asking for private on a
// server that defaults to it. On success the repo's visibility is updated so
// the report (and --json) shows what now holds.
func applyRepoVisibility(ctx context.Context, c *coreapi.Client, created *coreapi.Repo, vis coreapi.SetRepoVisibilityInputBodyVisibility) error {
	if vis == "" {
		return nil
	}
	if current, ok := created.Visibility.Get(); ok && current == string(vis) {
		return nil
	}
	out, err := c.SetRepoVisibility(ctx, &coreapi.SetRepoVisibilityInputBody{Visibility: vis}, coreapi.SetRepoVisibilityParams{RepoId: created.ID})
	if err != nil {
		return fmt.Errorf("set repository visibility: %w", err)
	}
	created.Visibility = coreapi.NewOptString(string(out.Visibility))
	return nil
}

// repoCreateRef is the /et/<project>/<repo> ref the output names the new repo
// by. The server's full name wins; otherwise it is composed from the project
// name the command resolved. Empty when neither is known (a project given as a
// ULID, on a server that omits fullName): no next steps are printed then,
// rather than ones that cannot work.
func repoCreateRef(r *coreapi.Repo, projectName string) string {
	if ref := nativeRepoPath(r.FullName.Or("")); ref != "" {
		return ref
	}
	if projectName == "" || r.Name == "" {
		return ""
	}
	return "/" + nativeCloneForge + "/" + projectName + "/" + r.Name
}

// printRepoCreateNextSteps prints the commands that put a new repo to use:
// clone it or — the alternative, for an existing checkout — point that
// checkout's origin at it; then, either way, mirror it onto more clusters.
func printRepoCreateNextSteps(w io.Writer, ref string) {
	if ref == "" {
		return
	}
	fmt.Fprintf(w, "\nNext steps\n"+
		"  Clone it:                entire repo clone %[1]s\n"+
		"  Or point origin at it:   entire repo remote add origin %[1]s --override\n"+
		"  Mirror it:               entire repo mirror add %[1]s\n", ref)
}
