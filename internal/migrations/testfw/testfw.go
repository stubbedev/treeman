// Package testfw detects which test framework owns a repo. Drives
// the paratest clone-count math. Ported from
// `crates/treeman-migrations/src/testfw.rs`.
package testfw

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// WorkerIndex describes the per-worker env value format.
type WorkerIndex string

const (
	WorkerOneBased    WorkerIndex = "one_based"
	WorkerZeroBased   WorkerIndex = "zero_based"
	WorkerPytestXdist WorkerIndex = "pytest_xdist"
)

// CloneStrategy says whether the framework needs one DB clone per
// worker or a single shared DB.
type CloneStrategy string

const (
	CloneShared    CloneStrategy = "shared"
	ClonePerWorker CloneStrategy = "per_worker"
)

// Spec describes one test framework + its scoping properties.
type Spec struct {
	Name        string
	Language    string
	WorkerEnv   string // env var the runner sets; "" = no per-worker env
	WorkerIndex WorkerIndex
	Strategy    CloneStrategy
	Detect      func(repoRoot string) bool `json:"-"`
}

// DetectAll returns every Spec whose marker predicate matches.
func DetectAll(repoRoot string) []Spec {
	out := []Spec{}
	for _, s := range builtins() {
		if s.Detect(repoRoot) {
			out = append(out, s)
		}
	}
	return out
}

// DetectedCloneCount returns the requested number of paratest clones
// based on detection. Returns 0 if nothing detected.
func DetectedCloneCount(repoRoot string) uint32 {
	detected := DetectAll(repoRoot)
	if len(detected) == 0 {
		return 0
	}
	for _, s := range detected {
		if s.Strategy == ClonePerWorker {
			return NumCPUs()
		}
	}
	return 1
}

// NumCPUs returns the available parallelism count (runtime.NumCPU
// when unspecified). Capped sanely.
func NumCPUs() uint32 {
	n := runtime.NumCPU()
	if n < 1 {
		return 4
	}
	return uint32(n)
}

func builtins() []Spec {
	return []Spec{
		spec("paratest", "php", "TEST_TOKEN", WorkerOneBased, ClonePerWorker,
			func(r string) bool { return composerHas(r, "brianium/paratest") }),
		spec("pest", "php", "TEST_TOKEN", WorkerOneBased, ClonePerWorker,
			func(r string) bool { return composerHas(r, "pestphp/pest") }),
		spec("codeception", "php", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return composerHas(r, "codeception/codeception") && !composerHas(r, "brianium/paratest")
			}),
		spec("phpunit", "php", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return composerHas(r, "phpunit/phpunit") &&
					!composerHas(r, "brianium/paratest") &&
					!composerHas(r, "pestphp/pest")
			}),
		spec("pytest-xdist", "python", "PYTEST_XDIST_WORKER", WorkerPytestXdist, ClonePerWorker,
			func(r string) bool { return pythonDepHas(r, "pytest-xdist") }),
		spec("pytest-parallel", "python", "", WorkerOneBased, CloneShared,
			func(r string) bool { return pythonDepHas(r, "pytest-parallel") }),
		spec("pytest", "python", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return pythonDepHas(r, "pytest") &&
					!pythonDepHas(r, "pytest-xdist") &&
					!pythonDepHas(r, "pytest-parallel")
			}),
		spec("parallel_tests", "ruby", "TEST_ENV_NUMBER", WorkerOneBased, ClonePerWorker,
			func(r string) bool { return gemfileHas(r, "parallel_tests") }),
		spec("rspec", "ruby", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return gemfileHas(r, "rspec") && !gemfileHas(r, "parallel_tests")
			}),
		spec("minitest", "ruby", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return gemfileHas(r, "minitest") && !gemfileHas(r, "parallel_tests")
			}),
		spec("jest", "javascript", "JEST_WORKER_ID", WorkerOneBased, ClonePerWorker,
			func(r string) bool { return packageJSONHas(r, "jest") }),
		spec("vitest", "javascript", "VITEST_POOL_ID", WorkerOneBased, ClonePerWorker,
			func(r string) bool { return packageJSONHas(r, "vitest") }),
		spec("playwright", "javascript", "TEST_PARALLEL_INDEX", WorkerZeroBased, ClonePerWorker,
			func(r string) bool { return packageJSONHas(r, "@playwright/test") }),
		spec("cypress-parallel", "javascript", "CYPRESS_THREAD", WorkerOneBased, ClonePerWorker,
			func(r string) bool {
				return packageJSONHas(r, "cypress-parallel") || packageJSONHas(r, "@badeball/cypress-parallel")
			}),
		spec("mocha-parallel-tests", "javascript", "MOCHA_PARALLEL_INDEX", WorkerOneBased, ClonePerWorker,
			func(r string) bool { return packageJSONHas(r, "mocha-parallel-tests") }),
		spec("mocha", "javascript", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return packageJSONHas(r, "mocha") && !packageJSONHas(r, "mocha-parallel-tests")
			}),
		spec("bun-test", "javascript", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return (hasPath(r, "bun.lockb") || hasPath(r, "bun.lock")) &&
					!packageJSONHas(r, "vitest") &&
					!packageJSONHas(r, "jest")
			}),
		spec("deno-test", "javascript", "", WorkerOneBased, CloneShared,
			func(r string) bool { return hasPath(r, "deno.json") || hasPath(r, "deno.jsonc") }),
		spec("cargo-nextest", "rust", "NEXTEST_TEST_GLOBAL_SLOT", WorkerZeroBased, ClonePerWorker,
			func(r string) bool {
				return hasPath(r, ".config/nextest.toml") || hasPath(r, "nextest.toml") ||
					cargoTomlHas(r, "nextest")
			}),
		spec("cargo-test", "rust", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return hasPath(r, "Cargo.toml") &&
					!hasPath(r, ".config/nextest.toml") && !hasPath(r, "nextest.toml")
			}),
		spec("ginkgo", "go", "", WorkerOneBased, ClonePerWorker,
			func(r string) bool {
				return hasPath(r, "go.mod") && (goSumHas(r, "github.com/onsi/ginkgo/v2") || goSumHas(r, "github.com/onsi/ginkgo"))
			}),
		spec("go-test", "go", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return hasPath(r, "go.mod") && !goSumHas(r, "github.com/onsi/ginkgo")
			}),
		spec("maven-surefire", "java", "SUREFIRE_FORK_NUMBER", WorkerOneBased, ClonePerWorker,
			func(r string) bool { return hasPath(r, "pom.xml") }),
		spec("gradle-test", "java", "GRADLE_TEST_WORKER", WorkerOneBased, ClonePerWorker,
			func(r string) bool {
				return hasPath(r, "build.gradle") || hasPath(r, "build.gradle.kts")
			}),
		spec("dotnet-test", "dotnet", "", WorkerOneBased, CloneShared,
			func(r string) bool {
				return hasAnyWithExt(r, []string{".csproj", ".fsproj", ".sln"})
			}),
		spec("mix-test", "elixir", "MIX_TEST_PARTITION", WorkerOneBased, ClonePerWorker,
			func(r string) bool { return hasPath(r, "mix.exs") }),
	}
}

func spec(name, lang, env string, idx WorkerIndex, strat CloneStrategy, detect func(string) bool) Spec {
	return Spec{
		Name: name, Language: lang, WorkerEnv: env,
		WorkerIndex: idx, Strategy: strat, Detect: detect,
	}
}

// ─────────────────────────── marker helpers ───────────────────────────

func hasPath(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

func readFile(p string) (string, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func composerHas(root, dep string) bool {
	txt, ok := readFile(filepath.Join(root, "composer.json"))
	if !ok {
		return false
	}
	return strings.Contains(txt, dep)
}

func packageJSONHas(root, dep string) bool {
	txt, ok := readFile(filepath.Join(root, "package.json"))
	if !ok {
		return false
	}
	return strings.Contains(txt, `"`+dep+`"`)
}

func gemfileHas(root, gem string) bool {
	for _, f := range []string{"Gemfile", "Gemfile.lock"} {
		if txt, ok := readFile(filepath.Join(root, f)); ok && strings.Contains(txt, gem) {
			return true
		}
	}
	return false
}

func pythonDepHas(root, dep string) bool {
	for _, f := range []string{
		"pyproject.toml", "requirements.txt", "requirements-dev.txt",
		"requirements/dev.txt", "Pipfile", "Pipfile.lock",
		"setup.py", "setup.cfg", "poetry.lock", "uv.lock",
	} {
		if txt, ok := readFile(filepath.Join(root, f)); ok && strings.Contains(txt, dep) {
			return true
		}
	}
	return false
}

func cargoTomlHas(root, needle string) bool {
	paths := []string{filepath.Join(root, "Cargo.toml")}
	for _, sub := range []string{"crates", "services", "apps", "packages"} {
		entries, err := os.ReadDir(filepath.Join(root, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			paths = append(paths, filepath.Join(root, sub, e.Name(), "Cargo.toml"))
		}
	}
	for _, p := range paths {
		if txt, ok := readFile(p); ok && strings.Contains(txt, needle) {
			return true
		}
	}
	return false
}

func goSumHas(root, modulePath string) bool {
	for _, f := range []string{"go.sum", "go.mod"} {
		if txt, ok := readFile(filepath.Join(root, f)); ok && strings.Contains(txt, modulePath) {
			return true
		}
	}
	return false
}

func hasAnyWithExt(root string, exts []string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		for _, want := range exts {
			if ext == want {
				return true
			}
		}
	}
	return false
}
