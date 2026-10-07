package antigravity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText submits a non-interactive prompt to the Antigravity CLI. The
// binary is `agy`; -p is the short alias for --print (single-prompt mode).
//
// The prompt travels in argv. Earlier releases accepted it on stdin behind a
// single-space -p placeholder (the Gemini CLI convention, verified on agy
// 1.0.16), but agy 1.2.7 ignores stdin in print mode: `-p " "` fails with
// "Error: empty prompt", and `-p -` is answered as the literal message "-"
// (both observed live, trail 444, 2026-09-22), which is how
// `entire dispatch --local --agent antigravity` came to hand agy an empty
// prompt. argv is the only documented route ("Usage: agy --print 'your
// prompt here'").
//
// That makes prompt size this agent's problem in a way it is not for agents
// that use RunIsolatedTextGeneratorCLI's stdin. Linux caps a SINGLE argument
// at MAX_ARG_STRLEN (128 KiB) however large the total ARG_MAX is, so an
// unbounded prompt fails with E2BIG. summarize.maxCondensedTranscriptBytes is
// what keeps summary prompts inside it. Windows' ~32 KiB whole-command-line
// limit is tighter than any useful transcript budget and is not covered; a
// long enough prompt still fails there, loudly.
//
// agy also runs with a home directory of its own (isolatedHomeEnv). It reads
// its window-title command (~/.gemini/antigravity-cli/settings.json), its
// PreInvocation hooks (~/.gemini/config/hooks.json), MCP servers, and skills
// from the user's home, and print mode executes the title command and the
// hooks (observed live on agy 1.3.1). The empty working directory keeps
// workspace configuration out but not that, and agy has no flag to skip it,
// so a summary run, whose prompt carries untrusted transcript content, would
// otherwise execute whatever the user's global configuration names.
func (a *AntigravityAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"-p", prompt}
	if model != "" {
		args = append(args, "--model", model)
	}
	home, cleanup, err := agent.NewTextGenerationDir()
	if err != nil {
		return "", fmt.Errorf("antigravity text generation failed: %w", err)
	}
	defer cleanup()
	env, err := isolatedHomeEnv(home)
	if err != nil {
		return "", fmt.Errorf("antigravity text generation failed: %w", err)
	}
	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, a.CommandRunner, "agy", "antigravity", args, "", env...)
	if err != nil {
		if strings.Contains(capturedStderr, "Authentication required") {
			err = fmt.Errorf("%w: agy runs summaries with an isolated home so your agy settings, hooks, and MCP servers are not loaded, and it could not find its sign-in there; use API-key authentication for agy or choose another summary provider", err)
		}
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("antigravity text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return result, nil
}

// isolatedHomeEnv returns the environment overrides that point agy at home
// instead of the user's home directory: HOME (USERPROFILE on Windows, where
// Go's os.UserHomeDir reads it) and the XDG base directories.
//
// This keeps agy from LOADING the user's configuration; it is not a
// filesystem sandbox. agy has no switch to remove its tools, so an injected
// instruction can still name an absolute path, which only the empty working
// directory and agy's own approvals stand against.
//
// agy keeps its sign-in in the macOS login keychain, which it locates through
// $HOME, so on macOS the login keychain FILE is linked in (linkLoginKeychain).
// Elsewhere the credential stores agy uses (Windows Credential Manager, the
// Secret Service on Linux) are not under the home directory, and API-key
// authentication comes from the environment, which is inherited unchanged.
// agy's file-backed token fallback, used where no keyring is reachable, and
// gcloud application-default credentials both live under the home directory
// and are deliberately not carried over: a machine that signs in that way
// fails to authenticate, which GenerateText reports, rather than falling back
// to the user's configuration.
func isolatedHomeEnv(home string) ([]string, error) {
	if runtime.GOOS == "darwin" {
		if err := linkLoginKeychain(home); err != nil {
			return nil, err
		}
	}
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
	}
	if runtime.GOOS == "windows" {
		env = append(env, "USERPROFILE="+home)
	}
	return env, nil
}

// linkLoginKeychain links home/Library/Keychains/login.keychain-db to the
// user's login keychain. The link is to the file, not the Keychains
// directory: a directory link would make home/Library/Keychains/../.. the
// real home, while a file has no ".." to walk. The keychain file is
// encrypted and every read goes through securityd, as it does when agy runs
// normally. A user without a resolvable home or login keychain gets no link,
// and agy reports that it is not signed in.
func linkLoginKeychain(home string) error {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil //nolint:nilerr // no home to link from: agy reports the missing sign-in
	}
	keychain := filepath.Join(realHome, "Library", "Keychains", "login.keychain-db")
	if _, err := os.Stat(keychain); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat login keychain: %w", err)
	}
	dir := filepath.Join(home, "Library", "Keychains")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create isolated home: %w", err)
	}
	if err := os.Symlink(keychain, filepath.Join(dir, "login.keychain-db")); err != nil {
		return fmt.Errorf("link login keychain into isolated home: %w", err)
	}
	return nil
}
