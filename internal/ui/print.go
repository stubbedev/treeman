package ui

import (
	"fmt"
)

// Success prints `<sym> <msg>` in green to stdout.
func Success(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fprintln(Out, Green(SymSuccess)+" "+msg)
}

// Warn prints `<sym> <msg>` in yellow to stderr.
func Warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fprintln(Err, Yellow(SymWarn)+" "+msg)
}

// Error prints `<sym> <msg>` in red to stderr.
func Error(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fprintln(Err, Red(SymError)+" "+msg)
}

// Info prints `<sym> <msg>` in blue to stdout.
func Info(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fprintln(Out, Blue(SymInfo)+" "+msg)
}

// Hint prints a dim follow-up line, usually after a Warn/Error to
// suggest a remediation. Prefixed with two spaces so it visually
// attaches to the message above.
func Hint(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fprintln(Err, "  "+Dim(msg))
}

// Plain prints msg to stdout with no styling. Useful for output that
// callers may pipe (e.g. `treeman wt switch` prints a path).
func Plain(format string, args ...any) {
	fprintln(Out, fmt.Sprintf(format, args...))
}

// Steps tracks progress through a multi-step CLI operation. Each
// Next prints `[i/N] <msg>` with a dim prefix; Done flips the line
// to a green checkmark suffix.
type Steps struct {
	total, idx int
}

// NewSteps starts a step tracker. Pass the expected total; if you
// don't know the total ahead of time, pass 0 and the prefix becomes
// `[i]` instead of `[i/N]`.
func NewSteps(total int) *Steps { return &Steps{total: total} }

func (s *Steps) prefix() string {
	if s.total <= 0 {
		return Dim(fmt.Sprintf("[%d]", s.idx))
	}
	return Dim(fmt.Sprintf("[%d/%d]", s.idx, s.total))
}

// Start advances the counter and prints a step header.
func (s *Steps) Start(format string, args ...any) {
	s.idx++
	fprintln(Out, s.prefix()+" "+Cyan(SymArrow)+" "+fmt.Sprintf(format, args...))
}

// Done marks the most-recent step finished.
func (s *Steps) Done(format string, args ...any) {
	fprintln(Out, s.prefix()+" "+Green(SymSuccess)+" "+fmt.Sprintf(format, args...))
}

// Skip prints a dim "skipped" line for the most-recent step.
func (s *Steps) Skip(format string, args ...any) {
	fprintln(Out, s.prefix()+" "+Dim(SymDot)+" "+Dim(fmt.Sprintf(format, args...)))
}
