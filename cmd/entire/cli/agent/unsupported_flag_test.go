package agent

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRejectedFlag(t *testing.T) {
	t.Parallel()
	ours := []string{"--tools", "--available-tools", "-s"}
	tests := []struct {
		name   string
		output string
		want   string
		ok     bool
	}{
		{"clap (copilot)", "error: unexpected argument '--available-tools' found\n\nUsage: copilot [OPTIONS]", "--available-tools", true},
		{"clap with value", "error: unexpected argument '--available-tools=x' found", "--available-tools", true},
		{"commander (claude)", "error: unknown option '--tools'", "--tools", true},
		{"short flag", "error: unexpected argument '-s' found", "-s", true},
		{"a flag that is not ours", "error: unknown option '--frobnicate'", "", false},
		{"no rejection", "Error: not logged in", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := RejectedFlag(tt.output, ours)
			if got != tt.want || ok != tt.ok {
				t.Errorf("RejectedFlag = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestFlagNames(t *testing.T) {
	t.Parallel()
	got := FlagNames([]string{"--deny-tool", "shell", "--available-tools=x", "-s", "value"})
	if want := []string{"--deny-tool", "--available-tools", "-s"}; !slices.Equal(got, want) {
		t.Errorf("FlagNames = %q, want %q", got, want)
	}
}

func TestUnsupportedFlagError_NamesTheRemedyAndUnwraps(t *testing.T) {
	t.Parallel()
	cause := errors.New("exit status 1")
	err := &UnsupportedFlagError{CLI: "copilot", Flag: "--available-tools", Err: cause}
	for _, want := range []string{"copilot", "--available-tools", "update copilot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to mention %q", err.Error(), want)
		}
	}
	if !errors.Is(err, cause) {
		t.Error("UnsupportedFlagError does not unwrap to its cause")
	}
}
