package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

const (
	testWizardAccountULID = "01HZX7QACC0000000000000000"
	testWizardAcmeULID    = "01HZX7QACME000000000000000"
	testWizardBetaULID    = "01HZX7QBETA000000000000000"
	testWizardLockedULID  = "01HZX7QL0CKED0000000000000"
)

func wizardTestOrg(id, name, region string, canCreate bool) coreapi.Org {
	return coreapi.Org{
		ID: id, Name: name, Region: region,
		Capabilities: coreapi.NewOptOrgCapabilities(coreapi.OrgCapabilities{CanCreateProject: canCreate}),
	}
}

func wizardTestData() projectCreateData {
	return projectCreateData{
		me: &coreapi.GetMeOutputBody{
			Auth: coreapi.MeAuth{Provider: "github"},
			Global: coreapi.MeGlobal{
				AccountId:        testWizardAccountULID,
				Handle:           coreapi.NewOptString("alice"),
				HomeJurisdiction: coreapi.NewOptString("us"),
			},
		},
		orgs: []coreapi.Org{
			wizardTestOrg(testWizardBetaULID, "beta", "eu", true),
			wizardTestOrg(testWizardLockedULID, "locked", "us", false),
			wizardTestOrg(testWizardAcmeULID, "Acme", "us", true),
		},
		regions: []coreapi.TopologyJurisdiction{
			{ID: "us", Label: "United States"},
			{ID: "eu", Label: "Europe"},
		},
		projects: []coreapi.Project{
			{Name: "widgets", OwnerId: testWizardAcmeULID, OwnerType: coreapi.ProjectOwnerTypeOrg},
			{Name: "dotfiles", OwnerId: testWizardAccountULID, OwnerType: coreapi.ProjectOwnerTypeAccount},
		},
	}
}

func TestProjectOwners_PersonalFirstThenCreatableOrgs(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	// An org whose capabilities are absent is offered; the server decides.
	d.orgs = append(d.orgs, coreapi.Org{ID: "01HZX7QN0CAPS0000000000000", Name: "nocaps"})

	owners, hidden := projectOwners(d.me, d.orgs)
	refs := make([]string, len(owners))
	for i, o := range owners {
		refs[i] = o.ref
	}
	assert.Equal(t, []string{"github:alice", "Acme", "beta", "nocaps"}, refs)
	assert.Equal(t, 1, hidden, "only the org without create permission is hidden")
	assert.True(t, owners[0].personal)
	assert.Equal(t, "us", owners[0].region, "the personal default is the home jurisdiction")
}

func TestProjectOwner_LabelsSayWhatEachRowIs(t *testing.T) {
	t.Parallel()
	owners, _ := projectOwners(wizardTestData().me, wizardTestData().orgs)
	assert.Equal(t, "github:alice  (you — personal project)", owners[0].label(12))
	assert.Equal(t, "Acme          (organization, us)", owners[1].label(12))
	for _, o := range owners {
		assert.NotContains(t, o.label(12), o.id, "a row never shows an id")
	}
}

func TestNewProjectCreateState_Defaults(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{}, "my-repo")
	require.NoError(t, err)
	assert.Equal(t, "my-repo", s.answers.name, "the folder name is suggested when no name was given")
	assert.True(t, s.owner().personal, "the personal row is the starting owner")
	assert.Equal(t, "us", s.answers.region)
}

func TestNewProjectCreateState_PrefillsFromTheCommandLine(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{name: "widgets", owner: "beta"}, "my-repo")
	require.NoError(t, err)
	assert.Equal(t, "widgets", s.answers.name, "the argument beats the folder name")
	assert.Equal(t, "beta", s.owner().ref)
	assert.Equal(t, "eu", s.answers.region, "the region starts at the owner's")
}

func TestNewProjectCreateState_MatchesOwner(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"Acme", "acme", testWizardAcmeULID} {
		s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: ref}, "")
		require.NoError(t, err, ref)
		assert.Equal(t, "Acme", s.owner().ref, ref)
	}
	for _, ref := range []string{"github:alice", "GitHub:Alice", testWizardAccountULID} {
		s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: ref}, "")
		require.NoError(t, err, ref)
		assert.True(t, s.owner().personal, ref)
	}
}

func TestNewProjectCreateState_RejectsOwnersNotOnOffer(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"locked", "nope", "github:bob"} {
		_, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: ref}, "")
		require.ErrorContains(t, err, "is not an owner you can create projects under", ref)
	}
}

func TestNewProjectCreateState_Region(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: "beta", region: "US"}, "")
	require.NoError(t, err)
	assert.Equal(t, "us", s.answers.region, "--region wins over the owner's region")
	s.setOwner(s.owners[0].key)
	assert.Equal(t, "us", s.answers.region, "and stays put when the owner changes")

	_, err = newProjectCreateState(wizardTestData(), projectCreateInput{region: "mars"}, "")
	require.ErrorContains(t, err, `unknown --region "mars": choose one of us, eu`)

	d := wizardTestData()
	d.regions = nil
	_, err = newProjectCreateState(d, projectCreateInput{}, "")
	require.ErrorContains(t, err, "no regions available")
}

// Moving the owner cursor goes through the accessor, which is what makes the
// region page follow the owner.
func TestProjectOwnerAccessor_MovesTheRegion(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	d.orgs = append(d.orgs, wizardTestOrg("01HZX7QAP0C00000000000000", "apac", "ap", true))
	s, err := newProjectCreateState(d, projectCreateInput{}, "")
	require.NoError(t, err)
	acc := projectOwnerAccessor{s: s}

	acc.Set("org:" + testWizardBetaULID)
	assert.Equal(t, "eu", s.answers.region)
	acc.Set(projectOwnerKeyPersonal)
	assert.Equal(t, "us", s.answers.region)
	acc.Set("org:01HZX7QAP0C00000000000000")
	assert.Equal(t, "us", s.answers.region, "an owner region not on offer falls back to the first region")
	assert.Equal(t, "org:01HZX7QAP0C00000000000000", acc.Get())

	// huh writes the value back after every message; that must not undo a
	// region picked by hand, nor count as an owner change.
	s.answers.region = "eu"
	changes := s.ownerChanges
	acc.Set(acc.Get())
	assert.Equal(t, "eu", s.answers.region)
	assert.Equal(t, changes, s.ownerChanges)
}

func TestProjectCreateState_ValidateName(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: "acme"}, "")
	require.NoError(t, err)

	require.ErrorContains(t, s.validateName("  "), "enter a project name")
	require.ErrorContains(t, s.validateName(strings.Repeat("x", 101)), "at most 100 characters")
	require.NoError(t, s.validateName(strings.Repeat("x", 100)))
	require.EqualError(t, s.validateName("Widgets"), `Acme already has a project named "widgets"`)
	require.NoError(t, s.validateName("dotfiles"), "another owner's project name is free here")

	s.setOwner(projectOwnerKeyPersonal)
	require.EqualError(t, s.validateName("dotfiles"), `you already have a project named "dotfiles"`)
	require.NoError(t, s.validateName("widgets"))

	s.existing = nil // the listing failed: only the length checks remain
	require.NoError(t, s.validateName("dotfiles"))
}

func TestProjectCreateState_SummaryNamesNoIDs(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{name: "my widgets", owner: "acme"}, "")
	require.NoError(t, err)
	assert.Equal(t, "Name     my widgets\n"+
		"Owner    Acme (organization)\n"+
		"Region   United States (us)\n"+
		`Command  entire project create 'my widgets' --owner Acme --region us`, s.summary())

	s.setOwner(projectOwnerKeyPersonal)
	assert.Equal(t, `entire project create 'my widgets' --owner github:alice --owner-type account --region us`, s.command())
	assert.Contains(t, s.summary(), "github:alice (you)")
	assert.NotContains(t, s.summary(), testWizardAccountULID)
}

// --- command level ----------------------------------------------------------

// fakeProjectCore serves the calls `project create` makes and records the
// create request.
type fakeProjectCore struct {
	mu       sync.Mutex
	requests []string
	created  *coreapi.CreateProjectInputBody
}

func (f *fakeProjectCore) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		reply := func(body string) {
			if _, err := io.WriteString(w, body); err != nil {
				t.Errorf("write response: %v", err)
			}
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/me":
			reply(`{"auth":{"provider":"github","providerUserId":"1"},
				"global":{"accountId":"` + testWizardAccountULID + `","createdAt":"2026-01-01T00:00:00Z",
				"handle":"alice","handles":[],"homeJurisdiction":"us"}}`)
		case "GET /api/v1/orgs":
			reply(`{"orgs":[{"id":"` + testWizardAcmeULID + `","name":"acme","region":"eu","createdAt":"2026-01-01T00:00:00Z",
				"capabilities":{"canCreateProject":true,"canManageMembers":true,"canDelete":true,"canChangeOwners":true}}]}`)
		case "GET /api/v1/topology":
			reply(`{"jurisdictions":[{"id":"us","label":"United States","regions":[]},{"id":"eu","label":"Europe","regions":[]}]}`)
		case "GET /api/v1/projects":
			reply(`{"projects":[]}`)
		case "POST /api/v1/projects":
			var body coreapi.CreateProjectInputBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.created = &body
			w.WriteHeader(http.StatusCreated)
			ownerName := "acme"
			if body.OwnerType == coreapi.CreateProjectInputBodyOwnerTypeAccount {
				ownerName = "alice"
			}
			reply(`{"id":"01HZX7QPR0JECT000000000000","name":"` + body.Name + `","ownerId":"` + body.OwnerId +
				`","ownerName":"` + ownerName + `","ownerType":"` + string(body.OwnerType) + `","region":"` + body.Region.Or("us") +
				`","createdAt":"2026-01-01T00:00:00Z"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// newProjectCoreFixture installs the fake as the active core client and a
// prompt that fails the test unless replaced.
// Not parallel: swaps package-level seams.
func newProjectCoreFixture(t *testing.T) *fakeProjectCore {
	t.Helper()
	fake := &fakeProjectCore{}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	prevClient := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prevClient })
	stubProjectCreatePrompt(t, func(*cobra.Command, *projectCreateState) (bool, error) {
		t.Error("the wizard must not open")
		return false, nil
	})
	return fake
}

func stubProjectCreatePrompt(t *testing.T, fn func(*cobra.Command, *projectCreateState) (bool, error)) {
	t.Helper()
	prev := projectCreatePrompt
	projectCreatePrompt = fn
	t.Cleanup(func() { projectCreatePrompt = prev })
}

func execProjectCreate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newProjectCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"create"}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// With a name and an owner the project is created straight away, even in a
// terminal, and an omitted --region stays omitted.
func TestProjectCreate_CompleteFlagsSkipTheWizard(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)

	out, err := execProjectCreate(t, "widgets", "--owner", "acme")
	require.NoError(t, err)
	assert.Equal(t, "✓ Created project acme/widgets in us\n", out)
	require.NotNil(t, fake.created)
	assert.Equal(t, testWizardAcmeULID, fake.created.OwnerId)
	assert.False(t, fake.created.Region.IsSet(), "the server picks the region")
	assert.NotContains(t, fake.requests, "GET /api/v1/topology")
}

func TestProjectCreate_CompleteFlagsJSON(t *testing.T) {
	newProjectCoreFixture(t)
	out, err := execProjectCreate(t, "widgets", "--owner", "acme", "--region", "eu", "--json")
	require.NoError(t, err)
	var got coreapi.CreatedProject
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "eu", got.Region)
}

// Without a terminal a missing name or owner is refused before any request.
func TestProjectCreate_NonInteractiveNeedsNameAndOwner(t *testing.T) {
	fake := newProjectCoreFixture(t)
	for _, args := range [][]string{{}, {"widgets"}, {"--owner", "acme"}} {
		_, err := execProjectCreate(t, args...)
		require.ErrorContains(t, err, "a project name and --owner are required without an interactive terminal", args)
		assert.NotContains(t, err.Error(), "ULID")
	}
	assert.Empty(t, fake.requests)
}

func TestProjectCreate_InvalidOwnerTypeFailsFirst(t *testing.T) {
	fake := newProjectCoreFixture(t)
	_, err := execProjectCreate(t, "widgets", "--owner-type", "team")
	require.ErrorContains(t, err, `invalid --owner-type "team"`)
	assert.Empty(t, fake.requests)
}

func TestProjectCreate_WizardCreatesWhatTheSummaryShowed(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	stubProjectCreatePrompt(t, func(_ *cobra.Command, s *projectCreateState) (bool, error) {
		assert.Equal(t, "widgets", s.answers.name, "the argument is the starting name")
		assert.True(t, s.owner().personal)
		assert.Equal(t, "us", s.answers.region)
		projectOwnerAccessor{s: s}.Set("org:" + testWizardAcmeULID)
		assert.Equal(t, "eu", s.answers.region, "the region follows the owner")
		return true, nil
	})

	out, err := execProjectCreate(t, "widgets")
	require.NoError(t, err)
	assert.Equal(t, "✓ Created project acme/widgets in eu\n", out)
	require.NotNil(t, fake.created)
	assert.Equal(t, testWizardAcmeULID, fake.created.OwnerId)
	assert.Equal(t, coreapi.CreateProjectInputBodyOwnerTypeOrg, fake.created.OwnerType)
	assert.Equal(t, "eu", fake.created.Region.Or(""), "the wizard sends the region it showed")
}

func TestProjectCreate_WizardPersonalProject(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	stubProjectCreatePrompt(t, func(*cobra.Command, *projectCreateState) (bool, error) { return true, nil })

	out, err := execProjectCreate(t, "widgets")
	require.NoError(t, err)
	assert.Equal(t, "✓ Created project github:alice/widgets in us\n", out)
	require.NotNil(t, fake.created)
	assert.Equal(t, testWizardAccountULID, fake.created.OwnerId)
	assert.Equal(t, coreapi.CreateProjectInputBodyOwnerTypeAccount, fake.created.OwnerType)
}

func TestProjectCreate_WizardCancelledCreatesNothing(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	stubProjectCreatePrompt(t, func(*cobra.Command, *projectCreateState) (bool, error) { return false, nil })

	out, err := execProjectCreate(t, "widgets")
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Nil(t, fake.created)
}

func TestProjectCreate_WizardRejectsUnknownOwnerBeforePrompting(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	_, err := execProjectCreate(t, "--owner", "nope")
	require.ErrorContains(t, err, `--owner "nope" is not an owner you can create projects under`)
	assert.Nil(t, fake.created)
}

// A declined summary is the user's answer: it prints the cancellation line on
// the prompt's writer and reports no error.
func TestProjectCreateState_DeclinedSummary(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	s := &projectCreateState{confirmed: true}
	assert.True(t, s.confirm(&w))
	assert.Empty(t, w.String())
	s.confirmed = false
	assert.False(t, s.confirm(&w))
	assert.Equal(t, "Project create cancelled.\n", w.String())
}
