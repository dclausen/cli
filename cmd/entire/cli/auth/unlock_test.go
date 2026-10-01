package auth

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// fakeProcs is a process table: pid -> (ppid, start).
type fakeProcs map[int]procInfo

// unlockFixture installs a fake process table and peer identity and points
// the cache dir at a short temp path (unix socket paths are length-limited).
// peer is the pid the server will see on every connection.
func unlockFixture(t *testing.T, procs fakeProcs, peer int) {
	t.Helper()
	const ppid = 100 // the user's git in pushTree
	// t.TempDir lives under a long per-test path on macOS; sockets need
	// something short.
	dir, err := os.MkdirTemp("/tmp", "entire-unlock-") //nolint:usetesting // t.TempDir is too long for a unix socket path on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		osroot.Forget(filepath.Join(dir, "entire"))
		_ = os.RemoveAll(dir)
	})
	t.Setenv("XDG_CACHE_HOME", dir)

	prevProc, prevPeer, prevPPID, prevWalk, prevServe := procLookup, peerLookup, currentPPID, walkStartPID, serveBundles
	procLookup = func(pid int) (procInfo, error) {
		info, ok := procs[pid]
		if !ok {
			return procInfo{}, os.ErrNotExist
		}
		return info, nil
	}
	peerLookup = func(*net.UnixConn) (int, int, error) { return peer, os.Getuid(), nil }
	currentPPID = func() int { return ppid }
	walkStartPID = func() int { return ppid }
	prompted.Store(true)
	forgetBundles()
	t.Cleanup(func() {
		procLookup, peerLookup, currentPPID, walkStartPID, serveBundles = prevProc, prevPeer, prevPPID, prevWalk, prevServe
		prompted.Store(false)
		forgetBundles()
	})
}

// serveCurrentBundles freezes what the server hands out, so a test can then
// clear the shared cache to play a fresh client process.
func serveCurrentBundles() {
	fixed := snapshotBundles()
	serveBundles = func() map[string]tokenBundle { return fixed }
}

// A user's git (100) started the helper under test; the hook chain is
// git(100) -> hook(200) -> nested git(300). An unrelated agent is 900.
func pushTree() fakeProcs {
	return fakeProcs{
		100: {ppid: 1, start: 1_000},
		200: {ppid: 100, start: 1_100},
		300: {ppid: 200, start: 1_200},
		900: {ppid: 1, start: 500},
	}
}

func TestUnlock_DescendantGetsBundles(t *testing.T) {
	unlockFixture(t, pushTree(), 300)
	rememberBundle("se1:abc|123", tokenBundle{Version: 1, Issuer: "https://core", Handle: "h", Access: "acc"})
	serveCurrentBundles()

	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatalf("StartUnlockServer: %v", err)
	}
	defer stop()

	// The nested helper walks up from its parent (300) and finds git(100).
	walkStartPID = func() int { return 300 }
	forgetBundles()
	got := fetchUnlockedBundles()
	if got == nil || got["se1:abc|123"].Access != "acc" {
		t.Fatalf("descendant did not receive bundles: %+v", got)
	}
}

func TestUnlock_UnrelatedPeerRefused(t *testing.T) {
	unlockFixture(t, pushTree(), 900)
	rememberBundle("se1:abc|123", tokenBundle{Version: 1, Access: "acc"})
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Even a client that knows the socket name gets nothing.
	walkStartPID = func() int { return 100 }
	forgetBundles()
	if got := fetchUnlockedBundles(); got != nil {
		t.Fatalf("unrelated peer received bundles: %+v", got)
	}
}

func TestUnlock_ReusedGitPIDRefused(t *testing.T) {
	procs := pushTree()
	unlockFixture(t, procs, 300)
	rememberBundle("se1:abc|123", tokenBundle{Version: 1, Access: "acc"})
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// git exited and pid 100 now belongs to a newer process.
	procs[100] = procInfo{ppid: 1, start: 9_000}
	walkStartPID = func() int { return 300 }
	forgetBundles()
	if got := fetchUnlockedBundles(); got != nil {
		t.Fatalf("reused pid received bundles: %+v", got)
	}
}

func TestUnlock_ReparentedServerRefuses(t *testing.T) {
	unlockFixture(t, pushTree(), 300)
	rememberBundle("se1:abc|123", tokenBundle{Version: 1, Access: "acc"})
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// The serving helper's parent is now launchd: git is gone. Dial the
	// socket git(100) owned directly, bypassing the client's ancestry walk.
	currentPPID = func() int { return 1 }
	forgetBundles()
	path := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "entire", unlockDirName, unlockSocketName(100))
	if got := dialUnlock(path); got != nil {
		t.Fatalf("reparented server still served: %+v", got)
	}
}

func TestUnlock_NoServerWithoutPrompt(t *testing.T) {
	unlockFixture(t, pushTree(), 300)
	prompted.Store(false)
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stop()
	walkStartPID = func() int { return 300 }
	if got := fetchUnlockedBundles(); got != nil {
		t.Fatalf("server ran without a prompt: %+v", got)
	}
}

func TestUnlock_OpenSealedSlotUsesUnlock(t *testing.T) {
	unlockFixture(t, pushTree(), 300)
	fs := &fakeSealer{}
	SetSealerForTesting(t, fs)
	prompted.Store(true)
	sl, err := protection.sealer()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := sealSlot(sl, tokenBundle{Issuer: "https://core.example.test", Handle: "h", Access: "acc"}, 600)
	if err != nil {
		t.Fatal(err)
	}
	serveCurrentBundles()
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// A fresh nested process: nothing cached, but an unlock is reachable.
	walkStartPID = func() int { return 300 }
	forgetBundles()
	b, _, err := openSealedSlot(enc, "https://core.example.test", "h", "test")
	if err != nil {
		t.Fatal(err)
	}
	if b.Access != "acc" || fs.count() != 0 {
		t.Fatalf("unlock not used: bundle=%+v prompts=%d", b, fs.count())
	}
}
