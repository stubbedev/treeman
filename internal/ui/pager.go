package ui

import (
	"io"
	"os"
	"os/exec"
	"strings"
)

// Pager wraps an external pager process (typically `less`) that
// receives ui.Out + ui.Err while it's active. The intended pattern:
//
//	p := ui.NewPager()
//	defer p.Close()
//	if err := p.Start(); err != nil { /* fall through to plain */ }
//	// ... print as usual ...
//
// NewPager returns nil when paging is disabled (NO_COLOR, dumb term,
// non-TTY stdout, $TREEMAN_NO_PAGER set, or $PAGER explicitly empty),
// so callers can use the value directly with a nil-safe Close.
//
// The pager binary is `$PAGER` when set, else `less` with the
// arguments that make sense for command output: `-F` (quit if one
// screen), `-R` (interpret ANSI colors raw), `-X` (no alt-screen
// flicker on exit). Falls back to printing inline when neither
// exists on $PATH.
type Pager struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	prevOut io.Writer
	prevErr io.Writer
}

// NewPager constructs a Pager if interactive output makes sense. It
// does not start the process — call Start. Returns nil when the
// environment indicates the caller wants plain output (CI, pipe,
// explicit opt-out).
func NewPager() *Pager {
	if !IsTTY() {
		return nil
	}
	if os.Getenv("TREEMAN_NO_PAGER") != "" {
		return nil
	}
	if v, ok := os.LookupEnv("PAGER"); ok && v == "" {
		return nil
	}
	return &Pager{}
}

// Start launches the pager subprocess and retargets ui.Out / ui.Err
// to its stdin. If the pager binary can't be located or started,
// Start leaves the Out/Err writers untouched and returns an error
// so the caller can fall through to inline rendering.
func (p *Pager) Start() error {
	if p == nil {
		return nil
	}
	bin, args := pagerCommand()
	if bin == "" {
		return os.ErrNotExist
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
	p.cmd = cmd
	p.stdin = stdin
	p.prevOut, p.prevErr = Out, Err
	Out = stdin
	Err = stdin
	return nil
}

// Close flushes pending output, closes the pager's stdin, restores
// the previous Out/Err writers, and waits for the pager process to
// exit so we don't return control to the shell while less is still
// repainting.
func (p *Pager) Close() {
	if p == nil || p.cmd == nil {
		return
	}
	_ = p.stdin.Close()
	_ = p.cmd.Wait()
	Out, Err = p.prevOut, p.prevErr
	p.cmd = nil
}

// pagerCommand picks the pager binary + args.
//
//   - $PAGER takes precedence (split on whitespace, first token is
//     the binary). This is the POSIX-standard knob and we honour it
//     verbatim so users who set `PAGER="less -SR"` get exactly that.
//   - Else `less -FRX` if `less` is on $PATH. `-F` lets short output
//     pass through without entering the alt-screen; `-R` keeps our
//     ANSI colors readable; `-X` skips the init/deinit sequence so
//     the terminal isn't cleared on exit.
//   - Else empty — caller should fall through to inline rendering.
func pagerCommand() (string, []string) {
	if v := os.Getenv("PAGER"); v != "" {
		parts := strings.Fields(v)
		if len(parts) == 0 {
			return "", nil
		}
		if _, err := exec.LookPath(parts[0]); err != nil {
			return "", nil
		}
		return parts[0], parts[1:]
	}
	if _, err := exec.LookPath("less"); err == nil {
		return "less", []string{"-FRX"}
	}
	return "", nil
}
