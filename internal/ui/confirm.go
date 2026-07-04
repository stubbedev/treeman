package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
)

// In is the input stream for interactive prompts. Overridable for
// tests; defaults to os.Stdin.
var In io.Reader = os.Stdin

// Confirm asks the user `<question> [y/N]: ` and reports whether
// the operation should proceed.
//
// Returns true (proceed) without prompting when stdin is not a TTY
// — pipes, CI, and shell scripts are treated as having opted into
// destructive behaviour by being non-interactive. Callers that
// want an explicit override expose a `--yes/-y` flag and skip the
// call entirely when set, so script users never see this path.
//
// The prompt is written to stderr so stdout (often consumed by
// shell integrations: `cd "$(treeman worktree go foo)"`) stays
// clean.
func Confirm(question string) bool {
	if !isatty.IsTerminal(os.Stdin.Fd()) && !isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		return true
	}
	_, _ = fmt.Fprint(Err, Yellow(SymWarn)+" "+question+" [y/N]: ")
	line, err := bufio.NewReader(In).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// ConfirmYes is Confirm with a YES default: Enter (or anything that
// isn't n/N) proceeds, only an explicit no cancels. This is the
// warn-but-don't-obstruct contract of the old zsh _confirm_destructive
// prompts (push guard, stash clear, wipe) — the user is being warned,
// not gatekept. Same non-TTY auto-proceed as Confirm.
func ConfirmYes(question string) bool {
	if !isatty.IsTerminal(os.Stdin.Fd()) && !isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		return true
	}
	_, _ = fmt.Fprint(Err, Yellow(SymWarn)+" "+question+" ["+Green("Y")+"/"+Red("n")+"]: ")
	line, err := bufio.NewReader(In).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "n", "no":
		return false
	}
	return true
}
