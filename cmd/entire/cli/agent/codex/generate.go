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

// generateTextDisabledFeatures are codex features that give the model a
// tool. Summary generation needs none (the transcript is already in the
// prompt), and its prompt carries untrusted transcript content. codex's
// default read-only sandbox blocks writes and network but not reads, so with
// shell_tool or unified_exec enabled an injected instruction could read an
// arbitrary file into the summary; disabling only those two still left a read
// through the code-mode JS host (verified live on codex-cli 0.156.1).
//
// This is a denylist and it is NOT complete: codex exec offers no "no tools"
// switch, withholding sandbox read permissions (sandbox_permissions=[]) does
// not stop reads, and codex keeps adding tool-bearing features. On 0.156.1
// the model still reports collaboration (sub-agent) tools, apply_patch, web
// search, and goal tools with this set disabled; none of them read the
// canary in live probes, but that is observed behavior, not a guarantee.
// view_image, multi_agent, and image_generation are listed because they are
// stable, on by default, and reach files or spawn work. `codex features list`
// shows candidates.
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
	"view_image",
	"multi_agent",
	"image_generation",
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

// generateTextArgs builds the codex argv for one text-generation call.
//
// --ignore-user-config skips ~/.codex/config.toml. The features below are
// flags, but MCP servers, a web_search setting, and hooks come from that file,
// and `-c mcp_servers={}` does not remove configured servers (codex merges
// -c overrides into the table). Ignoring the file keeps all three out; login
// lives in auth.json and still works. A custom model provider or profile set
// in config.toml is not used for summaries as a result.
func generateTextArgs(disabled []string, model string) []string {
	args := []string{"exec", "--skip-git-repo-check", "--ignore-user-config"}
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
