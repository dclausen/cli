package cli

import "testing"

// Every provider's display and stored spellings must round-trip, or what
// `auth status` shows stops being what `grant add` accepts.
func TestProviderIdentity_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, provider, stored, display string
	}{
		{"github username", "github", "alice", "alice"},
		{"github username that looks minted", "github", "github-foo", "github-foo"},
		{"google minted handle", "google", "google-100164574874856813796", "100164574874856813796"},
		{"provider case is ignored", "Google", "google-1001", "1001"},
		{"unknown provider passes through", "gitlab", "gitlab-bob", "gitlab-bob"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id := identityFor(tt.provider)
			if got := id.displayHandle(tt.stored); got != tt.display {
				t.Errorf("displayHandle(%q) = %q, want %q", tt.stored, got, tt.display)
			}
			if got := id.storedHandle(tt.display); got != tt.stored {
				t.Errorf("storedHandle(%q) = %q, want %q", tt.display, got, tt.stored)
			}
		})
	}
}

// A value copied from --json carries the stored spelling; typing it back must
// not mint the prefix a second time.
func TestProviderIdentity_StoredFormIsAcceptedAsTyped(t *testing.T) {
	t.Parallel()
	for _, typed := range []string{"google-1001", "Google-1001"} {
		if got := identityFor(providerGoogle).storedHandle(typed); got != typed {
			t.Errorf("storedHandle(%q) = %q, want it unchanged", typed, got)
		}
	}
}

func TestDisplayGranteeName(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"google:google-1001", "google:1001"},
		{"github:alice", "github:alice"},
		{"github:github-foo", "github:github-foo"},
		{"acme", "acme"},                                             // org or team name
		{"01HZX0000000000000000000AB", "01HZX0000000000000000000AB"}, // ULID
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := displayGranteeName(tt.in); got != tt.want {
				t.Errorf("displayGranteeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
