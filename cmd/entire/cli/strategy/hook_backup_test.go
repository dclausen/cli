package strategy

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
)

var backupClock = time.Date(2026, 10, 1, 17, 4, 5, 0, time.UTC)

const (
	userHookV1 = "#!/bin/sh\necho v1\n"
	userHookV2 = "#!/bin/sh\necho v2\n"
)

// hooksFixture is a hooks dir and a separate lock dir, both opened as roots.
type hooksFixture struct {
	t        *testing.T
	dir      string
	root     *os.Root
	lockRoot *os.Root
}

func newHooksFixture(t *testing.T) *hooksFixture {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	lockRoot, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close(); lockRoot.Close() })
	return &hooksFixture{t: t, dir: dir, root: root, lockRoot: lockRoot}
}

func (f *hooksFixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *hooksFixture) read(name string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		f.t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func (f *hooksFixture) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(f.dir, name))
	return err == nil
}

func (f *hooksFixture) install(specs ...hookSpec) {
	f.t.Helper()
	if _, err := installHooks(context.Background(), f.lockRoot, f.root, f.dir, specs, backupClock); err != nil {
		f.t.Fatalf("installHooks: %v", err)
	}
}

func (f *hooksFixture) olderCopies() []string {
	f.t.Helper()
	copies, err := olderHookCopies(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	return copies
}

// contents returns what each name holds, for comparing sets of versions.
func (f *hooksFixture) contents(names ...string) []string {
	f.t.Helper()
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, f.read(n))
	}
	slices.Sort(out)
	return out
}

func specFor(t *testing.T, name string) hookSpec {
	t.Helper()
	for _, spec := range buildHookSpecs(bareEntireHookCmd) {
		if spec.name == name {
			return spec
		}
	}
	t.Fatalf("no hook spec %s", name)
	return hookSpec{}
}

func TestInstallHooks_ChangedHookKeepsEveryVersion(t *testing.T) {
	t.Parallel()
	f := newHooksFixture(t)
	spec := specFor(t, "pre-push")
	f.write("pre-push"+backupSuffix, userHookV1)
	f.write("pre-push", userHookV2)

	f.install(spec)

	if got := f.read("pre-push"); got != generateChainedContent(spec.content, spec.name) {
		t.Errorf("hook = %q, want Entire's chained hook", got)
	}
	if got := f.read("pre-push" + backupSuffix); got != userHookV2 {
		t.Errorf("backup = %q, want the current hook", got)
	}
	older := f.olderCopies()
	if len(older) != 1 || f.read(older[0]) != userHookV1 {
		t.Errorf("older copies = %v, want one holding the previous backup", older)
	}
}

func TestInstallHooks_IdenticalHookKeepsNoCopy(t *testing.T) {
	t.Parallel()
	f := newHooksFixture(t)
	f.write("pre-push"+backupSuffix, userHookV1)
	f.write("pre-push", userHookV1)

	f.install(specFor(t, "pre-push"))

	if got := f.read("pre-push" + backupSuffix); got != userHookV1 {
		t.Errorf("backup = %q", got)
	}
	if older := f.olderCopies(); len(older) != 0 {
		t.Errorf("older copies = %v, want none", older)
	}
}

// A hook manager that keeps rewriting the hook must not grow a copy per install.
func TestInstallHooks_AlternatingHookKeepsOneCopyPerVersion(t *testing.T) {
	t.Parallel()
	f := newHooksFixture(t)
	spec := specFor(t, "pre-push")
	f.write("pre-push"+backupSuffix, userHookV1)
	for _, v := range []string{userHookV2, userHookV1, userHookV2, userHookV1} {
		f.write("pre-push", v)
		f.install(spec)
	}

	older := f.olderCopies()
	if got := f.contents(older...); !slices.Equal(got, []string{userHookV1, userHookV2}) {
		t.Errorf("older copies hold %q, want one each of v1 and v2", got)
	}
}

func TestInstallHooks_SymlinkedHookIsKeptAsALink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f := newHooksFixture(t)
	f.write("pre-push"+backupSuffix, userHookV1)
	if err := os.Symlink("../../scripts/pre-push", filepath.Join(f.dir, "pre-push")); err != nil {
		t.Fatal(err)
	}

	f.install(specFor(t, "pre-push"))

	target, err := os.Readlink(filepath.Join(f.dir, "pre-push"+backupSuffix))
	if err != nil || target != "../../scripts/pre-push" {
		t.Errorf("backup link = %q, %v; want the hook's link", target, err)
	}
}

// A backup carrying Entire's marker would make the hook call itself.
func TestInstallHooks_NeverChainsToEntiresOwnHook(t *testing.T) {
	t.Parallel()
	spec := specFor(t, "pre-push")

	t.Run("replaced by the foreign hook", func(t *testing.T) {
		t.Parallel()
		f := newHooksFixture(t)
		f.write("pre-push"+backupSuffix, generateChainedContent(spec.content, spec.name))
		f.write("pre-push", userHookV1)

		f.install(spec)

		if got := f.read("pre-push" + backupSuffix); got != userHookV1 {
			t.Errorf("backup = %q, want the foreign hook", got)
		}
		if older := f.olderCopies(); len(older) != 0 {
			t.Errorf("older copies = %v, want none", older)
		}
	})

	t.Run("not chained", func(t *testing.T) {
		t.Parallel()
		f := newHooksFixture(t)
		f.write("pre-push"+backupSuffix, spec.content)

		f.install(spec)

		if got := f.read("pre-push"); got != spec.content {
			t.Errorf("hook = %q, want Entire's hook without the chain", got)
		}
	})
}

func TestInstallHooks_ConcurrentInstallsLoseNothing(t *testing.T) {
	t.Parallel()
	f := newHooksFixture(t)
	spec := specFor(t, "pre-push")
	f.write("pre-push"+backupSuffix, userHookV1)
	f.write("pre-push", userHookV2)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := installHooks(context.Background(), f.lockRoot, f.root, f.dir, []hookSpec{spec}, backupClock); err != nil {
				t.Errorf("installHooks: %v", err)
			}
		})
	}
	wg.Wait()

	kept := f.contents(append(f.olderCopies(), "pre-push"+backupSuffix)...)
	if !slices.Contains(kept, userHookV1) || !slices.Contains(kept, userHookV2) {
		t.Errorf("kept %q, want both versions", kept)
	}
}

func TestInstallHooks_LockTimeoutNamesTheLockFile(t *testing.T) {
	t.Parallel()
	f := newHooksFixture(t)
	release, err := flock.AcquireIn(f.lockRoot, hooksLockName)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = installHooks(context.Background(), f.lockRoot, f.root, f.dir, []hookSpec{specFor(t, "pre-push")}, backupClock)
	if err == nil || !strings.Contains(err.Error(), hooksLockName) {
		t.Errorf("err = %v, want a timeout naming %s", err, hooksLockName)
	}
}
