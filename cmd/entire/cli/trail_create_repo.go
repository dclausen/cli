package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/spf13/cobra"
)

// trailBranchPresence is what a remote says about a trail's branch.
type trailBranchPresence int

const (
	trailBranchUnknown trailBranchPresence = iota
	trailBranchPresent
	trailBranchMissing
)

// trailRemoteBranchState reports whether branch exists on the repository
// named by forge/owner/repo. It is a seam for tests.
var trailRemoteBranchState = remoteTrailBranchState

// runTrailCreateForRepo creates a trail on the repository --repo names,
// through the API alone. Unlike the local path it never opens a clone, never
// creates or pushes a branch, and never prompts: the branch must already exist
// on that repository, because the server would otherwise start one at the
// base tip and bind the trail to the wrong code.
func runTrailCreateForRepo(cmd *cobra.Command, repoArg, title, body, base, branch, statusStr, typeStr, priorityStr string, assignees []string, checkout, noBranch bool) error {
	ctx := cmd.Context()
	if checkout {
		return errors.New("cannot combine --repo with --checkout: --repo creates the trail remotely and leaves the local clone alone")
	}
	if err := validateTrailCreateEnums(cmd, typeStr, priorityStr); err != nil {
		return err
	}
	title, base, branch = strings.TrimSpace(title), strings.TrimSpace(base), strings.TrimSpace(branch)
	switch {
	case title == "":
		return errors.New("--repo requires --title")
	case base == "":
		return errors.New("--repo requires --base")
	case branch == "" && !noBranch:
		return errors.New("--repo requires --branch or --no-branch")
	}
	if statusStr == "" {
		statusStr = string(trail.StatusOpen)
	}
	if err := validateTrailCreateFields(ctx, title, branch, statusStr, noBranch); err != nil {
		return err
	}
	forge, owner, repoName, err := parseTrailRepoArg(repoArg)
	if err != nil {
		return err
	}
	ref := forge + "/" + owner + "/" + repoName
	if !noBranch {
		presence, err := trailRemoteBranchState(ctx, forge, owner, repoName, branch)
		switch {
		case presence == trailBranchMissing:
			return fmt.Errorf("branch %s does not exist on %s; push it first", branch, ref)
		case presence != trailBranchPresent:
			return fmt.Errorf("could not verify branch %s on %s: %w; push it first or check your access", branch, ref, err)
		}
	}
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), forge, owner, repoName)
	if err != nil {
		return renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), owner+"/"+repoName, err)
	}
	basePath, err := trailRepoBasePath(forge, owner, repoName, repoID)
	if err != nil {
		return err
	}
	createResp, err := postTrailCreate(ctx, client, basePath, forge, owner, repoName, title, body, branch, base, statusStr, strings.TrimSpace(typeStr), strings.TrimSpace(priorityStr), assignees)
	if err != nil {
		return err
	}
	printCreatedTrail(cmd.OutOrStdout(), createResp.Trail, forge, owner, repoName)
	return nil
}

// remoteTrailBranchState asks the repository's git remote whether branch
// exists. An error of any kind (auth, network, unresolvable repo) is Unknown,
// never Missing: a private GitHub repo without credentials must not read as
// an absent branch.
func remoteTrailBranchState(ctx context.Context, forge, owner, repo, branch string) (trailBranchPresence, error) {
	url, err := trailRepoCloneURL(ctx, forge, owner, repo)
	if err != nil {
		return trailBranchUnknown, err
	}
	cmd := execx.NonInteractive(ctx, "git", "ls-remote", "--heads", url, "refs/heads/"+branch)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// git echoes the remote URL in its errors, and a URL can carry
		// credentials, so stderr is redacted per URL before it is surfaced.
		if msg := redactGitStderr(stderr.String()); msg != "" {
			return trailBranchUnknown, fmt.Errorf("git ls-remote: %s", msg)
		}
		return trailBranchUnknown, fmt.Errorf("git ls-remote: %w", err)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		return trailBranchMissing, nil
	}
	return trailBranchPresent, nil
}

// trailRepoCloneURL is the git URL for a --repo triple: the native repo's own
// entire:// coordinates from the control plane, or the GitHub HTTPS URL, which
// uses the caller's git credentials.
func trailRepoCloneURL(ctx context.Context, forge, owner, repo string) (string, error) {
	switch forge {
	case gitremote.ForgeNative:
		c, err := activeCoreClient(ctx)
		if err != nil {
			return "", fmt.Errorf("connect to Entire control plane: %w", err)
		}
		r, err := resolveNativeRepo(ctx, c, owner, repo)
		if err != nil {
			return "", err
		}
		u := repoRemoteURL(*r)
		if u == "" {
			return "", fmt.Errorf("%s/%s/%s has no clone coordinates yet", forge, owner, repo)
		}
		return u, nil
	case gitremote.ForgeGitHub:
		return "https://github.com/" + owner + "/" + repo + ".git", nil
	default:
		return "", fmt.Errorf("unsupported forge %q", forge)
	}
}
