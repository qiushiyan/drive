package turn

import (
	"regexp"
	"strings"
)

// InputKind is what a session's input box holds.
type InputKind string

const (
	InputEmpty      InputKind = "empty"
	InputSuggestion InputKind = "suggestion" // Claude Code's dimmed guess at the next prompt; typing replaces it
	InputTyped      InputKind = "typed"      // text someone typed and did not send
	InputUnknown    InputKind = "unknown"    // no prompt line on screen
	InputPrompt     InputKind = "prompt"     // a prompt's options, such as "❯ 1. Yes", where the box would be
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)
var leadingSGR = regexp.MustCompile(`^(?:\x1b\[[0-9;]*m)*`)

// VimInsert says whether a screen's status line shows Claude Code's vim
// editor mode in INSERT mode. NORMAL mode shows nothing there.
func VimInsert(vt string) bool {
	return strings.Contains(sgr.ReplaceAllString(vt, ""), "-- INSERT --")
}

// InputBox reads the input box from a screen captured with its terminal
// sequences (`rex capture --format vt`): the last line holding the ❯ prompt.
// Claude Code draws its suggestion dim (SGR 2) and typed text plain.
func InputBox(vt string) (InputKind, string) {
	line := ""
	found := false
	for row := range strings.SplitSeq(vt, "\n") {
		if strings.Contains(row, "❯") {
			line, found = row, true
		}
	}
	if !found {
		return InputUnknown, ""
	}
	after := strings.SplitN(line, "❯", 2)[1]
	after = strings.TrimRight(strings.TrimLeft(after, "  "), "\r")
	plain := strings.TrimSpace(sgr.ReplaceAllString(after, ""))
	if plain == "" || strings.Trim(plain, "─") == "" {
		return InputEmpty, ""
	}
	if strings.Contains(leadingSGR.FindString(after), "\x1b[2m") {
		return InputSuggestion, plain
	}
	return InputTyped, plain
}
