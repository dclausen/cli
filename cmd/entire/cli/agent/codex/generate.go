package codex

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// generateTextDisabledFeatures are the codex features that give the model a
// tool. Summary generation needs none (the transcript is already in the
// prompt), and its prompt carries untrusted transcript content. codex's
// default read-only sandbox blocks writes and network but not reads, so with
// any of these enabled an injected instruction could read an arbitrary file
// into the summary. Verified live on codex-cli 0.156.1: disabling shell_tool
// and unified_exec alone still let the model read a file outside the working
// directory through the code-mode JS host, and with this full set a read,
// write, or network request inside or outside the working directory did
// nothing while a normal summary still completed.
//
// This is a denylist because codex exec offers no "no tools" switch and
// withholding sandbox read permissions (sandbox_permissions=[]) does not stop
// reads. A new tool-bearing codex feature is therefore enabled until it is
// added here; `codex features list` shows them.
var generateTextDisabledFeatures = []string{
	"shell_tool",
	"unified_exec",
	"code_mode_host",
	"apps",
	"plugins",
	"browser_use",
	"browser_use_external",
	"computer_use",
	"in_app_browser",
}

// GenerateText sends a prompt to the Codex CLI and returns the raw text response.
//
// codex exits with "Unknown feature flag: <name>" when asked to disable a
// feature it has never had, which an older codex does for names added after
// it shipped. Such a name is dropped and the call retried: a feature that
// version does not know cannot give the model a tool, so the run stays
// tool-free. Names codex lists as removed are accepted as-is.
func (c *CodexAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	disabled := slices.Clone(generateTextDisabledFeatures)
	for {
		result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, c.CommandRunner, "codex", "codex", generateTextArgs(disabled, model), prompt)
		if err == nil {
			return result, nil
		}
		if name, ok := unknownDisabledFeature(capturedStderr, disabled); ok {
			logging.Debug(ctx, "codex does not know a feature Entire disables for text generation; retrying without it",
				slog.String("feature", name))
			disabled = slices.DeleteFunc(disabled, func(f string) bool { return f == name })
			continue
		}
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("codex text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
}

func generateTextArgs(disabled []string, model string) []string {
	args := []string{"exec", "--skip-git-repo-check"}
	for _, feature := range disabled {
		args = append(args, "--disable", feature)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, "-")
}

var unknownFeaturePattern = regexp.MustCompile(`Unknown feature flag: (\S+)`)

// unknownDisabledFeature returns the feature codex rejected as unknown, when
// it is one of disabled. Anything else is a real failure, so the loop in
// GenerateText ends: each retry removes one name, and only a name still in
// the list can trigger another.
func unknownDisabledFeature(stderr string, disabled []string) (string, bool) {
	m := unknownFeaturePattern.FindStringSubmatch(stderr)
	if m == nil || !slices.Contains(disabled, m[1]) {
		return "", false
	}
	return m[1], true
}
