package cli

import (
	"context"
	"log/slog"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// shellWindowSlack widens each tool call window for timestamp precision: the
// transcript stamps a call when it is issued and when its result is recorded,
// and the file's write lands somewhere in between.
const shellWindowSlack = 500 * time.Millisecond

// shellWriteHint matches shell commands that may write files: redirects,
// in-place editors, file operations, worktree-changing git commands, heredocs,
// interpreters and scripts, formatters, generators and installers. Matching too
// much only widens the candidate windows; a missed write leaves the file to
// the user, as before shell writes were matched at all.
var shellWriteHint = regexp.MustCompile(`>|\btee\b|\b(sed|perl)\s+-[a-zA-Z]*i|\b(cp|mv|rm|touch|mkdir|ln|patch|install|rsync|unzip|tar)\s|\bgit\s+(-C\s+\S+\s+)?(checkout|apply|stash|reset|restore|switch|merge|rebase|pull|cherry-pick|clean|am|mv|rm)\b|<<|open\(|write_text|writeFile|WriteFile|\b(gofmt|goimports|prettier|eslint|ruff|black|npm|pnpm|yarn|bun|pip|make|mise|go\s+(fmt|generate|get|mod|run))\b|\b(python3?|node|ruby|deno|bash|sh|zsh)\s`)

// shellRedirectToNowhere strips redirects that write no file (descriptor
// duplication and /dev/null) before shellWriteHint looks for one.
var shellRedirectToNowhere = regexp.MustCompile(`[0-9]*>>?\s*(&[0-9-]|/dev/null)`)

func shellCommandMayWrite(command string) bool {
	return shellWriteHint.MatchString(shellRedirectToNowhere.ReplaceAllString(command, ""))
}

// ownedToolWindows is one agent's write-capable tool calls, with file-edit
// paths already repo-relative.
type ownedToolWindows struct {
	owner   string
	windows []agent.ToolCallWindow
}

// matchShellWrittenFiles decides which changed files a subagent's shell calls
// wrote, from each file's modification time. A file is the subagent's when
// its mtime falls inside one of the subagent's write-looking shell calls and
// no other agent's write-capable call. When another agent's call overlaps
// too, the file goes to whichever side's overlapping calls name it; if both
// or neither do, it is ambiguous: certainly agent work, but not provably this
// subagent's. now closes calls that have no result yet.
func matchShellWrittenFiles(own []agent.ToolCallWindow, others []ownedToolWindows, mtimes map[string]time.Time, now time.Time) (matched, ambiguous []string) {
	var ownShell []agent.ToolCallWindow
	for _, w := range own {
		if w.Command != "" && shellCommandMayWrite(w.Command) {
			ownShell = append(ownShell, w)
		}
	}
	if len(ownShell) == 0 {
		return nil, nil
	}
	for file, mtime := range mtimes {
		ownHits := windowsAt(ownShell, mtime, now)
		if len(ownHits) == 0 {
			continue
		}
		var otherHits []agent.ToolCallWindow
		for _, o := range others {
			for _, w := range windowsAt(o.windows, mtime, now) {
				switch {
				case w.Command != "" && shellCommandMayWrite(w.Command):
					otherHits = append(otherHits, w)
				case w.FilePath == file:
					otherHits = append(otherHits, w)
				}
			}
		}
		if len(otherHits) == 0 {
			matched = append(matched, file)
			continue
		}
		ownNames, otherNames := windowsName(ownHits, file), windowsName(otherHits, file)
		switch {
		case ownNames && !otherNames:
			matched = append(matched, file)
		case !ownNames && otherNames:
			// Another agent's call names it; not this subagent's.
		default:
			ambiguous = append(ambiguous, file)
		}
	}
	return matched, ambiguous
}

func windowsAt(windows []agent.ToolCallWindow, at, now time.Time) []agent.ToolCallWindow {
	var hits []agent.ToolCallWindow
	for _, w := range windows {
		end := w.End
		if end.IsZero() {
			end = now
		}
		if !at.Before(w.Start.Add(-shellWindowSlack)) && !at.After(end.Add(shellWindowSlack)) {
			hits = append(hits, w)
		}
	}
	return hits
}

// windowsName reports whether any of the calls names file: an edit of it, or
// a shell command containing its base name.
func windowsName(windows []agent.ToolCallWindow, file string) bool {
	base := path.Base(file)
	for _, w := range windows {
		if w.FilePath == file || (w.Command != "" && containsWord(w.Command, base)) {
			return true
		}
	}
	return false
}

// containsWord reports whether word occurs in s delimited by characters that
// cannot be part of a file name token.
func containsWord(s, word string) bool {
	isToken := func(r byte) bool {
		return r == '_' || r == '.' || r == '-' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
	}
	for i := 0; i+len(word) <= len(s); {
		j := strings.Index(s[i:], word)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(word)
		if (start == 0 || !isToken(s[start-1])) && (end == len(s) || !isToken(s[end])) {
			return true
		}
		i = start + 1
	}
	return false
}

// shellWrittenSubagentFiles finds the worktree changes a subagent's shell
// calls made, which its transcript does not name (see matchShellWrittenFiles).
// The subagent's calls are read from line from of transcriptPath; known are
// the files the transcript analyzer already attributed to it. The other
// agents are the parent and the session's other subagents.
//
// openFrom is the transcript line that issued the earliest write-looking
// shell call still running (0 when none): a later scan must start before it,
// or files the command writes after this scan are never matched to it.
func shellWrittenSubagentFiles(ctx context.Context, ag agent.Agent, state *strategy.SessionState, rec session.TaskRecord, transcriptPath, repoRoot string, from int, known []string) (matched, ambiguous []string, openFrom int) {
	extractor, ok := agent.AsToolCallWindowExtractor(ag)
	if !ok || transcriptPath == "" || state == nil {
		return nil, nil, 0
	}
	own, err := extractor.ExtractToolCallWindows(ctx, transcriptPath, from)
	if err != nil {
		logging.Warn(ctx, "failed to read subagent tool calls for shell-write matching",
			slog.String("session_id", state.SessionID),
			slog.String("tool_use_id", rec.ToolUseID),
			slog.String("error", err.Error()))
		return nil, nil, 0
	}
	for _, w := range own {
		if w.End.IsZero() && shellCommandMayWrite(w.Command) && (openFrom == 0 || w.Line < openFrom) {
			openFrom = w.Line
		}
	}
	if !slices.ContainsFunc(own, func(w agent.ToolCallWindow) bool { return shellCommandMayWrite(w.Command) }) {
		return nil, nil, openFrom
	}
	now := time.Now()
	mtimes := changedFileMtimes(ctx, repoRoot, known, own, now)
	if len(mtimes) == 0 {
		return nil, nil, openFrom
	}
	matched, ambiguous = matchShellWrittenFiles(own, otherAgentWindows(ctx, extractor, state, rec, repoRoot), mtimes, now)
	return matched, ambiguous, openFrom
}

// changedFileMtimes returns the modification times of changed worktree files
// that are not in known and were modified while one of the calls ran.
func changedFileMtimes(ctx context.Context, repoRoot string, known []string, calls []agent.ToolCallWindow, now time.Time) map[string]time.Time {
	changes, err := DetectFileChanges(ctx, nil)
	if err != nil || changes == nil {
		return nil
	}
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return nil
	}
	mtimes := make(map[string]time.Time)
	for _, file := range FilterAndNormalizePaths(append(changes.New, changes.Modified...), repoRoot) {
		if slices.Contains(known, file) {
			continue
		}
		name, nameErr := worktreedir.Name(repoRoot, file)
		if nameErr != nil {
			continue
		}
		info, statErr := osroot.LstatNoSymlinks(root, name)
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		if len(windowsAt(calls, info.ModTime(), now)) > 0 {
			mtimes[file] = info.ModTime()
		}
	}
	return mtimes
}

// otherAgentWindows reads the write-capable tool calls of every agent in the
// session other than rec's subagent: the parent and the other subagents.
func otherAgentWindows(ctx context.Context, extractor agent.ToolCallWindowExtractor, state *strategy.SessionState, rec session.TaskRecord, repoRoot string) []ownedToolWindows {
	read := func(owner, transcriptPath string) (ownedToolWindows, bool) {
		windows, err := extractor.ExtractToolCallWindows(ctx, transcriptPath, 0)
		if err != nil || len(windows) == 0 {
			return ownedToolWindows{}, false
		}
		for i := range windows {
			if windows[i].FilePath != "" {
				if rel := FilterAndNormalizePaths([]string{windows[i].FilePath}, repoRoot); len(rel) == 1 {
					windows[i].FilePath = rel[0]
				}
			}
		}
		return ownedToolWindows{owner: owner, windows: windows}, true
	}
	var others []ownedToolWindows
	if state.TranscriptPath != "" {
		if o, ok := read("parent", state.TranscriptPath); ok {
			others = append(others, o)
		}
	}
	for _, other := range state.TaskRecords {
		if other.ToolUseID == rec.ToolUseID || other.TranscriptUnavailable {
			continue
		}
		transcriptPath := other.DeclaredTranscriptPath
		if transcriptPath == "" && state.TranscriptPath != "" {
			transcriptPath = ResolveAgentTranscriptPath(filepath.Dir(state.TranscriptPath), state.SessionID, other.AgentID)
		}
		if transcriptPath == "" {
			continue
		}
		if o, ok := read(other.ToolUseID, transcriptPath); ok {
			others = append(others, o)
		}
	}
	return others
}
