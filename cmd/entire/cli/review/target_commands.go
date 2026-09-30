package review

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
)

// targetAgentCommandDirs are the checkout directories Claude Code loads
// commands, skills, and subagents from. Reviewer isolation keeps the
// checkout's settings, hooks, and MCP servers out, but Claude has no flag
// that drops only project commands while keeping the user's own, and a
// command file can run shell lines (`!` blocks, gated by its own
// allowed-tools frontmatter) when invoked, including by a review profile's
// /skill that it shadows.
var targetAgentCommandDirs = []string{".claude/commands", ".claude/skills", ".claude/agents"}

// targetAgentCommandChanges lists files under targetAgentCommandDirs that the
// target checkout adds or changes relative to where it forked from the
// caller's HEAD, including uncommitted changes in a reused worktree. Measuring
// from the merge base means the caller's own edits to those files never count
// against a target that simply lacks them.
func targetAgentCommandChanges(ctx context.Context, callerWorktree, targetWorktree string) ([]string, error) {
	callerHead, err := gitIn(ctx, callerWorktree, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve caller HEAD: %w", err)
	}
	base, err := gitIn(ctx, targetWorktree, "merge-base", callerHead, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("find where the target forked from %s: %w", callerHead, err)
	}
	// diff-index compares the merge base's tree with the working tree without
	// refreshing the index, which a worktree-comparing `git diff` may rewrite.
	changed, err := gitIn(ctx, targetWorktree, append([]string{"diff-index", "--name-only", "--no-renames", base, "--"}, targetAgentCommandDirs...)...)
	if err != nil {
		return nil, fmt.Errorf("list the target's agent command changes: %w", err)
	}
	untracked, err := gitIn(ctx, targetWorktree, append([]string{"ls-files", "--others", "--exclude-standard", "--"}, targetAgentCommandDirs...)...)
	if err != nil {
		return nil, fmt.Errorf("list the target's untracked agent command files: %w", err)
	}
	var files []string
	for _, line := range strings.Split(changed+"\n"+untracked, "\n") {
		if line = strings.TrimSpace(line); line != "" && !slices.Contains(files, line) {
			files = append(files, line)
		}
	}
	slices.Sort(files)
	return files, nil
}

// targetAgentCommandsError explains why a target review stopped and how to
// proceed once the files have been looked at.
func targetAgentCommandsError(target string, files []string) error {
	return fmt.Errorf("%s adds or changes Claude Code commands, skills, or agents:\n  %s\n"+
		"A reviewer running in its worktree loads these, and a command can run shell commands when invoked. "+
		"Read them first; to review the branch anyway, rerun with --trust-target-commands",
		target, strings.Join(files, "\n  "))
}

func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// -C names the repository; a GIT_DIR/GIT_WORK_TREE inherited from the
	// caller's environment would otherwise take precedence over it.
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
