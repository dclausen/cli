package antigravity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// agy 1.2.7 ignores stdin in print mode (`-p " "` fails with "empty prompt",
// `-p -` is answered as the literal message "-"), so the prompt must travel in
// argv. This pins the shape `entire dispatch --local --agent antigravity` and
// `explain --generate` depend on.
func TestGenerateText_PassesPromptInArgv(t *testing.T) {
	t.Parallel()
	var gotBinary string
	var gotArgs []string
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, binary string, argv ...string) *exec.Cmd {
		gotBinary, gotArgs = binary, argv
		return exec.CommandContext(ctx, "echo", "PONG")
	}}

	out, err := a.GenerateText(context.Background(), "Summarize this transcript.", "gemini-3.8-flash-low")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if out != "PONG" {
		t.Fatalf("GenerateText output = %q, want the CLI's stdout", out)
	}
	if gotBinary != "agy" {
		t.Fatalf("binary = %q, want agy", gotBinary)
	}
	want := []string{"-p", "Summarize this transcript.", "--model", "gemini-3.8-flash-low"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args = %q, want %q", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args = %q, want %q", gotArgs, want)
		}
	}
}

// agy reads its window-title command and its PreInvocation hooks from the
// user's home (~/.gemini/antigravity-cli/settings.json and
// ~/.gemini/config/hooks.json), and print mode runs both (verified live on agy
// 1.3.1). A summary prompt carries untrusted transcript content, so the run
// gets a home of its own: nothing from the user's agy configuration loads.
// On macOS the login keychain file, where agy keeps its sign-in, is linked in
// so authentication still works.
func TestGenerateText_RunsWithAnIsolatedHome(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's environment")
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			`printf 'home=%s\ncwd=%s\nentries=%s\nkeychains=%s\nxdg=%s\n' "$HOME" "$(pwd -P)" "$(ls -A "$HOME" | tr '\n' ' ')" "$(readlink "$HOME/Library/Keychains/login.keychain-db")" "$XDG_CONFIG_HOME"`)
	}}

	out, err := a.GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = strings.TrimSpace(v)
		}
	}
	if got["home"] == "" || got["home"] == realHome {
		t.Fatalf("agy ran with the user's home %q; want an isolated one", got["home"])
	}
	if strings.HasPrefix(got["cwd"], got["home"]) {
		t.Errorf("working directory %q is inside the isolated home %q; the working directory must stay empty", got["cwd"], got["home"])
	}
	if !strings.HasPrefix(got["xdg"], got["home"]) {
		t.Errorf("XDG_CONFIG_HOME = %q, want it inside the isolated home %q", got["xdg"], got["home"])
	}
	wantEntries := ""
	if runtime.GOOS == "darwin" {
		// Only the keychain file, so no ".." walks back into the real home.
		want := ""
		if keychain := filepath.Join(realHome, "Library", "Keychains", "login.keychain-db"); fileExists(keychain) {
			want, wantEntries = keychain, "Library"
		}
		if got["keychains"] != want {
			t.Errorf("login.keychain-db links to %q, want %q", got["keychains"], want)
		}
	}
	if got["entries"] != wantEntries {
		t.Errorf("isolated home holds %q, want %q", got["entries"], wantEntries)
	}
}

func TestGenerateText_SignInFailureNamesTheIsolation(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to fake agy")
	}
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `echo "Authentication required. Please visit the URL to log in:" >&2; exit 1`)
	}}
	_, err := a.GenerateText(context.Background(), "prompt", "")
	if err == nil {
		t.Fatal("GenerateText succeeded; want the sign-in failure")
	}
	if !strings.Contains(err.Error(), "isolated home") {
		t.Errorf("error does not explain the isolated home: %v", err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
