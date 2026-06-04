// Package initgen renders a `.treeman.yaml` scaffold tailored to
// what a given repo looks like on disk.
//
// Detection drives what gets emitted; the runtime never re-detects.
// Every directive treeman acts on at runtime lives in the YAML this
// package writes. Shared by `treeman init` (CLI) and the MCP
// `init_repo` tool so neither has to shell out.
package initgen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/schema"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// WriteYAML scaffolds a `.treeman.yaml` under cwd. Returns the
// absolute target path, whether a new file was created (false when
// the file already existed and force was true), and the body bytes.
func WriteYAML(cwd string, force bool) (path string, created bool, body string, err error) {
	target := filepath.Join(cwd, ".treeman.yaml")
	_, statErr := os.Stat(target)
	exists := statErr == nil
	if exists && !force {
		return target, false, "", fmt.Errorf("%s already exists (pass force to overwrite)", target)
	}
	body = RenderTemplate(cwd)
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		return target, false, "", err
	}
	return target, !exists, body, nil
}

// WriteGlobalYAML scaffolds the user-global `config.yaml` at
// config.GlobalConfigPath(). Returns the target path, whether a new
// file was created, and the body. Refuses to clobber an existing file
// unless force is set. The parent dir is created if missing.
func WriteGlobalYAML(force bool) (path string, created bool, body string, err error) {
	target, ok := config.GlobalConfigPath()
	if !ok {
		return "", false, "", errors.New("cannot resolve global config path (no home dir)")
	}
	_, statErr := os.Stat(target)
	exists := statErr == nil
	if exists && !force {
		return target, false, "", fmt.Errorf("%s already exists (pass force to overwrite)", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return target, false, "", err
	}
	body = RenderGlobalTemplate()
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		return target, false, "", err
	}
	return target, !exists, body, nil
}

// RenderGlobalTemplate returns a commented `~/.config/treeman/config.yaml`
// scaffold covering only the keys that make sense machine-wide (daemon,
// snapshots, logs, auto_fetch, notifications). Repo-only blocks
// (databases, patches, hooks, …) are intentionally omitted — those
// belong in a per-repo `.treeman.yaml`. Pure function; no disk writes.
//
// The modeline points at the global schema file installed alongside it
// (config.GlobalConfigPath's sibling) so editors validate the file
// against the global-scoped schema, flagging repo-only keys.
func RenderGlobalTemplate() string {
	root := mapNode()
	root.HeadComment = "yaml-language-server: $schema=" + schema.GlobalURL +
		"\n\ntreeman user-global config — machine-wide defaults shared by every repo.\n" +
		"Repo-specific blocks (databases, patches, hooks, main_worktree, env_sources)\n" +
		"belong in each project's .treeman.yaml, not here."

	// daemon: process-level settings (global-only). Plain scalars render
	// unquoted, so `info`/`30`/`true` parse as the right YAML types.
	daemon := mapNode("log_level", scalar("info"))
	mapKeyNode(daemon, "log_level").LineComment = "debug | info | warn | error"
	mapSet(root, "daemon", daemon)

	// snapshots: machine-wide template cache + retention (global-only).
	snapshots := mapNode(
		"max_age_days", scalar("30"),
		"max_total_gb", scalar("20"),
	)
	mapKeyNode(snapshots, "max_age_days").HeadComment = "post-migration template cache eviction policy"
	mapSet(root, "snapshots", snapshots)

	// logs: daemon prune of the shared event log (global-only).
	logs := mapNode("keep_days", scalar("14"))
	mapKeyNode(logs, "keep_days").LineComment = "0 keeps forever"
	mapSet(root, "logs", logs)

	// auto_fetch: daemon periodic git fetch cadence (default; repos may
	// opt out with `auto_fetch: {enabled: false}`).
	af := mapNode(
		"enabled", scalar("true"),
		"interval_minutes", scalar("15"),
	)
	mapSet(root, "auto_fetch", af)

	// notifications: desktop banners on lifecycle changes (global-only,
	// off by default).
	notif := mapNode("enabled", scalar("false"))
	mapKeyNode(notif, "enabled").HeadComment = "desktop notifications on worktree lifecycle changes"
	mapSet(root, "notifications", notif)

	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	out, err := yamlpatch.Marshal(doc)
	if err != nil {
		return "daemon:\n  log_level: info\n"
	}
	return string(out)
}

// RenderTemplate inspects `cwd` and returns a fully-declarative
// `.treeman.yaml` body. Pure function — no disk writes — so it can
// be exercised in unit tests against fixture trees.
//
// Builds a yaml.Node AST and marshals it through yamlpatch.Marshal
// so the rendering rules (two-space indent, comment placement) match
// what config_set produces when patching the file later. Explanatory
// comments ride on HeadComment / LineComment fields so the output
// stays self-documenting without ad-hoc string concatenation.
func RenderTemplate(cwd string) string {
	name := filepath.Base(cwd)
	has := func(p string) bool {
		_, err := os.Stat(filepath.Join(cwd, p))
		return err == nil
	}

	jsPkgMgr := detectJSPkgMgr(cwd)
	hasGoMod := has("go.mod")
	hasComposer := has("composer.json")
	frontendDir := ""
	for _, candidate := range []string{"frontend", "resources/js", "client", "ui"} {
		if has(filepath.Join(candidate, "package.json")) {
			frontendDir = candidate
			break
		}
	}

	detected := framework.DefaultRegistry().DetectAll(cwd)

	root := mapNode()
	// HeadComment on the root mapping becomes the file's first line —
	// the yaml-language-server modeline that wires editor hinting.
	root.HeadComment = "yaml-language-server: $schema=" + schema.URL

	// worktrees:
	mapSet(root, "worktrees", mapNode(
		"root", scalar(".worktrees"),
		"copies", seqNode(scalar(".env")),
	))

	// env_sources: read-list for the credential resolver (patches
	// live in the top-level `patches:` block).
	envSources := defaultEnvSourcesFor(detected, has)
	if len(envSources) > 0 {
		sources := seqNode()
		for _, s := range envSources {
			sources.Content = append(sources.Content, scalar(s))
		}
		sources.LineComment = "files read in order, last wins"
		mapSet(root, "env_sources", sources)
	}

	// patches: — generic file-rewriting block. For Laravel projects
	// the canonical patch is `.env.testing` → DB_TEST_DATABASE.
	// Each patched file is wired through git's clean/smudge filter
	// so it doesn't appear dirty in `git status` but pulls can still
	// overwrite it. Driver is auto-detected from the `.env`
	// extension.
	if hasFrameworkNamed(detected, "laravel") {
		setMap := mapNode(
			"DB_TEST_DATABASE", scalar(name+"_testing_{slug}"),
		)
		patch := mapNode(
			"file", scalar(".env.testing"),
			"set", setMap,
		)
		mapSet(root, "patches", seqNode(patch))
	}

	// databases:
	if len(detected) > 0 {
		spec := detected[0]
		engine := spec.EngineHint
		if engine == "" {
			engine = "mysql"
		}
		db := mapNode(
			"engine", scalar(engine),
			"name_template", scalar(name+"_testing_{slug}"),
			"migrate", migrateBlock(spec),
			"inputs", inputNodes(spec),
			"test_clones", mapNode(
				"clones", scalar("auto"),
				"name_template", scalar(name+"_testing_{slug}_test_{n}"),
			),
		)
		mapSet(root, "databases", seqNode(db))
	}

	// hooks: on-create-before-engines:
	actions := seqNode()
	if hasComposer {
		actions.Content = append(actions.Content, hookGroup("composer install --no-interaction --prefer-dist"))
	}
	if jsPkgMgr != "" {
		actions.Content = append(actions.Content, hookGroup(jsInstallCmd(jsPkgMgr)))
	}
	if frontendDir != "" && jsPkgMgr != "" {
		actions.Content = append(actions.Content, hookGroup("cd "+frontendDir+" && "+jsInstallCmd(jsPkgMgr)))
	}
	if hasGoMod {
		actions.Content = append(actions.Content, hookGroup("go mod download"))
	}
	if len(actions.Content) == 0 {
		actions.Style = yaml.FlowStyle
		actions.HeadComment = "add install commands here — each action is one parallel group;\nuse run: [step1, step2] for sequenced commands inside one group."
	}
	hooks := mapNode("on-create-before-engines", actions)
	mapSet(root, "hooks", hooks)
	if len(detected) == 0 {
		// Attach the "no databases yet" hint as a HeadComment on the
		// hooks key so it lands above the hooks: section, taking the
		// slot the missing databases: block would have occupied.
		mapKeyNode(root, "hooks").HeadComment = "databases: [] — add an engine block above when DB scaffolding is needed."
	}

	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	body, err := yamlpatch.Marshal(doc)
	if err != nil {
		// Falls back to a minimal stub so callers always get something
		// loadable. Should never trigger — the builders above only
		// emit well-formed nodes.
		return "worktrees:\n  root: .worktrees\n"
	}
	return string(body)
}

// migrateBlock renders the `migrations.migrate:` mapping for a
// scaffolded framework — the shell command and the env-var overrides
// that point the framework's migrate CLI at the per-run template DB.
// Env keys are sorted so the emitted YAML is deterministic.
func migrateBlock(spec framework.Spec) *yaml.Node {
	out := mapNode("run", scalar(spec.MigrateRun))
	if len(spec.MigrateEnv) > 0 {
		envKeys := make([]string, 0, len(spec.MigrateEnv))
		for k := range spec.MigrateEnv {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		env := mapNode()
		for _, k := range envKeys {
			mapSet(env, k, scalar(spec.MigrateEnv[k]))
		}
		mapSet(out, "env", env)
	}
	return out
}

// inputNodes builds the `databases[].inputs:` sequence from a
// detected framework. Each (migration_dir, file_glob) pair becomes
// one entry; each lockfile becomes another. All entries are content-
// hashed — there is no per-entry hash mode anymore.
func inputNodes(spec framework.Spec) *yaml.Node {
	type entry struct{ glob, label string }
	var entries []entry
	seen := map[string]struct{}{}
	add := func(glob, label string) {
		if _, dup := seen[glob]; dup {
			return
		}
		seen[glob] = struct{}{}
		entries = append(entries, entry{glob: glob, label: label})
	}
	for _, dir := range spec.MigrationDirs {
		for _, pat := range spec.FileGlobs {
			add(dir+"/**/"+pat, "migrations")
		}
	}
	for _, lf := range spec.Lockfiles {
		add(lf, "lockfile")
	}
	seq := seqNode()
	for _, e := range entries {
		m := mapNode("glob", scalar(e.glob))
		if e.label != "" {
			mapSet(m, "label", scalar(e.label))
		}
		seq.Content = append(seq.Content, m)
	}
	return seq
}

// DetectFrameworkNames returns the names of every migration framework
// detected under cwd. Exposed so callers (CLI status lines, MCP
// `init_repo` output) can report what triggered the scaffold without
// re-running detection.
func DetectFrameworkNames(cwd string) []string {
	specs := framework.DefaultRegistry().DetectAll(cwd)
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// ─── yaml.Node builders ──────────────────────────────────────────

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}

// mapNode constructs a mapping. Arguments alternate key (string) +
// value (*yaml.Node).
func mapNode(kv ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		val, _ := kv[i+1].(*yaml.Node)
		n.Content = append(n.Content, scalar(key), val)
	}
	return n
}

func mapSet(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content, scalar(key), value)
}

func seqNode(items ...*yaml.Node) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode}
	n.Content = append(n.Content, items...)
	return n
}

// hookGroup wraps a shell command in the `{run: "<cmd>"}` Action
// shape the hooks runner expects.
func hookGroup(cmd string) *yaml.Node {
	return mapNode("run", scalar(cmd))
}

// mapKeyNode returns the key scalar node matching `name` inside a
// MappingNode, or nil. Used to attach LineComment / HeadComment to
// the key (the scalar that prints "name:") rather than its value.
func mapKeyNode(m *yaml.Node, name string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == name {
			return m.Content[i]
		}
	}
	return nil
}

func defaultEnvSourcesFor(detected []framework.Spec, has func(string) bool) []string {
	candidates := []string{".env", ".env.local"}
	if hasFrameworkNamed(detected, "laravel") {
		candidates = append(candidates, ".env.testing", ".env.testing.local")
	} else if hasFrameworkNamed(detected, "rails", "django") {
		candidates = append(candidates, ".env.test", ".env.test.local")
	}
	var out []string
	for _, c := range candidates {
		if has(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		out = candidates[:1]
	}
	return out
}

func hasFrameworkNamed(specs []framework.Spec, names ...string) bool {
	for _, s := range specs {
		if slices.Contains(names, s.Name) {
			return true
		}
	}
	return false
}

// detectJSPkgMgr returns "yarn", "pnpm", "npm", "bun", or "deno"
// based on lockfile presence — or "" when there's no package.json.
// Precedence matches the lockfile that pins the toolchain version.
func detectJSPkgMgr(cwd string) string {
	has := func(p string) bool {
		_, err := os.Stat(filepath.Join(cwd, p))
		return err == nil
	}
	switch {
	case has("deno.lock") || has("deno.json"):
		return "deno"
	case has("bun.lockb"):
		return "bun"
	case has("pnpm-lock.yaml"):
		return "pnpm"
	case has("yarn.lock"):
		return "yarn"
	case has("package-lock.json"):
		return "npm"
	case has("package.json"):
		return "npm"
	}
	return ""
}

func jsInstallCmd(mgr string) string {
	switch mgr {
	case "yarn":
		return "yarn install --frozen-lockfile"
	case "pnpm":
		return "pnpm install --frozen-lockfile"
	case "bun":
		return "bun install --frozen-lockfile"
	case "deno":
		return "deno install --lock=deno.lock"
	case "npm":
		return "npm ci"
	}
	return "npm ci"
}
