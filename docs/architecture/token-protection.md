# Secure Enclave token protection

Opt-in, macOS only. `entire auth protect` seals every saved login to a key in
the Mac's Secure Enclave. From then on each use of a login shows the macOS
Touch ID (or password) dialog naming the action, so an agent running
`git push` or `entire trail create` cannot spend the user's login silently.

## What is stored where

| Item | Location | Prompts |
| --- | --- | --- |
| Enclave key blob | `<config dir>/token-key.sekey`, mode 0600 | never |
| Access + refresh token bundle | the context's access slot in the keyring, as `se1:<base64>\|<expiry>` | on read |
| Refresh slot | cleared while protected | n/a |

The key is created with `kSecAttrIsPermanent=false` and exported as its
token object id (`toid`). That blob is wrapped by this Mac's enclave and
carries the access-control policy (`userPresence`) inside it, so any process
that loads it still hits the dialog. Nothing is added to the keychain's
data-protection class, which is what lets an unsigned `go build` binary use
the enclave: keychain-resident enclave keys need a `keychain-access-groups`
entitlement, and that entitlement is restricted on macOS. A bare CLI binary
cannot carry the provisioning profile it needs, so ad-hoc and Developer ID
signed binaries carrying it are killed at launch. The blob route sidesteps
that entirely.

Sealing is ECIES to the public half (`SecKeyCreateEncryptedData`) and never
prompts, so login and refresh rotation stay silent. Unsealing goes through
`SecKeyCreateDecryptedData` on the private half and prompts. All calls are
bound at runtime with purego; release builds stay `CGO_ENABLED=0`.

## Code map

- `internal/entireclient/tokenstore/senclave`: `Generate`, `Load`, `Seal`,
  `Unseal(ciphertext, reason)`, `PublicKey`. Darwin only; other platforms
  return `ErrUnsupported`.
- `cmd/entire/cli/auth/protected.go`: bundle format, `EnableProtection`,
  `DisableProtection`, `TokensProtected`, `WithPromptAction`,
  `PromptDeclined`, and the `protection` sealer source tests swap with
  `SetSealerForTesting`.
- `cmd/entire/cli/auth/refresh.go`: `contextTokenStore` loads and saves the
  sealed bundle for auth-go's token manager. One load is one prompt because
  both tokens live in one bundle.
- `cmd/entire/cli/auth/contexts.go`: `RecordLoginContext` writes sealed when
  protected; `LoginTokenForContext` unseals.
- `cmd/entire/cli/auth_protect.go`: the `auth protect` and `auth unprotect`
  commands, registered as experimental.
- `cmd/git-remote-entire/main.go`: attaches "git push to <host>" or
  "git fetch from <host>" to the request context so the dialog names it,
  and ignores `ENTIRE_TLS_SKIP_VERIFY` while protected.

## Dialog text

macOS renders `"<binary>" is trying to <reason>.` The reason is
`<action> with Entire login <handle>@<login server host>`, for example
`git push to us.entire.io with Entire login toothbrush@us.auth.entire.io`.
CLI commands derive the action from their non-flag words (`entire trail
list`). The binary name and the dialog itself come from macOS and cannot be
forged by the caller.

## Prompt count

| Operation | Prompts |
| --- | --- |
| `entire login`, token refresh | 0 |
| `entire <command>` touching the API | 1 |
| `git fetch` / `git pull` | 1 |
| `git push` with checkpoint sync on | 2 (the pre-push hook's nested push is a second helper process) |
| agent hooks (commit, session) | 0 (they never read tokens) |

## Threat model

Protected against, with user-level code execution on the Mac:

- Reading tokens from the keychain or `security find-generic-password`: the
  slot holds ciphertext.
- Running `git push` or `entire` commands: the dialog names the action.
- Editing `contexts.json` to point a context at an attacker-controlled login
  server: the bundle carries its issuer and handle and is refused on
  mismatch (`ErrBundleMismatch`).
- Intercepting TLS via `ENTIRE_TLS_SKIP_VERIFY` or `--insecure-http-auth`:
  both are ignored while protected. Loopback `http://` cores stay allowed.
- Turning protection off: `auth unprotect` has to unseal first, which prompts.

Not protected against:

- A process that loads the key blob itself and shows its own dialog text.
  The blob is readable by the user, and the enclave enforces presence, not
  which binary asked. Binding to our signed binaries needs the keychain
  route, which needs an app bundle with a provisioning profile.
- Replay of a captured login JWT until it expires (one hour). Making the
  bearer useless without a fresh enclave signature needs DPoP (RFC 9449) on
  the login server and data plane: bind tokens to this key, sign a proof per
  request. The key and plumbing here are the client half of that.
- Root or kernel compromise.

## Platform notes

- No GUI session (SSH, headless): unseal fails with `ErrNoInteraction`,
  mapped to `auth.ErrPromptDeclined`. Commands fail closed and say so.
  `ENTIRE_TOKEN` still bypasses by design; it is a separate credential.
- The entiredb CLI shares the keyring prefix and will read ciphertext from
  a protected slot. Port `senclave` there or document the breakage before
  shipping to users who run both.
- Dev builds: plain `go build` works. No signing or entitlements needed.

## Manual verification

```
go build -o entire ./cmd/entire && go build -o git-remote-entire ./cmd/git-remote-entire
./entire auth protect
./entire auth status            # expect one Touch ID dialog, "protection" row
git -c core.sshCommand= fetch   # in a repo with an entire:// remote; one dialog
./entire auth unprotect
```
