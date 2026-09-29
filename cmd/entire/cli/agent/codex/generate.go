package codex

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
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
func (c *CodexAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"exec", "--skip-git-repo-check"}
	for _, feature := range generateTextDisabledFeatures {
		args = append(args, "--disable", feature)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "-")

	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, c.CommandRunner, "codex", "codex", args, prompt)
	if err != nil {
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("codex text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return result, nil
}
