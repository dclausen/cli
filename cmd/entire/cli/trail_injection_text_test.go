package cli

import (
	"os"
	"strings"
	"testing"
)

func TestEntireTrailContextInjection_LegacyUnlessOptedIn(t *testing.T) {
	for _, value := range []string{"unset", "", "0", "false", "true", "typo", " 1 "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(projectTrailsEnv, value)
			if value == "unset" {
				if err := os.Unsetenv(projectTrailsEnv); err != nil {
					t.Fatal(err)
				}
			}
			got := entireTrailContextInjection(trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "app"})
			for _, want := range []string{"Entire is enabled for this repo.", "`entire agent-help`", "never create checkpoints by hand", "Leave setup and destructive commands", "gh/acme/app", "never ask the user for the repo name"} {
				if !strings.Contains(got, want) {
					t.Errorf("legacy injection missing %q: %s", want, got)
				}
			}
			for _, unwanted := range []string{"project-level intent", "Start with `entire trail show`", "`entire trail update`", "otherwise create one"} {
				if strings.Contains(got, unwanted) {
					t.Errorf("legacy injection contains project guidance %q: %s", unwanted, got)
				}
			}
		})
	}
}

func TestEntireTrailContextInjection_ProjectTrailWorkflow(t *testing.T) {
	t.Setenv(projectTrailsEnv, "1")

	for _, tc := range []struct {
		name  string
		scope trailEnablementScope
	}{
		{"known repo", trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "app"}},
		{"no repo", trailEnablementScope{}},
		{"partial scope", trailEnablementScope{Forge: "gh", Owner: "acme"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := entireTrailContextInjection(tc.scope)
			for _, want := range []string{
				"Entire Trails is enabled.",
				"project-level intent across repositories and branches—not just one branch",
				"Start with `entire trail show`",
				"Reuse an existing trail when the work shares its intent; otherwise create one.",
				"Keep its description current with `entire trail update`.",
				"Use `entire agent-help trail` to discover commands and flags.",
				"never ask the user for the repo name",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("injection missing %q:\n%s", want, got)
				}
			}
			for _, unwanted := range []string{
				"Commits automatically",
				"checkpoint",
				"entire why",
				"Leave setup and destructive commands",
				"--repo",
			} {
				if strings.Contains(got, unwanted) {
					t.Errorf("injection should stay focused on trails, found %q:\n%s", unwanted, got)
				}
			}
			if tc.scope.Repo != "" {
				if !strings.Contains(got, "auto-detected from the git origin remote as gh/acme/app") {
					t.Errorf("missing detected repo:\n%s", got)
				}
			} else {
				if strings.Contains(got, "gh/acme") || strings.Contains(got, "//") {
					t.Errorf("incomplete scope must not emit a repo key:\n%s", got)
				}
				if !strings.Contains(got, "Entire auto-detects the repo from the git origin remote") {
					t.Errorf("missing generic repo context:\n%s", got)
				}
			}
		})
	}
}
