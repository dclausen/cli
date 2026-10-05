package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// hooksLockName serializes hook installs and removals across Entire processes.
// It lives in the git common dir, never the hooks dir: a tracked core.hooksPath
// would show it as an untracked file.
const hooksLockName = "entire-hooks.lock"

// hooksLockTimeout bounds the wait; the moves under the lock take milliseconds.
const hooksLockTimeout = time.Second

func openHooksLockRoot(ctx context.Context) (*os.Root, error) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(commonDir)
	if err != nil {
		return nil, fmt.Errorf("open git common dir: %w", err)
	}
	return root, nil
}

func acquireHooksLock(ctx context.Context, lockRoot *os.Root) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, hooksLockTimeout)
	defer cancel()
	release, err := flock.AcquireContextIn(ctx, lockRoot, hooksLockName)
	if err != nil {
		return nil, fmt.Errorf("another Entire process is changing git hooks (lock %s): %w", filepath.Join(lockRoot.Name(), hooksLockName), err)
	}
	return release, nil
}

// backupAction is what prepareHookBackup did with a foreign hook.
type backupAction int

const (
	backupCreated      backupAction = iota // <hook>.pre-entire now holds it
	backupReplacedSame                     // identical to <hook>.pre-entire
	backupRotated                          // a different backup was kept as an older copy
)

const (
	// tempPrefix names Entire's in-flight copies; installs sweep leftovers.
	tempPrefix = ".entire-tmp-"
	// olderCopyStampLayout names older backups; no colons, for Windows.
	olderCopyStampLayout = "20060102T150405Z"
)

// prepareHookBackup makes <name>.pre-entire hold the foreign hook at name. It
// only links or copies, never moves name away, so name keeps a runnable hook
// until the caller's atomic write replaces it, and a crash at any step leaves
// every version on disk. A different existing backup is kept as
// <name>.pre-entire.<timestamp> unless an identical older copy exists. The
// caller holds the hooks lock.
func prepareHookBackup(root *os.Root, name string, now time.Time) (backupAction, string, error) {
	backup := name + backupSuffix
	if hookFileExists(root, backup) && sameHookFile(root, name, backup) {
		return backupReplacedSame, "", nil
	}
	action, older := backupCreated, ""
	if hookFileExists(root, backup) && !carriesEntireMarker(root, backup) {
		action = backupRotated
		if older = identicalOlderCopy(root, backup); older == "" {
			var err error
			if older, err = keepOlderCopy(root, backup, now); err != nil {
				return 0, "", err
			}
		}
	}
	if err := replaceWithCopy(root, name, backup); err != nil {
		return 0, "", err
	}
	return action, older, nil
}

// replaceWithCopy atomically replaces dst with a link to (or copy of) src.
func replaceWithCopy(root *os.Root, src, dst string) error {
	tmp := tempPrefix + dst
	if err := root.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove leftover %s: %w", tmp, err)
	}
	if err := linkOrCopy(root, src, tmp); err != nil {
		return err
	}
	if err := root.Rename(tmp, dst); err != nil {
		return errors.Join(fmt.Errorf("replace %s: %w", dst, err), root.Remove(tmp))
	}
	return nil
}

// linkOrCopy creates dst holding src, failing if dst exists. A hard link keeps
// a symlink a symlink; where links are unsupported it copies instead.
func linkOrCopy(root *os.Root, src, dst string) error {
	err := root.Link(src, dst)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return err //nolint:wrapcheck // callers check fs.ErrExist
	}
	info, err := root.Lstat(src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := root.Readlink(src)
		if err != nil {
			return fmt.Errorf("copy %s: %w", src, err)
		}
		return root.Symlink(target, dst) //nolint:wrapcheck // callers check fs.ErrExist
	}
	data, err := osroot.ReadFileNoFollow(root, src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	f, err := root.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err //nolint:wrapcheck // callers check fs.ErrExist
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, root.Chmod(dst, 0o755)); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return nil
}

// keepOlderCopy links backup to a free <backup>.<timestamp>[-N] name; the link
// fails rather than replace a name another process took.
func keepOlderCopy(root *os.Root, backup string, now time.Time) (string, error) {
	base := backup + "." + now.UTC().Format(olderCopyStampLayout)
	for i := 1; i < 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		err := linkOrCopy(root, backup, name)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("keep an older copy of %s: %w", backup, err)
		}
	}
	return "", fmt.Errorf("no free name for an older copy of %s", backup)
}

func identicalOlderCopy(root *os.Root, backup string) string {
	copies, err := olderHookCopies(root)
	if err != nil {
		return ""
	}
	for _, c := range copies {
		if strings.HasPrefix(c, backup+".") && sameHookFile(root, c, backup) {
			return c
		}
	}
	return ""
}

// olderHookCopies lists <hook>.pre-entire.<timestamp> files in root, sorted.
func olderHookCopies(root *os.Root) ([]string, error) {
	names, err := hookDirNames(root)
	if err != nil {
		return nil, err
	}
	var copies []string
	for _, n := range names {
		if slices.ContainsFunc(gitHookNames, func(hook string) bool {
			return strings.HasPrefix(n, hook+backupSuffix+".")
		}) {
			copies = append(copies, n)
		}
	}
	slices.Sort(copies)
	return copies, nil
}

// removeLeftoverTemps deletes copies an interrupted install left behind.
func removeLeftoverTemps(root *os.Root) error {
	names, err := hookDirNames(root)
	if err != nil {
		return err
	}
	for _, n := range names {
		if strings.HasPrefix(n, tempPrefix) {
			if err := root.Remove(n); err != nil {
				return fmt.Errorf("remove leftover %s: %w", n, err)
			}
		}
	}
	return nil
}

func hookDirNames(root *os.Root) ([]string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open hooks dir: %w", err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("read hooks dir: %w", err)
	}
	return names, nil
}

// sameHookFile: a and b are regular files with identical bytes, or symlinks to
// the same target.
func sameHookFile(root *os.Root, a, b string) bool {
	infoA, errA := root.Lstat(a)
	infoB, errB := root.Lstat(b)
	if errA != nil || errB != nil {
		return false
	}
	linkA, linkB := infoA.Mode()&fs.ModeSymlink != 0, infoB.Mode()&fs.ModeSymlink != 0
	if linkA != linkB {
		return false
	}
	if linkA {
		targetA, errA := root.Readlink(a)
		targetB, errB := root.Readlink(b)
		return errA == nil && errB == nil && targetA == targetB
	}
	dataA, errA := osroot.ReadFileNoFollow(root, a)
	dataB, errB := osroot.ReadFileNoFollow(root, b)
	return errA == nil && errB == nil && bytes.Equal(dataA, dataB)
}

// carriesEntireMarker: name is a regular file containing Entire's marker.
func carriesEntireMarker(root *os.Root, name string) bool {
	data, err := osroot.ReadFileNoFollow(root, name)
	return err == nil && bytes.Contains(data, []byte(entireHookMarker))
}
