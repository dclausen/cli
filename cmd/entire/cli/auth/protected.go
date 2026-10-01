package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/entireclient/tokenstore/senclave"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Secure Enclave token protection.
//
// With protection on, a login's access and refresh tokens are sealed into
// one bundle under a Secure Enclave key and stored in the context's access
// slot; the refresh slot is cleared. Sealing uses the public half and never
// prompts, so login and refresh rotation stay silent. Unsealing needs the
// private half, so every token read makes macOS show its Touch ID (or
// password) dialog naming the action. The sealed value keeps the usual
// "|<expiry>" suffix so expiry stays readable without a prompt.
//
// Protection is on when the key blob exists in the per-user config dir.
// There is no flag to flip: an agent cannot turn it off without passing the
// prompt, because the plaintext it would need is what the prompt guards.

const (
	// ProtectedKeyFile holds the Secure Enclave key blob.
	ProtectedKeyFile = "token-key.sekey"
	sealedPrefix     = "se1:"
	bundleVersion    = 1
)

// Sentinel errors for the protection layer.
var (
	// ErrProtectionOff: no Secure Enclave key is enrolled.
	ErrProtectionOff = errors.New("tokens are not Secure Enclave protected")
	// ErrPromptDeclined: the user dismissed the dialog, or none could show.
	ErrPromptDeclined = errors.New("authentication prompt declined")
	// ErrBundleMismatch: the sealed token names a different issuer or handle.
	ErrBundleMismatch = errors.New("sealed token does not match this context; contexts.json may have been edited")
)

// tokenBundle is the plaintext a sealed slot decrypts to. Issuer and handle
// bind the tokens to their context so a rewritten contexts.json cannot
// redirect them to another login server.
type tokenBundle struct {
	Version int    `json:"v"`
	Issuer  string `json:"issuer"`
	Handle  string `json:"handle"`
	Access  string `json:"access"`
	Refresh string `json:"refresh,omitempty"`
}

// sealerSource caches the enrolled sealer. Tests swap open.
type sealerSource struct {
	mu     sync.Mutex
	open   func() (senclave.Sealer, error)
	cached senclave.Sealer
}

var protection = sealerSource{open: openEnclaveSealer}

// sealer returns the enrolled sealer, or ErrProtectionOff.
func (s *sealerSource) sealer() (senclave.Sealer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil {
		return s.cached, nil
	}
	sl, err := s.open()
	if err != nil {
		return nil, err
	}
	s.cached = sl
	return sl, nil
}

func (s *sealerSource) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached = nil
}

// SetSealerForTesting replaces the sealer source. Pass nil to simulate
// protection being off.
func SetSealerForTesting(t interface{ Cleanup(fn func()) }, sl senclave.Sealer) {
	protection.mu.Lock()
	prevOpen := protection.open
	protection.cached = nil
	protection.open = func() (senclave.Sealer, error) {
		if sl == nil {
			return nil, ErrProtectionOff
		}
		return sl, nil
	}
	protection.mu.Unlock()
	forgetBundles()
	t.Cleanup(func() {
		protection.mu.Lock()
		protection.open = prevOpen
		protection.cached = nil
		protection.mu.Unlock()
		forgetBundles()
	})
}

// openEnclaveSealer loads the key blob from the per-user config dir.
func openEnclaveSealer() (senclave.Sealer, error) {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrProtectionOff
		}
		return nil, fmt.Errorf("open config dir: %w", err)
	}
	blob, err := osroot.ReadFileNoFollow(root, ProtectedKeyFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrProtectionOff
		}
		return nil, fmt.Errorf("read %s: %w", ProtectedKeyFile, err)
	}
	key, err := senclave.Load(blob)
	if err != nil {
		return nil, fmt.Errorf("load Secure Enclave token key: %w", err)
	}
	return key, nil
}

// TokensProtected reports whether a Secure Enclave key is enrolled.
func TokensProtected() bool {
	_, err := protection.sealer()
	return err == nil
}

// PromptDeclined reports whether err came from a dismissed or unavailable
// dialog, so callers skip fallbacks that would prompt again.
func PromptDeclined(err error) bool {
	return errors.Is(err, ErrPromptDeclined)
}

func isSealed(encoded string) bool {
	return strings.HasPrefix(encoded, sealedPrefix)
}

func normalizeIssuer(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// expiresInFrom converts an absolute expiry back to the encoded form.
func expiresInFrom(expiresAt time.Time) int64 {
	if expiresAt.IsZero() {
		return int64(defaultSavedTokenTTL.Seconds())
	}
	if secs := int64(time.Until(expiresAt).Seconds()); secs > 0 {
		return secs
	}
	return 1
}

// sealSlot encodes b into a sealed slot value carrying expiresIn.
func sealSlot(sl senclave.Sealer, b tokenBundle, expiresIn int64) (string, error) {
	b.Version = bundleVersion
	b.Issuer = normalizeIssuer(b.Issuer)
	plain, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("encode token bundle: %w", err)
	}
	ct, err := sl.Seal(plain)
	if err != nil {
		return "", fmt.Errorf("seal tokens: %w", err)
	}
	encoded := tokenstore.EncodeTokenWithExpiration(sealedPrefix+base64.StdEncoding.EncodeToString(ct), expiresIn)
	// What this process just sealed it may read back without a prompt.
	rememberBundle(encoded, b)
	return encoded, nil
}

// unsealedCache remembers bundles this process already unsealed or sealed,
// keyed by the slot's exact encoded value. One command builds several token
// managers and each re-reads the slot; without this every read would be a
// dialog. Identical ciphertext means identical plaintext, and a slot
// rotated by another process has new ciphertext, so it prompts again.
var unsealedCache = struct {
	mu sync.Mutex
	m  map[string]tokenBundle
}{m: map[string]tokenBundle{}}

func cachedBundle(encoded string) (tokenBundle, bool) {
	unsealedCache.mu.Lock()
	defer unsealedCache.mu.Unlock()
	b, ok := unsealedCache.m[encoded]
	return b, ok
}

func rememberBundle(encoded string, b tokenBundle) {
	unsealedCache.mu.Lock()
	defer unsealedCache.mu.Unlock()
	unsealedCache.m[encoded] = b
}

func forgetBundles() {
	unsealedCache.mu.Lock()
	defer unsealedCache.mu.Unlock()
	unsealedCache.m = map[string]tokenBundle{}
}

// openSealedSlot decrypts a sealed slot value and checks it belongs to
// issuer and handle. reason is the dialog text. A bundle this process has
// already unsealed or sealed is returned without a prompt.
func openSealedSlot(encoded, issuer, handle, reason string) (tokenBundle, time.Time, error) {
	_, expiresAt := tokenstore.DecodeTokenWithExpiration(encoded)
	b, ok := cachedBundle(encoded)
	if !ok {
		// Inside a running push, the helper that already passed the
		// dialog can hand us its bundles.
		if m := fetchUnlockedBundles(); m != nil {
			for k, v := range m {
				rememberBundle(k, v)
			}
			b, ok = m[encoded]
		}
	}
	if !ok {
		var err error
		b, err = unsealSlot(encoded, reason)
		if err != nil {
			return tokenBundle{}, time.Time{}, err
		}
		prompted.Store(true)
	}
	if b.Version != bundleVersion || b.Issuer != normalizeIssuer(issuer) || b.Handle != handle {
		return tokenBundle{}, time.Time{}, ErrBundleMismatch
	}
	rememberBundle(encoded, b)
	return b, expiresAt, nil
}

// unsealSlot decrypts a sealed slot value through the enclave. Prompts.
func unsealSlot(encoded, reason string) (tokenBundle, error) {
	sl, err := protection.sealer()
	if err != nil {
		if errors.Is(err, ErrProtectionOff) {
			return tokenBundle{}, errors.New("stored tokens are sealed but no Secure Enclave key is enrolled")
		}
		return tokenBundle{}, err
	}
	payload, _ := tokenstore.DecodeTokenWithExpiration(encoded)
	ct, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(payload, sealedPrefix))
	if err != nil {
		return tokenBundle{}, fmt.Errorf("decode sealed tokens: %w", err)
	}
	plain, err := sl.Unseal(ct, reason)
	if err != nil {
		return tokenBundle{}, mapUnsealErr(err)
	}
	var b tokenBundle
	if err := json.Unmarshal(plain, &b); err != nil {
		return tokenBundle{}, fmt.Errorf("decode token bundle: %w", err)
	}
	return b, nil
}

func mapUnsealErr(err error) error {
	if errors.Is(err, senclave.ErrCanceled) || errors.Is(err, senclave.ErrNoInteraction) {
		return fmt.Errorf("%w: %w", ErrPromptDeclined, err)
	}
	return fmt.Errorf("unseal tokens: %w", err)
}

// Prompt reasons.

type promptActionKey struct{}

// WithPromptAction records what the caller is doing, for the dialog text.
// Example: "git push to us.entire.io".
func WithPromptAction(ctx context.Context, action string) context.Context {
	return context.WithValue(ctx, promptActionKey{}, action)
}

func promptActionFrom(ctx context.Context) string {
	if ctx != nil {
		if a, ok := ctx.Value(promptActionKey{}).(string); ok && a != "" {
			return a
		}
	}
	return defaultPromptAction()
}

// defaultPromptAction names the running command from its non-flag words.
func defaultPromptAction() string {
	words := []string{"entire"}
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "-") || len(words) >= 3 {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// promptReason formats the dialog text: '<binary> is trying to <reason>.'
func promptReason(action, issuer, handle string) string {
	return fmt.Sprintf("%s with Entire login %s", action, loginLabel(issuer, handle))
}

func loginLabel(issuer, handle string) string {
	if host, ok := hostOf(issuer); ok {
		return handle + "@" + host
	}
	return handle
}

// Enrollment.

// EnableProtection enrols a Secure Enclave key when none exists and seals
// every saved login. It never prompts. Returns the sealed context names.
func EnableProtection(cfgDir string) (sealed []string, created bool, err error) {
	sl, err := protection.sealer()
	if errors.Is(err, ErrProtectionOff) {
		if err := enrolKey(); err != nil {
			return nil, false, err
		}
		created = true
		sl, err = protection.sealer()
	}
	if err != nil {
		return nil, created, err
	}
	f, err := contexts.Load(cfgDir)
	if err != nil {
		return nil, created, fmt.Errorf("load contexts: %w", err)
	}
	for _, c := range f.Contexts {
		done, err := sealContext(sl, c)
		if err != nil {
			return sealed, created, fmt.Errorf("seal context %q: %w", c.Name, err)
		}
		if done {
			sealed = append(sealed, c.Name)
		}
	}
	return sealed, created, nil
}

func enrolKey() error {
	blob, err := senclave.Generate()
	if err != nil {
		return fmt.Errorf("create Secure Enclave key: %w", err)
	}
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return fmt.Errorf("open config dir: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, ProtectedKeyFile, blob, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", ProtectedKeyFile, err)
	}
	protection.reset()
	return nil
}

// sealContext seals one context's plaintext tokens in place.
func sealContext(sl senclave.Sealer, c *contexts.Context) (bool, error) {
	if c == nil || c.KeychainService == "" || c.Handle == "" {
		return false, nil
	}
	enc, err := tokenstore.Get(c.KeychainService, c.Handle)
	if errors.Is(err, tokenstore.ErrNotFound) || (err == nil && enc == "") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read access token: %w", err)
	}
	if isSealed(enc) {
		return false, nil
	}
	access, expiresAt := tokenstore.DecodeTokenWithExpiration(enc)
	refreshSlot := tokenstore.RefreshService(c.KeychainService)
	refresh, err := tokenstore.Get(refreshSlot, c.Handle)
	if err != nil && !errors.Is(err, tokenstore.ErrNotFound) {
		return false, fmt.Errorf("read refresh token: %w", err)
	}
	sealedEnc, err := sealSlot(sl, tokenBundle{Issuer: c.CoreURL, Handle: c.Handle, Access: access, Refresh: refresh}, expiresInFrom(expiresAt))
	if err != nil {
		return false, err
	}
	if err := tokenstore.Set(c.KeychainService, c.Handle, sealedEnc); err != nil {
		return false, fmt.Errorf("store sealed tokens: %w", err)
	}
	if err := tokenstore.Delete(refreshSlot, c.Handle); err != nil && !errors.Is(err, tokenstore.ErrNotFound) {
		return false, fmt.Errorf("clear refresh slot: %w", err)
	}
	return true, nil
}

// DisableProtection unseals every saved login back to plaintext, prompting
// once per context, then removes the key. Returns the unsealed names.
func DisableProtection(cfgDir string) (unsealed []string, err error) {
	if _, err := protection.sealer(); err != nil {
		return nil, err
	}
	f, err := contexts.Load(cfgDir)
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}
	for _, c := range f.Contexts {
		done, err := unsealContext(c)
		if err != nil {
			return unsealed, fmt.Errorf("unseal context %q: %w", c.Name, err)
		}
		if done {
			unsealed = append(unsealed, c.Name)
		}
	}
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return unsealed, fmt.Errorf("open config dir: %w", err)
	}
	if err := osroot.Remove(root, ProtectedKeyFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return unsealed, fmt.Errorf("remove %s: %w", ProtectedKeyFile, err)
	}
	protection.reset()
	forgetBundles()
	return unsealed, nil
}

// unsealContext rewrites one context's sealed tokens as plaintext.
func unsealContext(c *contexts.Context) (bool, error) {
	if c == nil || c.KeychainService == "" || c.Handle == "" {
		return false, nil
	}
	enc, err := tokenstore.Get(c.KeychainService, c.Handle)
	if errors.Is(err, tokenstore.ErrNotFound) || (err == nil && !isSealed(enc)) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read access token: %w", err)
	}
	b, expiresAt, err := openSealedSlot(enc, c.CoreURL, c.Handle,
		promptReason("turn off token protection", c.CoreURL, c.Handle))
	if err != nil {
		return false, err
	}
	// Refresh first, matching every other credential write.
	if b.Refresh != "" {
		if err := tokenstore.Set(tokenstore.RefreshService(c.KeychainService), c.Handle, b.Refresh); err != nil {
			return false, fmt.Errorf("store refresh token: %w", err)
		}
	}
	if err := tokenstore.Set(c.KeychainService, c.Handle, tokenstore.EncodeTokenWithExpiration(b.Access, expiresInFrom(expiresAt))); err != nil {
		return false, fmt.Errorf("store access token: %w", err)
	}
	return true, nil
}
