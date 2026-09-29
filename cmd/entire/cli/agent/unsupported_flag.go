package agent

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// rejectedFlagPattern matches the unknown-option errors of the agent CLIs:
// clap's "unexpected argument '--x' found" (Copilot) and commander's
// "unknown option '--x'" (Claude). The capture stops at '=' so
// "--flag=value" reports "--flag".
var rejectedFlagPattern = regexp.MustCompile(`(?:unexpected argument|unknown option) '(-[^'=\s]+)`)

// RejectedFlag reports which of flags a CLI refused as unknown, judging by
// its error output. Only flags in the list count, so an unrelated
// unknown-option error is not mistaken for one of ours.
func RejectedFlag(output string, flags []string) (string, bool) {
	for _, m := range rejectedFlagPattern.FindAllStringSubmatch(output, -1) {
		if slices.Contains(flags, m[1]) {
			return m[1], true
		}
	}
	return "", false
}

// FlagNames returns the option names in args ("--x" from "--x" or
// "--x=value"), for passing to RejectedFlag.
func FlagNames(args []string) []string {
	var names []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			name, _, _ := strings.Cut(a, "=")
			names = append(names, name)
		}
	}
	return names
}

// UnsupportedFlagError reports that an agent CLI is too old for a flag Entire
// passes to keep text generation isolated. The flag cannot be dropped to make
// the call succeed, because it is what keeps untrusted transcript content
// from driving tools; the remedy is updating the CLI.
type UnsupportedFlagError struct {
	CLI  string
	Flag string
	Err  error
}

func (e *UnsupportedFlagError) Error() string {
	return fmt.Sprintf("%s does not support %s, which Entire needs to generate text with no tools available; update %s and retry: %v", e.CLI, e.Flag, e.CLI, e.Err)
}

func (e *UnsupportedFlagError) Unwrap() error { return e.Err }
