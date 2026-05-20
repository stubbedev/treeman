//! Read shell-style env files (`.env`, `.env.testing`, etc.).
//!
//! Mirrors the subset of the dotenv spec used by Laravel, Symfony, Node
//! (`dotenv`), Python (`python-dotenv`), Ruby (`dotenv`), and sqlx-cli.
//! Supports:
//!   * `KEY=VALUE` and `export KEY=VALUE`
//!   * Single- or double-quoted values
//!   * Inline `# comment` after unquoted values
//!   * `${OTHER}` variable expansion against the same file's prior keys
//!     and the process env
//!
//! Does NOT support:
//!   * Multiline values (Laravel doesn't emit them; if needed later, add
//!     a heredoc detector)
//!   * Command substitution `$(cmd)` (security hazard)

use std::collections::HashMap;
use std::path::Path;

use anyhow::{Context, Result};

#[derive(Debug, Clone, Default)]
pub struct EnvFile {
    pub vars: HashMap<String, String>,
    pub source: Option<std::path::PathBuf>,
}

impl EnvFile {
    pub fn get(&self, key: &str) -> Option<&str> {
        self.vars.get(key).map(|s| s.as_str())
    }
    pub fn merge(&mut self, other: EnvFile) {
        for (k, v) in other.vars {
            self.vars.entry(k).or_insert(v);
        }
    }
}

pub fn parse(input: &str) -> EnvFile {
    let mut out = EnvFile::default();
    for line in input.lines() {
        let line = line.trim_start();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let line = line.strip_prefix("export ").unwrap_or(line);
        let Some(eq) = line.find('=') else {
            continue;
        };
        let key = line[..eq].trim().to_string();
        let mut chars = key.chars();
        let first_ok = chars
            .next()
            .map(|c| c.is_ascii_alphabetic() || c == '_')
            .unwrap_or(false);
        if !first_ok || !chars.all(|c| c.is_ascii_alphanumeric() || c == '_') {
            continue;
        }
        let raw = line[eq + 1..].trim();
        let value = strip_quotes_and_comment(raw);
        let expanded = expand(&value, &out.vars);
        out.vars.insert(key, expanded);
    }
    out
}

pub fn read(path: &Path) -> Result<EnvFile> {
    let txt = std::fs::read_to_string(path)
        .with_context(|| format!("read env file {}", path.display()))?;
    let mut env = parse(&txt);
    env.source = Some(path.to_path_buf());
    Ok(env)
}

/// Load `path` if it exists; otherwise return an empty EnvFile.
pub fn read_optional(path: &Path) -> EnvFile {
    if path.is_file() {
        read(path).unwrap_or_default()
    } else {
        EnvFile::default()
    }
}

/// Walk through the conventional env files for a repo, with later files
/// overriding earlier (matches Laravel's `.env` → `.env.testing` order).
pub fn read_layered<P: AsRef<Path>>(paths: &[P]) -> EnvFile {
    let mut out = EnvFile::default();
    for p in paths {
        let one = read_optional(p.as_ref());
        for (k, v) in one.vars {
            out.vars.insert(k, v);
        }
    }
    out
}

fn strip_quotes_and_comment(s: &str) -> String {
    let s = s.trim();
    if (s.starts_with('"') && s.ends_with('"') && s.len() >= 2)
        || (s.starts_with('\'') && s.ends_with('\'') && s.len() >= 2)
    {
        return s[1..s.len() - 1].to_string();
    }
    // Strip inline comments on unquoted values: `VAL # note`
    if let Some(hash) = s.find(" #") {
        return s[..hash].trim_end().to_string();
    }
    s.to_string()
}

fn expand(s: &str, locals: &HashMap<String, String>) -> String {
    // Cheap `${VAR}` interpolation, no recursion.
    let mut out = String::with_capacity(s.len());
    let bytes = s.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'$' && i + 1 < bytes.len() && bytes[i + 1] == b'{' {
            if let Some(end) = s[i + 2..].find('}') {
                let key = &s[i + 2..i + 2 + end];
                if let Some(v) = locals.get(key).cloned().or_else(|| std::env::var(key).ok()) {
                    out.push_str(&v);
                } else {
                    out.push_str(&s[i..i + 2 + end + 1]);
                }
                i += 2 + end + 1;
                continue;
            }
        }
        out.push(bytes[i] as char);
        i += 1;
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn basic_kv() {
        let e = parse("FOO=bar\nBAZ=qux\n");
        assert_eq!(e.get("FOO"), Some("bar"));
        assert_eq!(e.get("BAZ"), Some("qux"));
    }

    #[test]
    fn quoted_values() {
        let e = parse(
            r#"A="hello world"
B='single quoted'
"#,
        );
        assert_eq!(e.get("A"), Some("hello world"));
        assert_eq!(e.get("B"), Some("single quoted"));
    }

    #[test]
    fn export_prefix() {
        let e = parse("export FOO=bar\n");
        assert_eq!(e.get("FOO"), Some("bar"));
    }

    #[test]
    fn inline_comment_unquoted_only() {
        let e = parse("FOO=bar # trailing\nBAZ=\"x # not a comment\"\n");
        assert_eq!(e.get("FOO"), Some("bar"));
        assert_eq!(e.get("BAZ"), Some("x # not a comment"));
    }

    #[test]
    fn comments_skipped() {
        let e = parse("# top\nFOO=bar\n#mid\nBAZ=qux\n");
        assert_eq!(e.vars.len(), 2);
    }

    #[test]
    fn expand_local() {
        let e = parse("FOO=hello\nBAR=${FOO}/world\n");
        assert_eq!(e.get("BAR"), Some("hello/world"));
    }

    #[test]
    fn ignores_invalid_keys() {
        let e = parse("12 = no\nFOO=ok\n  =empty\n");
        assert_eq!(e.vars.len(), 1);
        assert_eq!(e.get("FOO"), Some("ok"));
    }
}
