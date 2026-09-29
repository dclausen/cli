package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/require"
)

// trailCreateRepoFixture stubs the trail API client and the remote branch
// check, and records the create request. The seams are package variables, so
// tests using it do not run in parallel; each chdirs into a directory that is
// not a git repository to prove the remote-only path never opens one.
type trailCreateRepoFixture struct {
	gotPath   string
	gotCreate map[string]any
	calls     int
	checked   []string // forge/owner/repo@branch passed to the branch check
}

func newTrailCreateRepoFixture(t *testing.T, presence trailBranchPresence, checkErr error) *trailCreateRepoFixture {
	t.Helper()
	f := &trailCreateRepoFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&f.gotCreate); err != nil {
			t.Errorf("decode create request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(api.TrailCreateResponse{Trail: api.TrailResource{ID: "trl_remote", Number: 7, Title: "Remote"}}); err != nil {
			t.Errorf("encode create response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	prevClient := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("token", srv.URL), "native-repo-id", nil
	}
	prevCheck := trailRemoteBranchState
	trailRemoteBranchState = func(_ context.Context, forge, owner, repo, branch string) (trailBranchPresence, error) {
		f.checked = append(f.checked, forge+"/"+owner+"/"+repo+"@"+branch)
		return presence, checkErr
	}
	t.Cleanup(func() {
		newTrailAPIClient = prevClient
		trailRemoteBranchState = prevCheck
	})
	t.Chdir(t.TempDir())
	return f
}

func runTrailCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newTrailCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SetContext(t.Context())
	err := cmd.Execute()
	return out.String(), err
}

func TestTrailCreateRepo_NativeRemoteOnly(t *testing.T) {
	f := newTrailCreateRepoFixture(t, trailBranchPresent, nil)

	out, err := runTrailCmd(t, "create", "--repo", "et/proj/app", "--branch", "feat", "--base", "main",
		"--title", "Remote", "--body", "b", "--status", "draft", "--add-assignee", "alice")

	require.NoError(t, err)
	require.Equal(t, "/api/v1/repos/native-repo-id/trails", f.gotPath)
	require.Equal(t, "Remote", f.gotCreate["title"])
	require.Equal(t, "feat", f.gotCreate["branch_name"])
	require.Equal(t, "main", f.gotCreate["base"])
	require.Equal(t, "draft", f.gotCreate["status"])
	require.Equal(t, []string{"et/proj/app@feat"}, f.checked)
	require.Contains(t, out, `Created trail "Remote"`)
}

func TestTrailCreateRepo_GitHubRemoteOnly(t *testing.T) {
	f := newTrailCreateRepoFixture(t, trailBranchPresent, nil)

	_, err := runTrailCmd(t, "create", "--repo", "gh/acme/app", "--branch", "feat", "--base", "main", "--title", "Remote")

	require.NoError(t, err)
	require.Equal(t, "/api/v1/trails/gh/acme/app", f.gotPath)
	require.Equal(t, "open", f.gotCreate["status"], "status defaults to open")
}

func TestTrailCreateRepo_Branchless(t *testing.T) {
	f := newTrailCreateRepoFixture(t, trailBranchMissing, nil)

	_, err := runTrailCmd(t, "create", "--repo", "et/proj/app", "--no-branch", "--base", "main", "--title", "Remote")

	require.NoError(t, err)
	require.Empty(t, f.checked, "branchless trails need no branch check")
	require.NotContains(t, f.gotCreate, "branch_name")
}

func TestTrailCreateRepo_BranchMissingOrUnknown(t *testing.T) {
	tests := []struct {
		name     string
		presence trailBranchPresence
		checkErr error
		want     string
	}{
		{name: "missing", presence: trailBranchMissing, want: "branch feat does not exist on et/proj/app; push it first"},
		{name: "unknown", presence: trailBranchUnknown, checkErr: errors.New("permission denied"), want: "could not verify branch feat on et/proj/app: permission denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTrailCreateRepoFixture(t, tt.presence, tt.checkErr)

			_, err := runTrailCmd(t, "create", "--repo", "et/proj/app", "--branch", "feat", "--base", "main", "--title", "Remote")

			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
			require.Zero(t, f.calls, "no trail may be created for an unverified branch")
		})
	}
}

func TestTrailCreateRepo_RequiresExplicitFields(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no title", args: []string{"--branch", "feat", "--base", "main"}, want: "--title"},
		{name: "no base", args: []string{"--branch", "feat", "--title", "T"}, want: "--base"},
		{name: "no branch", args: []string{"--base", "main", "--title", "T"}, want: "--branch or --no-branch"},
		{name: "checkout", args: []string{"--branch", "feat", "--base", "main", "--title", "T", "--checkout"}, want: "cannot combine --repo with --checkout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTrailCreateRepoFixture(t, trailBranchPresent, nil)

			_, err := runTrailCmd(t, append([]string{"create", "--repo", "et/proj/app"}, tt.args...)...)

			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
			require.Zero(t, f.calls)
		})
	}
}

func TestRedactGitStderr_RedactsCredentialedURLs(t *testing.T) {
	t.Parallel()
	got := redactGitStderr("fatal: unable to access 'https://user:s3cret@github.com/acme/app.git/': 403\n\nhint: check access\n")
	require.NotContains(t, got, "s3cret")
	require.Contains(t, got, "fatal: unable to access")
	require.Contains(t, got, "; hint: check access")
}
