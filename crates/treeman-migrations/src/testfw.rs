//! Test-framework detection. Determines how many DB clones to provision
//! and which worker env var the test runner exposes. Independent of the
//! migration-framework registry: a Laravel repo can run paratest OR pest,
//! a Rust repo can use cargo test OR cargo-nextest, etc.

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerIndex {
    /// 1, 2, …, N (paratest TEST_TOKEN; jest JEST_WORKER_ID; parallel_tests TEST_ENV_NUMBER).
    OneBased,
    /// 0, 1, …, N-1.
    ZeroBased,
    /// gw0, gw1, … (pytest-xdist).
    PytestXdist,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CloneStrategy {
    /// One DB clone per worker (num_cpus by default).
    PerWorker,
    /// Single shared DB (no clones beyond the source).
    Shared,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TestFrameworkSpec {
    pub name: String,
    pub language: String,
    /// Env var the runner sets per worker. None = no per-worker scoping.
    pub worker_env: Option<String>,
    pub worker_index: WorkerIndex,
    pub clone_strategy: CloneStrategy,
}

/// Detect every test framework whose markers match.
pub fn detect_all(repo_root: &Path) -> Vec<TestFrameworkSpec> {
    builtins()
        .into_iter()
        .filter(|(_s, p)| p(repo_root))
        .map(|(s, _)| s)
        .collect()
}

/// Sum of clones across detected per-worker frameworks; capped at the
/// number of CPUs. Returns `None` when nothing was detected.
pub fn detected_clone_count(repo_root: &Path) -> Option<u32> {
    let detected = detect_all(repo_root);
    if detected.is_empty() {
        return None;
    }
    // Per-worker frameworks all want one clone per worker. Multiple
    // detected frameworks share the same clone pool: take num_cpus once.
    if detected
        .iter()
        .any(|s| matches!(s.clone_strategy, CloneStrategy::PerWorker))
    {
        Some(num_cpus())
    } else {
        Some(1)
    }
}

pub fn num_cpus() -> u32 {
    std::thread::available_parallelism()
        .map(|n| n.get() as u32)
        .unwrap_or(4)
}

// ───────────────────────── builtins ─────────────────────────

type Predicate = fn(&Path) -> bool;

fn builtins() -> Vec<(TestFrameworkSpec, Predicate)> {
    vec![
        // PHP — paratest (Laravel default, brianium/paratest). Sets
        // TEST_TOKEN (1-based, unique among CURRENTLY running workers,
        // reused on re-run) and UNIQUE_TEST_TOKEN (unique per run and
        // per process). Per-worker DB scoping should use TEST_TOKEN.
        // https://github.com/paratestphp/paratest README "Test token"
        (
            spec(
                "paratest",
                "php",
                Some("TEST_TOKEN"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| composer_has(r, "brianium/paratest"),
        ),
        // PHP — pest with --parallel uses paratest under the hood,
        // same TEST_TOKEN. https://pestphp.com/docs/parallel-testing
        (
            spec(
                "pest",
                "php",
                Some("TEST_TOKEN"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| composer_has(r, "pestphp/pest"),
        ),
        // PHP — Codeception. Parallel via robo-paracept (Robo task
        // runner) or via the paratest extension on PHPUnit-style
        // tests. We mark Shared because neither path exposes a
        // stable per-worker env var on its own; users who run
        // Codeception inside paratest get TEST_TOKEN via that detector.
        // https://codeception.com/docs/ParallelExecution
        (
            spec(
                "codeception",
                "php",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| composer_has(r, "codeception/codeception") && !composer_has(r, "brianium/paratest"),
        ),
        // PHP — vanilla phpunit (no parallel). Shared DB.
        (
            spec(
                "phpunit",
                "php",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| {
                composer_has(r, "phpunit/phpunit")
                    && !composer_has(r, "brianium/paratest")
                    && !composer_has(r, "pestphp/pest")
            },
        ),
        // Python — pytest-xdist. PYTEST_XDIST_WORKER is "gw0", "gw1", …
        // 0-based after the "gw" prefix. https://pytest-xdist.readthedocs.io/
        (
            spec(
                "pytest-xdist",
                "python",
                Some("PYTEST_XDIST_WORKER"),
                WorkerIndex::PytestXdist,
                CloneStrategy::PerWorker,
            ),
            |r| python_dep_has(r, "pytest-xdist"),
        ),
        // Python — pytest-parallel. Does NOT expose PYTEST_XDIST_WORKER
        // or any standard per-worker env. Project is largely
        // unmaintained; we detect it so it shows up in `fw detect`
        // but mark Shared so we don't pretend to fan out clones.
        (
            spec(
                "pytest-parallel",
                "python",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| python_dep_has(r, "pytest-parallel"),
        ),
        // Python — plain pytest (shared DB).
        (
            spec(
                "pytest",
                "python",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| {
                python_dep_has(r, "pytest")
                    && !python_dep_has(r, "pytest-xdist")
                    && !python_dep_has(r, "pytest-parallel")
            },
        ),
        // Ruby — parallel_tests. TEST_ENV_NUMBER is empty string for
        // worker 1 ("", not "1") and "2", "3", … for the rest unless
        // `--first-is-1` is passed. Consuming code must handle both
        // forms. https://github.com/grosser/parallel_tests
        (
            spec(
                "parallel_tests",
                "ruby",
                Some("TEST_ENV_NUMBER"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| gemfile_has(r, "parallel_tests"),
        ),
        // Ruby — rspec without parallel_tests (shared DB).
        (
            spec(
                "rspec",
                "ruby",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| gemfile_has(r, "rspec") && !gemfile_has(r, "parallel_tests"),
        ),
        // Ruby — minitest without parallel_tests (shared DB).
        (
            spec(
                "minitest",
                "ruby",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| gemfile_has(r, "minitest") && !gemfile_has(r, "parallel_tests"),
        ),
        // JS/TS — jest. JEST_WORKER_ID is 1-based; set to "1" when
        // --runInBand is used (single-process mode). https://jestjs.io/
        (
            spec(
                "jest",
                "javascript",
                Some("JEST_WORKER_ID"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| package_json_dep_has(r, "jest"),
        ),
        // JS/TS — vitest. VITEST_POOL_ID is 1-based and bounded by
        // maxWorkers (Jest-equivalent); VITEST_WORKER_ID is globally
        // unique but unbounded as workers are recycled. POOL_ID is
        // the right one for DB scoping.
        // https://github.com/vitest-dev/vitest/pull/1473
        (
            spec(
                "vitest",
                "javascript",
                Some("VITEST_POOL_ID"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| package_json_dep_has(r, "vitest"),
        ),
        // JS/TS — playwright. TEST_PARALLEL_INDEX is 0..workers-1
        // (bounded — use this for DB scoping). TEST_WORKER_INDEX is
        // 1-based globally unique (unbounded). https://playwright.dev/docs/test-parallel
        (
            spec(
                "playwright",
                "javascript",
                Some("TEST_PARALLEL_INDEX"),
                WorkerIndex::ZeroBased,
                CloneStrategy::PerWorker,
            ),
            |r| package_json_dep_has(r, "@playwright/test"),
        ),
        // JS/TS — cypress-parallel (tnicola/cypress-parallel). Sets
        // CYPRESS_THREAD as a 1-based index. The newer
        // @badeball/cypress-parallel and cypress-split use other
        // names (CYPRESS_SPLIT_INDEX, CI_NODE_INDEX) — users should
        // switch worker_env via the custom framework hook.
        (
            spec(
                "cypress-parallel",
                "javascript",
                Some("CYPRESS_THREAD"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| {
                package_json_dep_has(r, "cypress-parallel")
                    || package_json_dep_has(r, "@badeball/cypress-parallel")
            },
        ),
        // JS/TS — mocha-parallel-tests (legacy; mocha 8+ has native
        // --parallel but doesn't expose a per-worker env by default).
        // https://github.com/mocha-parallel/mocha-parallel-tests
        (
            spec(
                "mocha-parallel-tests",
                "javascript",
                Some("MOCHA_PARALLEL_INDEX"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| package_json_dep_has(r, "mocha-parallel-tests"),
        ),
        // JS/TS — plain mocha (shared DB).
        (
            spec(
                "mocha",
                "javascript",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| {
                package_json_dep_has(r, "mocha") && !package_json_dep_has(r, "mocha-parallel-tests")
            },
        ),
        // JS/TS — bun test. Bun runs tests in-process; no per-worker
        // process env. Mark Shared.
        // https://bun.sh/docs/cli/test
        (
            spec(
                "bun-test",
                "javascript",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| {
                (has_path(r, "bun.lockb") || has_path(r, "bun.lock"))
                    && !package_json_dep_has(r, "vitest")
                    && !package_json_dep_has(r, "jest")
            },
        ),
        // JS/TS — deno test. No per-worker process env; tests are
        // sandboxed and run concurrently in one runtime by default.
        // https://docs.deno.com/runtime/manual/basics/testing/
        (
            spec(
                "deno-test",
                "javascript",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| has_path(r, "deno.json") || has_path(r, "deno.jsonc"),
        ),
        // Rust — cargo-nextest. NEXTEST_TEST_GLOBAL_SLOT is a unique
        // slot within the run, real env var (also NEXTEST_TEST_GROUP_SLOT
        // for grouped tests). Slot indexing is internal — treat as
        // 0-based by convention; user can override in custom config.
        // https://nexte.st/docs/configuration/env-vars/
        (
            spec(
                "cargo-nextest",
                "rust",
                Some("NEXTEST_TEST_GLOBAL_SLOT"),
                WorkerIndex::ZeroBased,
                CloneStrategy::PerWorker,
            ),
            |r| {
                has_path(r, ".config/nextest.toml")
                    || has_path(r, "nextest.toml")
                    || cargo_toml_has(r, "nextest")
            },
        ),
        // Rust — plain cargo test (per-thread). DBs typically shared.
        (
            spec(
                "cargo-test",
                "rust",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| {
                has_path(r, "Cargo.toml")
                    && !has_path(r, ".config/nextest.toml")
                    && !has_path(r, "nextest.toml")
            },
        ),
        // Go — Ginkgo v2. Process index only available via the
        // `GinkgoParallelProcess()` Go function (1-based). No env
        // var is set by Ginkgo, so per-clone scoping has to be
        // wired by the test setup code itself — typically by
        // calling that function and using the result as the slug
        // suffix. We mark PerWorker so the daemon still provisions
        // clones; worker_env = None tells callers there is no env
        // to read. https://onsi.github.io/ginkgo/
        (
            spec(
                "ginkgo",
                "go",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| {
                has_path(r, "go.mod")
                    && (gosum_has(r, "github.com/onsi/ginkgo/v2")
                        || gosum_has(r, "github.com/onsi/ginkgo"))
            },
        ),
        // Go — go test parallelizes by package via `-p N` (and by
        // `t.Parallel()` within a package). Per-package DBs not
        // idiomatic; treat as shared. ginkgo handled separately.
        (
            spec(
                "go-test",
                "go",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| {
                has_path(r, "go.mod")
                    && !gosum_has(r, "github.com/onsi/ginkgo/v2")
                    && !gosum_has(r, "github.com/onsi/ginkgo")
            },
        ),
        // Java — Maven Surefire. The fork number is exposed as the
        // `${surefire.forkNumber}` PLACEHOLDER (1-based), which the
        // plugin replaces in `argLine`, `environmentVariables`, and
        // `systemPropertyVariables`. To use it as an env var, wire it
        // through `<environmentVariables>` in pom.xml — e.g.
        //   <SUREFIRE_FORK_NUMBER>${surefire.forkNumber}</SUREFIRE_FORK_NUMBER>
        // Without this wiring there is no SUREFIRE_FORK_NUMBER env;
        // we publish the canonical name so users can pin it the
        // same way.
        // https://maven.apache.org/surefire/maven-surefire-plugin/examples/fork-options-and-parallel-execution.html
        (
            spec(
                "maven-surefire",
                "java",
                Some("SUREFIRE_FORK_NUMBER"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| has_path(r, "pom.xml"),
        ),
        // Java — Gradle Test. Worker id is the `org.gradle.test.worker`
        // SYSTEM property (1-based), not an env var. Wire it via:
        //   test { environment 'GRADLE_TEST_WORKER',
        //         System.getProperty('org.gradle.test.worker') }
        // or use `systemProperty 'forkNumber', '...'`. Like Surefire
        // above we publish the canonical name; user must wire it.
        (
            spec(
                "gradle-test",
                "java",
                Some("GRADLE_TEST_WORKER"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| has_path(r, "build.gradle") || has_path(r, "build.gradle.kts"),
        ),
        // .NET — xUnit / NUnit with collection parallelism (shared by default).
        // ConnectionStrings__* / `xunit.runner.json` parallelism is
        // intra-process; no per-fork env.
        (
            spec(
                "dotnet-test",
                "dotnet",
                None,
                WorkerIndex::OneBased,
                CloneStrategy::Shared,
            ),
            |r| has_any_with_ext(r, &["csproj", "fsproj", "sln"]),
        ),
        // Elixir — `mix test --partitions N` shards tests across N
        // processes (typically across CI runners or jobs). The
        // partition number is supplied by the caller through the
        // MIX_TEST_PARTITION env (1..N, 1-based).
        // https://hexdocs.pm/mix/Mix.Tasks.Test.html
        (
            spec(
                "mix-test",
                "elixir",
                Some("MIX_TEST_PARTITION"),
                WorkerIndex::OneBased,
                CloneStrategy::PerWorker,
            ),
            |r| has_path(r, "mix.exs"),
        ),
    ]
}

fn spec(
    name: &str,
    language: &str,
    worker_env: Option<&str>,
    worker_index: WorkerIndex,
    clone_strategy: CloneStrategy,
) -> TestFrameworkSpec {
    TestFrameworkSpec {
        name: name.into(),
        language: language.into(),
        worker_env: worker_env.map(|s| s.into()),
        worker_index,
        clone_strategy,
    }
}

// ───────────────────────── marker predicates ─────────────────────────

fn has_path(root: &Path, rel: &str) -> bool {
    root.join(rel).exists()
}

fn read_file(p: &Path) -> Option<String> {
    if !p.is_file() {
        return None;
    }
    std::fs::read_to_string(p).ok()
}

fn composer_has(root: &Path, dep: &str) -> bool {
    let Some(txt) = read_file(&root.join("composer.json")) else {
        return false;
    };
    txt.contains(dep)
}

fn package_json_dep_has(root: &Path, dep: &str) -> bool {
    let Some(txt) = read_file(&root.join("package.json")) else {
        return false;
    };
    let needle = format!("\"{dep}\"");
    txt.contains(&needle)
}

fn cargo_toml_has(root: &Path, needle: &str) -> bool {
    // Workspace or single-crate Cargo.toml.
    let mut paths: Vec<PathBuf> = vec![root.join("Cargo.toml")];
    paths.extend(globbed_cargo_files(root));
    for p in paths {
        if let Some(txt) = read_file(&p) {
            if txt.contains(needle) {
                return true;
            }
        }
    }
    false
}

fn globbed_cargo_files(root: &Path) -> Vec<PathBuf> {
    let mut out = vec![];
    for sub in ["crates", "services", "apps", "packages"] {
        let dir = root.join(sub);
        if !dir.is_dir() {
            continue;
        }
        let Ok(rd) = std::fs::read_dir(&dir) else {
            continue;
        };
        for e in rd.flatten() {
            let p = e.path().join("Cargo.toml");
            if p.is_file() {
                out.push(p);
            }
        }
    }
    out
}

fn gosum_has(root: &Path, module_path: &str) -> bool {
    for f in ["go.sum", "go.mod"] {
        if let Some(txt) = read_file(&root.join(f)) {
            if txt.contains(module_path) {
                return true;
            }
        }
    }
    false
}

fn gemfile_has(root: &Path, gem: &str) -> bool {
    for f in ["Gemfile", "Gemfile.lock"] {
        let Some(txt) = read_file(&root.join(f)) else {
            continue;
        };
        if txt.contains(gem) {
            return true;
        }
    }
    false
}

fn python_dep_has(root: &Path, dep: &str) -> bool {
    for f in [
        "pyproject.toml",
        "requirements.txt",
        "requirements-dev.txt",
        "requirements/dev.txt",
        "Pipfile",
        "Pipfile.lock",
        "setup.py",
        "setup.cfg",
        "poetry.lock",
        "uv.lock",
    ] {
        let Some(txt) = read_file(&root.join(f)) else {
            continue;
        };
        if txt.contains(dep) {
            return true;
        }
    }
    false
}

fn has_any_with_ext(root: &Path, exts: &[&str]) -> bool {
    let Ok(rd) = std::fs::read_dir(root) else {
        return false;
    };
    for e in rd.flatten() {
        let Some(ext) = e
            .path()
            .extension()
            .and_then(|e| e.to_str())
            .map(|s| s.to_string())
        else {
            continue;
        };
        if exts.contains(&ext.as_str()) {
            return true;
        }
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmpdir(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "treeman-testfw-{}-{}",
            name,
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn detects_paratest_from_composer() {
        let d = tmpdir("paratest");
        std::fs::write(
            d.join("composer.json"),
            r#"{"require-dev":{"brianium/paratest":"^7.0"}}"#,
        )
        .unwrap();
        let names: Vec<_> = detect_all(&d).into_iter().map(|s| s.name).collect();
        assert!(names.contains(&"paratest".to_string()), "got {names:?}");
        std::fs::remove_dir_all(&d).ok();
    }

    #[test]
    fn detects_pytest_xdist_over_plain_pytest() {
        let d = tmpdir("pytest-xdist");
        std::fs::write(d.join("requirements.txt"), "pytest\npytest-xdist\n").unwrap();
        let names: Vec<_> = detect_all(&d).into_iter().map(|s| s.name).collect();
        assert!(names.contains(&"pytest-xdist".to_string()), "got {names:?}");
        assert!(
            !names.contains(&"pytest".to_string()),
            "plain pytest should defer to xdist: {names:?}"
        );
        std::fs::remove_dir_all(&d).ok();
    }

    #[test]
    fn detects_nextest_over_plain_cargo() {
        let d = tmpdir("nextest");
        std::fs::write(d.join("Cargo.toml"), "[package]\nname=\"x\"\n").unwrap();
        std::fs::create_dir_all(d.join(".config")).unwrap();
        std::fs::write(d.join(".config/nextest.toml"), "[profile.default]\n").unwrap();
        let names: Vec<_> = detect_all(&d).into_iter().map(|s| s.name).collect();
        assert!(
            names.contains(&"cargo-nextest".to_string()),
            "got {names:?}"
        );
        assert!(
            !names.contains(&"cargo-test".to_string()),
            "plain cargo-test should defer: {names:?}"
        );
        std::fs::remove_dir_all(&d).ok();
    }

    #[test]
    fn clones_per_worker_returns_num_cpus() {
        let d = tmpdir("clones");
        std::fs::write(
            d.join("composer.json"),
            r#"{"require-dev":{"brianium/paratest":"^7.0"}}"#,
        )
        .unwrap();
        let n = detected_clone_count(&d).unwrap();
        assert!(n >= 1);
        assert_eq!(n, num_cpus());
        std::fs::remove_dir_all(&d).ok();
    }

    #[test]
    fn shared_only_returns_one_clone() {
        let d = tmpdir("shared");
        std::fs::write(d.join("go.mod"), "module x\n").unwrap();
        let n = detected_clone_count(&d).unwrap();
        assert_eq!(n, 1);
        std::fs::remove_dir_all(&d).ok();
    }
}
