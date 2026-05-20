//! Idempotent env-file + phpunit.xml patcher. Ports `set_env_key` /
//! `set_phpunit_env` from `.gwt-postcreate`. After patching, optionally
//! `git update-index --skip-worktree <file>` via libgit2 so the diff
//! stays out of `git status`.

use std::path::Path;

use anyhow::{Context, Result};
use regex::Regex;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PatchOutcome {
    /// Wrote new content.
    Updated,
    /// File already had the requested value(s) — no write.
    Unchanged,
    /// File did not exist — no write.
    Missing,
}

/// Apply `(key, value)` pairs to a dotenv-style file. Replaces existing
/// `KEY=...` lines in place; appends missing keys at the end.
pub fn patch_env_file(path: &Path, pairs: &[(String, String)]) -> Result<PatchOutcome> {
    if !path.exists() {
        return Ok(PatchOutcome::Missing);
    }
    let original = std::fs::read_to_string(path)
        .with_context(|| format!("read {}", path.display()))?;
    let mut content = original.clone();
    for (key, value) in pairs {
        content = patch_env_one(&content, key, value);
    }
    if content == original {
        return Ok(PatchOutcome::Unchanged);
    }
    std::fs::write(path, &content)
        .with_context(|| format!("write {}", path.display()))?;
    Ok(PatchOutcome::Updated)
}

fn patch_env_one(content: &str, key: &str, value: &str) -> String {
    let pattern = format!(r"(?m)^{}\s*=.*$", regex::escape(key));
    let re = Regex::new(&pattern).expect("dynamic regex");
    if re.is_match(content) {
        re.replace_all(content, format!("{key}={value}").as_str()).into_owned()
    } else {
        let mut out = content.to_string();
        if !out.is_empty() && !out.ends_with('\n') {
            out.push('\n');
        }
        out.push_str(&format!("{key}={value}\n"));
        out
    }
}

/// Apply `(key, value)` pairs to a phpunit.xml file. Replaces existing
/// `<env name="KEY" .../>` entries in place; inserts new ones before
/// `</php>`.
pub fn patch_phpunit_file(path: &Path, pairs: &[(String, String)]) -> Result<PatchOutcome> {
    if !path.exists() {
        return Ok(PatchOutcome::Missing);
    }
    let original = std::fs::read_to_string(path)
        .with_context(|| format!("read {}", path.display()))?;
    let mut content = original.clone();
    for (key, value) in pairs {
        content = patch_phpunit_one(&content, key, value);
    }
    if content == original {
        return Ok(PatchOutcome::Unchanged);
    }
    std::fs::write(path, &content)
        .with_context(|| format!("write {}", path.display()))?;
    Ok(PatchOutcome::Updated)
}

fn patch_phpunit_one(content: &str, key: &str, value: &str) -> String {
    let key_esc = regex::escape(key);
    let existing = Regex::new(&format!(r#"<env name="{key_esc}"[^/]*/>"#)).unwrap();
    let replacement = format!(r#"<env name="{key}" value="{value}" force="true"/>"#);
    if existing.is_match(content) {
        existing.replace_all(content, replacement.as_str()).into_owned()
    } else {
        // Bash impl uses `\t<env ... force="true"/>\n\t</php>` indent;
        // mirror that for byte-exact diff parity.
        let inject = format!("\t{replacement}\n\t</php>");
        content.replacen("</php>", &inject, 1)
    }
}

/// `git update-index --skip-worktree <file>` via libgit2. Returns `Ok(false)`
/// if the file isn't tracked (e.g. `.env.testing` is gitignored — no-op).
pub fn skip_worktree(repo_root: &Path, file: &Path) -> Result<bool> {
    let repo = git2::Repository::discover(repo_root)
        .with_context(|| format!("discover git repo at {}", repo_root.display()))?;
    let workdir = repo.workdir().context("repo has no workdir")?;
    let rel = file.strip_prefix(workdir).unwrap_or(file);
    let mut index = repo.index()?;
    let entry = match index.get_path(rel, 0) {
        Some(e) => e,
        None => return Ok(false),
    };
    // SKIP_WORKTREE = 0x4000 in the extended flags. `git2::IndexEntry` is
    // `repr(C)` mirroring `git_index_entry` — clone field-by-field.
    const SKIP_WORKTREE: u16 = 0x4000;
    let new_entry = git2::IndexEntry {
        ctime: entry.ctime,
        mtime: entry.mtime,
        dev: entry.dev,
        ino: entry.ino,
        mode: entry.mode,
        uid: entry.uid,
        gid: entry.gid,
        file_size: entry.file_size,
        id: entry.id,
        flags: entry.flags,
        flags_extended: entry.flags_extended | SKIP_WORKTREE,
        path: entry.path,
    };
    index.add(&new_entry)?;
    index.write()?;
    Ok(true)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn tmpfile(content: &str) -> std::path::PathBuf {
        let mut p = std::env::temp_dir();
        p.push(format!("treeman-test-{}.tmp",
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()));
        let mut f = std::fs::File::create(&p).unwrap();
        f.write_all(content.as_bytes()).unwrap();
        p
    }

    #[test]
    fn env_replace_existing_key() {
        let p = tmpfile("FOO=old\nBAR=keep\n");
        let r = patch_env_file(&p, &[("FOO".into(), "new".into())]).unwrap();
        assert_eq!(r, PatchOutcome::Updated);
        let s = std::fs::read_to_string(&p).unwrap();
        assert_eq!(s, "FOO=new\nBAR=keep\n");
    }

    #[test]
    fn env_append_missing_key() {
        let p = tmpfile("FOO=keep\n");
        let r = patch_env_file(&p, &[("BAR".into(), "new".into())]).unwrap();
        assert_eq!(r, PatchOutcome::Updated);
        let s = std::fs::read_to_string(&p).unwrap();
        assert_eq!(s, "FOO=keep\nBAR=new\n");
    }

    #[test]
    fn env_idempotent_when_value_matches() {
        let p = tmpfile("FOO=already\n");
        let r = patch_env_file(&p, &[("FOO".into(), "already".into())]).unwrap();
        assert_eq!(r, PatchOutcome::Unchanged);
    }

    #[test]
    fn phpunit_replace_existing_env() {
        let p = tmpfile(
            "<phpunit>\n<php>\n\t<env name=\"FOO\" value=\"old\"/>\n\t</php>\n</phpunit>\n"
        );
        patch_phpunit_file(&p, &[("FOO".into(), "new".into())]).unwrap();
        let s = std::fs::read_to_string(&p).unwrap();
        assert!(s.contains(r#"<env name="FOO" value="new" force="true"/>"#));
    }

    #[test]
    fn phpunit_inject_new_env() {
        let p = tmpfile("<phpunit>\n<php>\n\t</php>\n</phpunit>\n");
        patch_phpunit_file(&p, &[("FOO".into(), "bar".into())]).unwrap();
        let s = std::fs::read_to_string(&p).unwrap();
        assert!(s.contains(r#"<env name="FOO" value="bar" force="true"/>"#));
    }
}
