package codex

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// An older codex rejects a feature name it has never had. That name is
// dropped and the call retried with every other feature still disabled.
func TestGenerateText_RetriesWithoutAFeatureCodexDoesNotKnow(t *testing.T) {
	t.Parallel()
	var calls [][]string
	ag := &CodexAgent{CommandRunner: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		calls = append(calls, slices.Clone(args))
		if slices.Contains(args, "code_mode_host") {
			return exec.CommandContext(ctx, "sh", "-c", "echo 'Error: Unknown feature flag: code_mode_host' >&2; exit 1")
		}
		return exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null; printf summary")
	}}

	got, err := ag.GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if got != "summary" {
		t.Fatalf("GenerateText = %q, want %q", got, "summary")
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (one rejected, one retry)", len(calls))
	}
	retry := calls[1]
	if slices.Contains(retry, "code_mode_host") {
		t.Errorf("retry still disables the unknown feature: %v", retry)
	}
	for _, f := range generateTextDisabledFeatures {
		if f == "code_mode_host" {
			continue
		}
		if i := slices.Index(retry, f); i < 1 || retry[i-1] != "--disable" {
			t.Errorf("retry dropped --disable %s: %v", f, retry)
		}
	}
}

// Any other failure is returned, not retried.
func TestGenerateText_DoesNotRetryOtherFailures(t *testing.T) {
	t.Parallel()
	calls := 0
	ag := &CodexAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		calls++
		return exec.CommandContext(ctx, "sh", "-c", "echo 'Error: Unknown feature flag: some_feature_we_do_not_pass' >&2; exit 1")
	}}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	if err == nil || !strings.Contains(err.Error(), "codex text generation failed") {
		t.Fatalf("err = %v, want a codex text generation failure", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}
