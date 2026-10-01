package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Per-push unlock.
//
// A user's `git push` to an entire:// remote runs one helper process for the
// whole push, and the pre-push hook spawns further git processes that each
// need the same login. Each would otherwise show its own dialog. Instead,
// the helper that passed the dialog serves its unsealed bundles over a Unix
// socket, and only to processes that descend from its parent git process.
//
// The plaintext never touches disk. The socket carries no secret on its
// own: a client must be a descendant of the serving helper's parent, checked
// through the kernel-reported peer pid and the process table, and the
// parent must still be the same process it was when serving began. The
// hardened-runtime release builds keep other processes out of the helper's
// memory; see docs/architecture/token-protection.md.

const (
	unlockDirName  = "unlock"
	unlockMaxDepth = 16
	unlockTimeout  = 2 * time.Second
	unlockMaxBytes = 1 << 20
	// Unix socket paths are limited to 104 bytes on macOS.
	unlockMaxPath = 100
)

// ErrUnlockUnsupported: this platform cannot identify socket peers.
var ErrUnlockUnsupported = errors.New("per-push unlock unsupported on this platform")

// procInfo is what the ancestry check needs from the process table.
type procInfo struct {
	ppid  int
	start int64 // process start, microseconds since the epoch
}

// Platform hooks; tests substitute fakes. serveBundles is what a server
// hands out, swapped in tests because server and client share one cache.
var (
	procLookup   = lookupProc
	peerLookup   = lookupPeer
	currentPPID  = os.Getppid // the server's parent, re-read per request
	walkStartPID = os.Getppid // where a client's ancestry walk begins
	serveBundles = snapshotBundles
)

// prompted records that this process passed the dialog itself, which is
// what qualifies it to serve others.
var prompted atomic.Bool

// unlockPayload is the wire format, one JSON document then EOF.
type unlockPayload struct {
	Version int                    `json:"v"`
	Bundles map[string]tokenBundle `json:"bundles"`
}

func unlockSocketName(gitPID int) string {
	return fmt.Sprintf("git-%d.sock", gitPID)
}

// UnlockServer serves this process's unsealed bundles to descendants of
// its parent git process.
type UnlockServer struct {
	ln       *net.UnixListener
	root     *os.Root
	name     string
	gitPID   int
	gitStart int64
	uid      int
	wg       sync.WaitGroup
}

// StartUnlockServer starts serving when this process unsealed a bundle
// through the dialog. It returns a no-op stop when there is nothing to
// serve or the platform cannot vouch for peers.
func StartUnlockServer(ctx context.Context) (func(), error) {
	noop := func() {}
	if !prompted.Load() {
		return noop, nil
	}
	gitPID := currentPPID()
	if gitPID <= 1 {
		return noop, nil
	}
	git, err := procLookup(gitPID)
	if err != nil {
		if errors.Is(err, ErrUnlockUnsupported) {
			return noop, nil
		}
		return noop, fmt.Errorf("inspect parent process: %w", err)
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		return noop, fmt.Errorf("open cache dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, unlockDirName, 0o700); err != nil {
		return noop, fmt.Errorf("create unlock dir: %w", err)
	}
	dir, err := userdirs.CacheDirChecked()
	if err != nil {
		return noop, fmt.Errorf("resolve cache dir: %w", err)
	}
	name := filepath.Join(unlockDirName, unlockSocketName(gitPID))
	path := filepath.Join(dir, name)
	if len(path) > unlockMaxPath {
		return noop, fmt.Errorf("unlock socket path too long: %d bytes", len(path))
	}
	// A stale socket from a dead helper with the same parent pid.
	if err := osroot.RemoveNoSymlinks(root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return noop, fmt.Errorf("remove stale unlock socket: %w", err)
	}
	s := &UnlockServer{root: root, name: name, gitPID: gitPID, gitStart: git.start, uid: os.Getuid()}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return noop, fmt.Errorf("listen on unlock socket: %w", err)
	}
	// Belt and braces with the 0700 directory: the check is the peer test.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return noop, fmt.Errorf("chmod unlock socket: %w", err)
	}
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop(ctx)
	return s.stop, nil
}

func (s *UnlockServer) stop() {
	_ = s.ln.Close()
	_ = osroot.RemoveNoSymlinks(s.root, s.name) //nolint:errcheck // best-effort cleanup; a stale socket is removed on the next start
	s.wg.Wait()
}

func (s *UnlockServer) acceptLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		conn, err := s.ln.AcceptUnix()
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			_ = conn.Close()
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

func (s *UnlockServer) handle(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(unlockTimeout)) //nolint:errcheck // a failed deadline only loses the timeout; the peer check still runs
	pid, uid, err := peerLookup(conn)
	if err != nil || uid != s.uid || !s.authorized(pid) {
		return
	}
	payload := unlockPayload{Version: bundleVersion, Bundles: serveBundles()}
	_ = json.NewEncoder(conn).Encode(payload) //nolint:errcheck,errchkjson // the client falls back to its own dialog on a short read
}

// authorized reports whether peerPID descends from the same, still-running
// git process this server was started under.
func (s *UnlockServer) authorized(peerPID int) bool {
	if currentPPID() != s.gitPID {
		return false // git exited; we were reparented
	}
	git, err := procLookup(s.gitPID)
	if err != nil || git.start != s.gitStart {
		return false // pid reused
	}
	pid := peerPID
	for depth := 0; depth < unlockMaxDepth && pid > 1; depth++ {
		if pid == s.gitPID {
			return true
		}
		info, err := procLookup(pid)
		if err != nil || info.start < s.gitStart {
			return false // a descendant cannot predate its ancestor
		}
		pid = info.ppid
	}
	return false
}

func snapshotBundles() map[string]tokenBundle {
	unsealedCache.mu.Lock()
	defer unsealedCache.mu.Unlock()
	out := make(map[string]tokenBundle, len(unsealedCache.m))
	for k, v := range unsealedCache.m {
		out[k] = v
	}
	return out
}

// fetchUnlockedBundles asks an ancestor's helper for its bundles. It walks
// this process's parents looking for a socket named after each, so only a
// process inside a running push finds one. Nil means no unlock available.
func fetchUnlockedBundles() map[string]tokenBundle {
	root, err := userdirs.CacheRootForRead()
	if err != nil {
		return nil
	}
	dir, err := userdirs.CacheDirChecked()
	if err != nil {
		return nil
	}
	pid := walkStartPID()
	for depth := 0; depth < unlockMaxDepth && pid > 1; depth++ {
		name := filepath.Join(unlockDirName, unlockSocketName(pid))
		if _, err := osroot.LstatNoSymlinks(root, name); err == nil {
			if m := dialUnlock(filepath.Join(dir, name)); m != nil {
				return m
			}
		}
		info, err := procLookup(pid)
		if err != nil {
			return nil
		}
		pid = info.ppid
	}
	return nil
}

func dialUnlock(path string) map[string]tokenBundle {
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	dialer := net.Dialer{Timeout: unlockTimeout}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil
	}
	// Only accept bundles from a process of our own user.
	if _, uid, err := peerLookup(uc); err != nil || uid != os.Getuid() {
		return nil
	}
	_ = conn.SetDeadline(time.Now().Add(unlockTimeout)) //nolint:errcheck // a failed deadline only loses the timeout on a local socket
	raw, err := io.ReadAll(io.LimitReader(conn, unlockMaxBytes))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var payload unlockPayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Version != bundleVersion {
		return nil
	}
	return payload.Bundles
}
