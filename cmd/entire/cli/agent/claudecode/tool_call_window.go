package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// ToolBash is Claude Code's shell tool.
const ToolBash = "Bash"

// Compile-time check.
var _ agent.ToolCallWindowExtractor = (*ClaudeCodeAgent)(nil)

// timedLine is a transcript line with the timestamp Claude Code stamps on it.
type timedLine struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
}

// ExtractToolCallWindows returns the Bash and file-edit calls issued after
// line startOffset, each spanning from the assistant line that issued it to
// the user line carrying its result.
func (c *ClaudeCodeAgent) ExtractToolCallWindows(_ context.Context, path string, startOffset int) ([]agent.ToolCallWindow, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path) //nolint:gosec // Path comes from Claude Code transcript location
	if err != nil {
		return nil, fmt.Errorf("failed to open transcript file: %w", err)
	}
	defer file.Close()

	var windows []agent.ToolCallWindow
	open := make(map[string]int) // tool_use id → index in windows
	reader := bufio.NewReader(file)
	for lineNum := 1; ; lineNum++ {
		data, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("failed to read transcript: %w", readErr)
		}
		if len(data) > 0 && lineNum > startOffset {
			collectToolWindows(data, lineNum, &windows, open)
		}
		if readErr != nil {
			break
		}
	}
	return windows, nil
}

func collectToolWindows(data []byte, lineNum int, windows *[]agent.ToolCallWindow, open map[string]int) {
	var line timedLine
	if json.Unmarshal(data, &line) != nil || line.Timestamp.IsZero() {
		return
	}
	switch line.Type {
	case envelopeTypeAssistant:
		var msg assistantMessage
		if json.Unmarshal(line.Message, &msg) != nil {
			return
		}
		for _, block := range msg.Content {
			if block.Type != transcript.ContentTypeToolUse || (block.Name != ToolBash && !slices.Contains(FileModificationTools, block.Name)) {
				continue
			}
			var input toolInput
			if json.Unmarshal(block.Input, &input) != nil {
				continue
			}
			window := agent.ToolCallWindow{Start: line.Timestamp, Line: lineNum}
			if block.Name == ToolBash {
				window.Command = input.Command
			} else {
				window.FilePath = input.FilePath
				if window.FilePath == "" {
					window.FilePath = input.NotebookPath
				}
			}
			open[block.ID] = len(*windows)
			*windows = append(*windows, window)
		}
	case transcript.TypeUser:
		var msg struct {
			Content []toolResultBlock `json:"content"`
		}
		// A plain-text user message has string content; it carries no results.
		if json.Unmarshal(line.Message, &msg) != nil {
			return
		}
		for _, block := range msg.Content {
			if block.Type != "tool_result" {
				continue
			}
			if i, ok := open[block.ToolUseID]; ok {
				(*windows)[i].End = line.Timestamp
				delete(open, block.ToolUseID)
			}
		}
	}
}
