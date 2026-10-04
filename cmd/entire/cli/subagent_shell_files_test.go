package cli

import (
	"slices"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/assert"
)

func TestShellCommandMayWrite(t *testing.T) {
	t.Parallel()
	for cmd, want := range map[string]bool{
		"printf x > notes/result.md":         true,
		"sed -i '' 's/a/b/' main.go":         true,
		"cat > a.go <<'EOF'\npackage a\nEOF": true,
		"git checkout -- main.go":            true,
		"mise run fmt":                       true,
		"go test ./... 2>&1 | tail -3":       false,
		"grep -rn foo . 2>/dev/null | head":  false,
		"git log --oneline -5":               false,
		"sed -n '1,20p' main.go":             false,
		"ls >/dev/null && echo ok":           false,
		"python3 scripts/gen.py":             true,
	} {
		assert.Equal(t, want, shellCommandMayWrite(cmd), cmd)
	}
}

func TestMatchShellWrittenFiles(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	at := func(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }
	call := func(start, end float64, cmd string) agent.ToolCallWindow {
		return agent.ToolCallWindow{Start: at(start), End: at(end), Command: cmd}
	}
	own := []agent.ToolCallWindow{
		call(10, 12, "printf x > notes/result.md"),
		call(20, 21, "go test ./..."), // read-only: not a candidate window
		call(30, 32, "python3 gen.py"),
	}
	others := []ownedToolWindows{{owner: "parent", windows: []agent.ToolCallWindow{
		call(30.5, 31.5, "make build"),
		{Start: at(40), End: at(41), FilePath: "edited.go"},
	}}}
	mtimes := map[string]time.Time{
		"notes/result.md": at(11),   // only our write window
		"test.out":        at(20.5), // only our read-only call: not ours
		"gen.out":         at(31),   // overlaps the parent's make, neither names it
		"human.md":        at(100),  // no agent call at all
		"edited.go":       at(40.5), // the parent's edit
	}
	matched, ambiguous := matchShellWrittenFiles(own, others, mtimes, at(200))
	slices.Sort(matched)
	assert.Equal(t, []string{"notes/result.md"}, matched)
	assert.Equal(t, []string{"gen.out"}, ambiguous)

	t.Run("path mention breaks a tie", func(t *testing.T) {
		t.Parallel()
		own := []agent.ToolCallWindow{call(10, 12, "printf x > out/ours.md")}
		others := []ownedToolWindows{{owner: "sibling", windows: []agent.ToolCallWindow{call(10, 12, "printf y > out/theirs.md")}}}
		mtimes := map[string]time.Time{"out/ours.md": at(11), "out/theirs.md": at(11)}
		matched, ambiguous := matchShellWrittenFiles(own, others, mtimes, at(200))
		assert.Equal(t, []string{"out/ours.md"}, matched)
		assert.Empty(t, ambiguous, "a file only the other agent names is theirs")
	})

	t.Run("a call without a result runs until now", func(t *testing.T) {
		t.Parallel()
		own := []agent.ToolCallWindow{{Start: at(10), Command: "printf x > live.md"}}
		matched, _ := matchShellWrittenFiles(own, nil, map[string]time.Time{"live.md": at(50)}, at(60))
		assert.Equal(t, []string{"live.md"}, matched)
	})
}
