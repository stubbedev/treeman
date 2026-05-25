package cmd

// cliSink routes wt orchestrator status lines through the CLI's
// PrintOK / PrintWarn / PrintInfo helpers — which in turn delegate
// to the ui package, so behavior like the --print-path stdout
// hijack (ui.Out = os.Stderr) is preserved.
type cliSink struct{}

func (cliSink) OK(format string, args ...any)   { PrintOK(format, args...) }
func (cliSink) Warn(format string, args ...any) { PrintWarn(format, args...) }
func (cliSink) Info(format string, args ...any) { PrintInfo(format, args...) }
