package patcher

// Driver names — the `format:` values a Patch can declare, and the
// values detectDriver returns. Single source of truth so the dispatch
// switches in apply.go / filter.go and the auto-detect can't drift.
const (
	DriverDotenv     = "dotenv"
	DriverPhpunit    = "phpunit"
	DriverPhpunitEnv = "phpunit_env" // accepted alias for DriverPhpunit
	DriverYAML       = "yaml"
	DriverJSON       = "json"
	DriverTOML       = "toml"
	DriverINI        = "ini"
)
