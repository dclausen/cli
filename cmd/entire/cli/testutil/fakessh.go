package testutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// InstallFakeSSH puts an `ssh` first on PATH that answers `ssh -G <host>` the
// way OpenSSH does, with "hostname <aliases[host]>" for a listed alias and
// "hostname <host>" otherwise, and clears GIT_SSH_COMMAND and GIT_SSH so git
// would run plain ssh. It never touches the developer's ~/.ssh/config, which
// OpenSSH reads from the passwd entry rather than $HOME and so cannot be
// redirected per test. Callers should also isolate git config
// (IsolateGitConfigEnv) so a core.sshCommand cannot leak in.
//
// Changes process-global state (PATH): the calling test must not be parallel.
// Skipped on Windows, which cannot run the shell script.
func InstallFakeSSH(t *testing.T, aliases map[string]string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh is a shell script")
	}
	hosts := make([]string, 0, len(aliases))
	for host := range aliases {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	var cases strings.Builder
	for _, host := range hosts {
		cases.WriteString("  " + host + ") echo \"hostname " + aliases[host] + "\" ;;\n")
	}
	script := "#!/bin/sh\n" +
		"[ \"$1\" = \"-G\" ] || exit 255\n" +
		"echo \"user git\"\n" +
		"case \"$2\" in\n" + cases.String() +
		"  *) echo \"hostname $2\" ;;\n" +
		"esac\n" +
		"echo \"port 22\"\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil { //nolint:gosec // test helper must be executable
		t.Fatalf("write fake ssh: %v", err)
	}
	// Run it once untimed: the first exec of a freshly written script can be
	// slow (macOS scans new executables), and that cost must not land inside
	// the caller's bounded ssh -G call.
	if out, err := exec.CommandContext(context.Background(), filepath.Join(dir, "ssh"), "-G", "warmup").CombinedOutput(); err != nil {
		t.Fatalf("fake ssh warm-up: %v: %s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
}
