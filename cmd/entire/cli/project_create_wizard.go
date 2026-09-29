package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/internal/coreapi"
)

// projectNameMaxLen mirrors CreateProjectInputBody.name's maxLength, so the
// wizard rejects an over-long name before the server does.
const projectNameMaxLen = 100

// projectCreateCancelled names the flow in its cancellation line.
const projectCreateCancelled = "Project create"

// projectCreateInput is what `project create` was given on the command line.
// In the wizard every field is only a starting value.
type projectCreateInput struct {
	name      string
	owner     string
	ownerType string
	region    string
}

// complete reports whether the command line names everything a project needs,
// in which case it is created without prompting.
func (in projectCreateInput) complete() bool {
	return in.name != "" && in.owner != ""
}

// projectOwner is one row of the owner picker. The user only ever sees ref (an
// org name or a provider-qualified handle); key and id are internal, the id
// being what the create request needs.
type projectOwner struct {
	key      string
	kind     coreapi.CreateProjectInputBodyOwnerType
	id       string
	ref      string
	region   string // the owner's jurisdiction: the region picker's default
	personal bool
}

// label is the owner's picker row, padded so the kind column lines up.
func (o projectOwner) label(width int) string {
	var kind string
	switch {
	case o.personal:
		kind = "you — personal project"
	case o.region != "":
		kind = "organization, " + o.region
	default:
		kind = "organization"
	}
	return fmt.Sprintf("%-*s  (%s)", width, o.ref, kind)
}

// projectRegion is one jurisdiction the region picker offers.
type projectRegion struct {
	id    string
	label string
}

func (r projectRegion) display() string {
	if r.label == "" || strings.EqualFold(r.label, r.id) {
		return r.id
	}
	return fmt.Sprintf("%s (%s)", r.label, r.id)
}

// projectCreateAnswers is what the wizard collects. It is its own struct so
// the summary can bind to the answers alone rather than to every listing.
type projectCreateAnswers struct {
	ownerKey string
	name     string
	region   string
}

// projectCreateState is the wizard's model: the choices on offer, the answers
// so far, and the listing the duplicate-name check reads.
type projectCreateState struct {
	owners  []projectOwner
	regions []projectRegion
	// hiddenOrgs counts the orgs left out because the caller cannot create
	// projects in them.
	hiddenOrgs int
	// existing is every project the caller can see, fetched once up front
	// because huh validates on the UI loop. Nil when the listing failed; the
	// server's own conflict check still applies.
	existing []coreapi.Project
	// regionPinned is set when --region was given: the region then stays put
	// instead of following the owner.
	regionPinned bool
	// ownerChanges counts owner changes. The region select's options are
	// bound to it rather than to the owner, because huh caches options per
	// binding value and, on a cache hit, leaves the cursor where it was: going
	// back to an owner picked before would then keep the other owner's region.
	ownerChanges int

	answers   projectCreateAnswers
	confirmed bool
}

// projectCreateData is everything the wizard loads before it opens.
type projectCreateData struct {
	me       *coreapi.GetMeOutputBody
	orgs     []coreapi.Org
	regions  []coreapi.TopologyJurisdiction
	projects []coreapi.Project
}

// loadProjectCreateData fetches the caller, their orgs, the jurisdictions and
// the visible projects concurrently. Only the project listing may fail: it
// feeds a pre-check the server repeats anyway.
func loadProjectCreateData(ctx context.Context, c *coreapi.Client) (projectCreateData, error) {
	var d projectCreateData
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		me, err := c.GetMe(gctx)
		if err != nil {
			return fmt.Errorf("fetch profile: %w", err)
		}
		d.me = me
		return nil
	})
	g.Go(func() error {
		orgs, err := listAllOrgs(gctx, c)
		if err != nil {
			return fmt.Errorf("list orgs: %w", err)
		}
		d.orgs = orgs
		return nil
	})
	g.Go(func() error {
		top, err := c.GetTopology(gctx)
		if err != nil {
			return fmt.Errorf("list regions: %w", err)
		}
		d.regions = top.Jurisdictions
		return nil
	})
	g.Go(func() error {
		projects, err := listAllProjects(gctx, c)
		if err != nil {
			logging.Debug(gctx, "project create: skipping duplicate-name pre-check", "error", err.Error())
			return nil
		}
		d.projects = projects
		return nil
	})
	if err := g.Wait(); err != nil {
		return projectCreateData{}, err //nolint:wrapcheck // each branch already names what it fetched
	}
	return d, nil
}

// projectOwnerKeyPersonal is the personal row's picker value; org rows are
// keyed by id, which a user never sees.
const projectOwnerKeyPersonal = "account"

// projectOwners builds the owner rows: the caller's own account first, then
// the orgs they may create projects in, sorted by name. It also returns how
// many orgs were left out.
func projectOwners(me *coreapi.GetMeOutputBody, orgs []coreapi.Org) ([]projectOwner, int) {
	profile := profileFromMe(me)
	ref := authIdentityLabel(profile)
	if ref == "" {
		ref = "you"
	}
	owners := []projectOwner{{
		key:      projectOwnerKeyPersonal,
		kind:     coreapi.CreateProjectInputBodyOwnerTypeAccount,
		id:       me.Global.AccountId,
		ref:      ref,
		region:   profile.Jurisdiction,
		personal: true,
	}}
	creatable := make([]coreapi.Org, 0, len(orgs))
	for _, o := range orgs {
		// An org that reports no capabilities is offered: the server still
		// refuses a create the caller may not make.
		if caps, ok := o.Capabilities.Get(); !ok || caps.CanCreateProject {
			creatable = append(creatable, o)
		}
	}
	slices.SortStableFunc(creatable, func(a, b coreapi.Org) int {
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	for _, o := range creatable {
		owners = append(owners, projectOwner{
			key:    "org:" + o.ID,
			kind:   coreapi.CreateProjectInputBodyOwnerTypeOrg,
			id:     o.ID,
			ref:    o.Name,
			region: o.Region,
		})
	}
	return owners, len(orgs) - len(creatable)
}

// projectRegions maps the topology's jurisdictions to picker rows.
func projectRegions(jurisdictions []coreapi.TopologyJurisdiction) []projectRegion {
	out := make([]projectRegion, 0, len(jurisdictions))
	for _, j := range jurisdictions {
		if j.ID == "" {
			continue
		}
		out = append(out, projectRegion{id: j.ID, label: j.Label})
	}
	return out
}

// newProjectCreateState assembles the wizard from what was loaded and what the
// command line said. A --owner or --region that names nothing on offer is an
// error rather than a silently different starting point.
func newProjectCreateState(d projectCreateData, in projectCreateInput, defaultName string) (*projectCreateState, error) {
	owners, hidden := projectOwners(d.me, d.orgs)
	s := &projectCreateState{
		owners:     owners,
		regions:    projectRegions(d.regions),
		hiddenOrgs: hidden,
		existing:   d.projects,
	}
	if len(s.regions) == 0 {
		return nil, errors.New("no regions available to create a project in")
	}

	s.answers.name = cmp.Or(in.name, defaultName)

	if in.region != "" {
		r, ok := s.regionByID(in.region)
		if !ok {
			return nil, fmt.Errorf("unknown --region %q: choose one of %s", in.region, strings.Join(s.regionIDs(), ", "))
		}
		s.answers.region = r.id
		s.regionPinned = true
	}

	ownerKey := projectOwnerKeyPersonal
	if in.owner != "" {
		o, ok := s.matchOwner(in.owner)
		if !ok {
			return nil, fmt.Errorf("--owner %q is not an owner you can create projects under", in.owner)
		}
		ownerKey = o.key
	}
	s.setOwner(ownerKey)
	return s, nil
}

// matchOwner finds the row a --owner value names: an org by name (exact, then
// case-folded) or id, or the caller's account by handle or id.
func (s *projectCreateState) matchOwner(ref string) (projectOwner, bool) {
	for _, o := range s.owners {
		if o.ref == ref || o.id == ref {
			return o, true
		}
	}
	for _, o := range s.owners {
		if strings.EqualFold(o.ref, ref) {
			return o, true
		}
	}
	return projectOwner{}, false
}

func (s *projectCreateState) owner() projectOwner {
	for _, o := range s.owners {
		if o.key == s.answers.ownerKey {
			return o
		}
	}
	return s.owners[0]
}

// setOwner records the owner and, unless --region pinned it, moves the region
// to that owner's jurisdiction (the first region when it has none on offer).
// Re-setting the current owner is a no-op: huh writes a select's value back
// after every message, which must not undo a region the user picked.
func (s *projectCreateState) setOwner(key string) {
	if key == s.answers.ownerKey {
		return
	}
	s.answers.ownerKey = key
	s.ownerChanges++
	if s.regionPinned {
		return
	}
	if r, ok := s.regionByID(s.owner().region); ok {
		s.answers.region = r.id
		return
	}
	s.answers.region = s.regions[0].id
}

func (s *projectCreateState) regionByID(id string) (projectRegion, bool) {
	for _, r := range s.regions {
		if strings.EqualFold(r.id, id) {
			return r, true
		}
	}
	return projectRegion{}, false
}

func (s *projectCreateState) regionIDs() []string {
	ids := make([]string, len(s.regions))
	for i, r := range s.regions {
		ids[i] = r.id
	}
	return ids
}

// validateName checks the length the API enforces and, when the listing
// loaded, that the chosen owner has no project of that name yet. Names are
// compared case-insensitively, as the API's own name lookup is.
func (s *projectCreateState) validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("enter a project name")
	}
	if utf8.RuneCountInString(name) > projectNameMaxLen {
		return fmt.Errorf("project names are at most %d characters", projectNameMaxLen)
	}
	o := s.owner()
	for _, p := range s.existing {
		if string(p.OwnerType) == string(o.kind) && p.OwnerId == o.id && strings.EqualFold(p.Name, name) {
			if o.personal {
				return fmt.Errorf("you already have a project named %q", p.Name)
			}
			return fmt.Errorf("%s already has a project named %q", o.ref, p.Name)
		}
	}
	return nil
}

// ownerDisplay names the chosen owner in the summary.
func (s *projectCreateState) ownerDisplay() string {
	o := s.owner()
	if o.personal {
		return o.ref + " (you)"
	}
	return o.ref + " (organization)"
}

func (s *projectCreateState) regionDisplay() string {
	if r, ok := s.regionByID(s.answers.region); ok {
		return r.display()
	}
	return s.answers.region
}

// command is the flag form of the answers, so the summary teaches the
// non-interactive spelling.
func (s *projectCreateState) command() string {
	o := s.owner()
	parts := []string{"entire project create", shellArg(strings.TrimSpace(s.answers.name)), "--owner", shellArg(o.ref)}
	if o.kind == coreapi.CreateProjectInputBodyOwnerTypeAccount {
		parts = append(parts, "--owner-type", ownerTypeAccount)
	}
	parts = append(parts, "--region", s.answers.region)
	return strings.Join(parts, " ")
}

func (s *projectCreateState) summary() string {
	rows := [][2]string{
		{"Name", strings.TrimSpace(s.answers.name)},
		{"Owner", s.ownerDisplay()},
		{"Region", s.regionDisplay()},
		{"Command", s.command()},
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%-8s %s", r[0], r[1])
	}
	return b.String()
}

// request is the create body the answers describe. The region is always sent:
// the wizard showed one, so that is the one the project gets.
func (s *projectCreateState) request() *coreapi.CreateProjectInputBody {
	o := s.owner()
	return &coreapi.CreateProjectInputBody{
		Name:      strings.TrimSpace(s.answers.name),
		OwnerId:   o.id,
		OwnerType: o.kind,
		Region:    coreapi.NewOptString(s.answers.region),
	}
}

// projectCreatePrompt is the seam the wizard's forms sit behind. It fills in
// s.answers and s.confirmed, returning (false, nil) when the user cancelled
// after being told so. Command-level tests swap it, because the forms are
// unreachable under `go test`.
var projectCreatePrompt = runProjectCreateForms

// runProjectCreateWizard is the prompting path of `project create`: load the
// choices, ask, then create what the summary showed.
func runProjectCreateWizard(cmd *cobra.Command, in projectCreateInput) error {
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		stop := startSpinner(cmd.ErrOrStderr(), "Loading owners and regions")
		d, err := loadProjectCreateData(ctx, c)
		stop(err == nil)
		if err != nil {
			return err
		}
		s, err := newProjectCreateState(d, in, currentFolderName(ctx))
		if err != nil {
			return err
		}
		ok, err := projectCreatePrompt(cmd, s)
		if err != nil || !ok {
			return err
		}
		created, err := c.CreateProject(ctx, s.request())
		if err != nil {
			return err
		}
		return printProjectCreated(cmd, &created.Response, s.owner().ref)
	})
}

// currentFolderName is the name the wizard suggests when none was given: the
// current repository's folder, or nothing outside one.
func currentFolderName(ctx context.Context) string {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return ""
	}
	return filepath.Base(root)
}

// runProjectCreateForms runs the wizard as one paged form, so Shift+Tab walks
// back through earlier answers and the region follows the owner. huh's
// accessible runner evaluates neither OptionsFunc nor DescriptionFunc, so there
// each stage is its own form, built once the answers it depends on are in.
func runProjectCreateForms(cmd *cobra.Command, s *projectCreateState) (bool, error) {
	s.confirmed = true
	if IsAccessibleMode() {
		for _, groups := range [][]*huh.Group{
			{s.ownerGroup()},
			{s.nameGroup(), s.regionGroup(false)},
			{s.summaryGroup(false)},
		} {
			if ok, err := runProjectCreateForm(cmd, s, groups...); !ok || err != nil {
				return ok, err
			}
		}
		return true, nil
	}
	return runProjectCreateForm(cmd, s, s.ownerGroup(), s.nameGroup(), s.regionGroup(true), s.summaryGroup(true))
}

// runProjectCreateForm runs one form and classifies how it ended: a cancelled
// context is an interruption and comes back as an error, a user abort prints
// the cancellation line where the prompt was, and a declined summary does too.
func runProjectCreateForm(cmd *cobra.Command, s *projectCreateState, groups ...*huh.Group) (bool, error) {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("project create: %w", err)
	}
	render, err := runPromptForm(cmd, NewAccessibleForm(groups...))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("project create: %w", ctxErr)
	}
	if err != nil {
		return false, handleFormCancellation(render, projectCreateCancelled, err)
	}
	return s.confirm(render), nil
}

// confirm turns a declined summary into the cancellation line, written where
// the prompt was drawn.
func (s *projectCreateState) confirm(render io.Writer) bool {
	if !s.confirmed {
		fmt.Fprintln(render, projectCreateCancelled+" cancelled.")
	}
	return s.confirmed
}

func (s *projectCreateState) ownerGroup() *huh.Group {
	width := 0
	for _, o := range s.owners {
		width = max(width, utf8.RuneCountInString(o.ref))
	}
	opts := make([]huh.Option[string], len(s.owners))
	for i, o := range s.owners {
		opts[i] = huh.NewOption(o.label(width), o.key)
	}
	sel := huh.NewSelect[string]().
		Title("Who will own this project?").
		Options(opts...).
		Accessor(projectOwnerAccessor{s: s})
	switch s.hiddenOrgs {
	case 0:
	case 1:
		sel.Description("1 organization hidden: you can't create projects in it.")
	default:
		sel.Description(fmt.Sprintf("%d organizations hidden: you can't create projects in them.", s.hiddenOrgs))
	}
	return huh.NewGroup(sel).Title("Owner")
}

func (s *projectCreateState) nameGroup() *huh.Group {
	return huh.NewGroup(
		huh.NewInput().
			Title("Project name").
			Value(&s.answers.name).
			Validate(s.validateName),
	).Title("Name")
}

// regionGroup offers the jurisdictions. dynamic re-selects the owner's region
// whenever the owner changes: huh re-runs an OptionsFunc when its binding
// changes and then moves the cursor to the bound value, which setOwner has
// already moved. See ownerChanges for why the binding is a counter.
func (s *projectCreateState) regionGroup(dynamic bool) *huh.Group {
	opts := make([]huh.Option[string], len(s.regions))
	for i, r := range s.regions {
		opts[i] = huh.NewOption(r.display(), r.id)
	}
	sel := huh.NewSelect[string]().
		Title("Where should its data live?").
		Description("Defaults to the owner's region.").
		Value(&s.answers.region)
	if dynamic {
		sel.OptionsFunc(func() []huh.Option[string] { return opts }, &s.ownerChanges)
	} else {
		sel.Options(opts...)
	}
	return huh.NewGroup(sel).Title("Region")
}

// summaryGroup shows what will be created and asks to go ahead. dynamic keeps
// the summary current as earlier pages are revisited.
func (s *projectCreateState) summaryGroup(dynamic bool) *huh.Group {
	note := huh.NewNote().Title("Summary")
	if dynamic {
		note.DescriptionFunc(s.summary, &s.answers)
	} else {
		note.Description(s.summary())
	}
	return huh.NewGroup(
		note,
		huh.NewConfirm().
			Title("Create this project?").
			Affirmative("Create").
			Negative("Cancel").
			Value(&s.confirmed),
	).Title("Summary")
}

// projectOwnerAccessor routes the owner select through setOwner, so moving the
// cursor also moves the region default.
type projectOwnerAccessor struct{ s *projectCreateState }

func (a projectOwnerAccessor) Get() string  { return a.s.answers.ownerKey }
func (a projectOwnerAccessor) Set(v string) { a.s.setOwner(v) }
