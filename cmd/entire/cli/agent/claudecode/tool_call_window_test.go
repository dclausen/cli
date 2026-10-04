package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractToolCallWindows(t *testing.T) {
	t.Parallel()

	transcript := `{"type":"user","timestamp":"2026-10-03T20:00:00.000Z","message":{"role":"user","content":"go"}}
{"type":"assistant","timestamp":"2026-10-03T20:00:01.250Z","message":{"content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"printf x > out.md"}},{"type":"tool_use","id":"toolu_read","name":"Read","input":{"file_path":"/repo/a.go"}}]}}
{"type":"user","timestamp":"2026-10-03T20:00:03.500Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_bash","content":""},{"type":"tool_result","tool_use_id":"toolu_read","content":"x"}]}}
{"type":"assistant","timestamp":"2026-10-03T20:00:04.000Z","message":{"content":[{"type":"tool_use","id":"toolu_edit","name":"Edit","input":{"file_path":"/repo/b.go","old_string":"a","new_string":"b"}}]}}
{"type":"user","timestamp":"2026-10-03T20:00:04.300Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_edit","content":"ok"}]}}
{"type":"assistant","timestamp":"2026-10-03T20:00:05.000Z","message":{"content":[{"type":"tool_use","id":"toolu_running","name":"Bash","input":{"command":"sleep 100"}}]}}
`
	path := filepath.Join(t.TempDir(), "agent-x.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(transcript), 0o600))
	at := func(s string) time.Time {
		parsed, err := time.Parse(time.RFC3339Nano, s)
		require.NoError(t, err)
		return parsed
	}

	windows, err := (&ClaudeCodeAgent{}).ExtractToolCallWindows(t.Context(), path, 0)
	require.NoError(t, err)
	require.Len(t, windows, 3, "Read is not a write-capable call")
	assert.Equal(t, "printf x > out.md", windows[0].Command)
	assert.Equal(t, at("2026-10-03T20:00:01.250Z"), windows[0].Start)
	assert.Equal(t, at("2026-10-03T20:00:03.500Z"), windows[0].End)
	assert.Equal(t, "/repo/b.go", windows[1].FilePath)
	assert.Equal(t, at("2026-10-03T20:00:04.300Z"), windows[1].End)
	assert.True(t, windows[2].End.IsZero(), "a call without a result is still running")
	assert.Equal(t, []int{2, 4, 6}, []int{windows[0].Line, windows[1].Line, windows[2].Line}, "each call records the line that issued it")

	fromOffset, err := (&ClaudeCodeAgent{}).ExtractToolCallWindows(t.Context(), path, 3)
	require.NoError(t, err)
	require.Len(t, fromOffset, 2, "calls issued at or before the offset line are skipped")
	assert.Equal(t, "/repo/b.go", fromOffset[0].FilePath)
}
