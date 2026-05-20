//! Tiny `{slug}` / `{slug_dash}` / `{slug_redis_queue}` / `{slug_redis_cache}`
//! substitution. Hand-rolled because tinytemplate doesn't accept `{n}`-style
//! placeholders that contain underscores in a context that's also valid for
//! struct-rendering, and we only need literal key replacement.

use crate::slug::Slug;

#[derive(Debug, Clone)]
pub struct TemplateContext {
    pub slug: String,
    pub slug_dash: String,
    pub slug_redis_queue: String,
    pub slug_redis_cache: String,
    pub n: Option<u32>,
}

impl TemplateContext {
    pub fn from_slug(slug: &Slug) -> Self {
        let (queue, cache) = slug.redis_indices();
        Self {
            slug: slug.value.clone(),
            slug_dash: slug.dashed(),
            slug_redis_queue: queue.to_string(),
            slug_redis_cache: cache.to_string(),
            n: None,
        }
    }
    pub fn with_n(mut self, n: u32) -> Self {
        self.n = Some(n);
        self
    }
}

/// Replace `{key}` placeholders. Unknown keys raise an error so a typo in
/// the config doesn't silently render as empty.
pub fn render(template: &str, ctx: &TemplateContext) -> Result<String, RenderError> {
    let mut out = String::with_capacity(template.len());
    let mut chars = template.char_indices().peekable();
    while let Some((i, c)) = chars.next() {
        if c != '{' {
            out.push(c);
            continue;
        }
        // Find the matching closing brace.
        let rest = &template[i + 1..];
        let end = match rest.find('}') {
            Some(e) => e,
            None => return Err(RenderError::UnmatchedBrace(i)),
        };
        let key = &rest[..end];
        out.push_str(&lookup(key, ctx).ok_or_else(|| RenderError::UnknownKey(key.into()))?);
        // Advance the iterator past the closing brace.
        for _ in 0..(end + 1) {
            chars.next();
        }
    }
    Ok(out)
}

fn lookup<'a>(key: &str, ctx: &'a TemplateContext) -> Option<std::borrow::Cow<'a, str>> {
    Some(match key {
        "slug" => std::borrow::Cow::Borrowed(ctx.slug.as_str()),
        "slug_dash" => std::borrow::Cow::Borrowed(ctx.slug_dash.as_str()),
        "slug_redis_queue" => std::borrow::Cow::Borrowed(ctx.slug_redis_queue.as_str()),
        "slug_redis_cache" => std::borrow::Cow::Borrowed(ctx.slug_redis_cache.as_str()),
        "n" => std::borrow::Cow::Owned(ctx.n?.to_string()),
        _ => return None,
    })
}

#[derive(Debug, thiserror::Error)]
pub enum RenderError {
    #[error("unknown template key: {0}")]
    UnknownKey(String),
    #[error("unmatched '{{' at offset {0}")]
    UnmatchedBrace(usize),
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::slug::{Slug, SlugSource};

    fn ctx() -> TemplateContext {
        TemplateContext::from_slug(&Slug {
            value: "kon_1234".into(),
            source: SlugSource::Ticket,
        })
    }

    #[test]
    fn renders_known_keys() {
        let c = ctx();
        assert_eq!(
            render("kontainer_testing_{slug}", &c).unwrap(),
            "kontainer_testing_kon_1234"
        );
        assert_eq!(
            render("phpunit-{slug_dash}-", &c).unwrap(),
            "phpunit-kon-1234-"
        );
    }

    #[test]
    fn unknown_key_errors() {
        let c = ctx();
        let e = render("{nope}", &c).unwrap_err();
        assert!(matches!(e, RenderError::UnknownKey(_)));
    }

    #[test]
    fn n_token_required_when_used() {
        let c = ctx();
        assert!(render("test_{n}", &c).is_err());
        let c = c.with_n(3);
        assert_eq!(render("test_{n}", &c).unwrap(), "test_3");
    }
}
