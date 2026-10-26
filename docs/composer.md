# Composer install

treeman is published on Packagist as
[`stubbedev/treeman`](https://packagist.org/packages/stubbedev/treeman),
so a PHP project can pin it as a dev dependency next to the code that
uses it.

```sh
composer require --dev stubbedev/treeman
vendor/bin/treeman --version
vendor/bin/treemand --version
```

Requires Composer 2.2+, PHP CLI 8.1+, and the `curl`, `pcntl` (with
`pcntl_exec` enabled) and `zlib` extensions. Linux amd64/arm64 and
macOS arm64 are supported; Windows and 32-bit platforms are not.

## How the bootstrap works

The package is a normal library with transparent `bin` bootstraps, not a
Composer plugin: no plugin approval or install scripts are involved, and
`composer install` does **not** download the native binaries.

- **Exact version.** Commit `composer.json` and `composer.lock`. The
  installed package metadata selects the exact release tag, never
  `latest`. Development branches and branch aliases are rejected.
- **Lazy download.** The first run of either bin announces and
  downloads both binaries from that tag's GitHub release, verifies the
  published SHA256, and extracts only allowlisted regular files.
- **Direct exec.** Later runs verify the cached binary hashes and `exec`
  them, preserving arguments, working directory, signals and exit
  status. The cache entry is prepended to `PATH` so the daemon finds
  its matching `treemand`.

## The binary cache

First use needs HTTPS access to GitHub release assets and a writable
cache directory: `$XDG_CACHE_HOME/treeman/composer`, falling back to
`$HOME/.cache/treeman/composer`. Set `TREEMAN_COMPOSER_CACHE` to an
absolute path to override it.

Entries are keyed by exact version and platform, serialized with a file
lock, and published atomically. The cache root, entry directory,
metadata and binaries must not be group- or world-writable, and
symlinks at those locations are rejected. Ancestor directories are not
validated, so keep the whole path owned by the executing user.

Hashes detect corruption, not an attacker who can rewrite both the
binaries and the cache metadata; published checksums protect against
corrupt downloads, not a compromised release publisher.

## Offline and air-gapped jobs

Warm the cache online with `vendor/bin/treeman --version`, then keep the
complete version/platform entry (with executable permissions) or the
whole cache. Set `TREEMAN_COMPOSER_OFFLINE=1` to forbid downloads; valid
cached entries also work offline without it, including from a read-only
cache. A missing or corrupt entry fails with a clear error and no other
version is substituted: remove it and warm it online again.

Composer's own package cache and `vendor/` must also be available for an
offline `composer install`; the binary cache does not replace them.

See [CI without a daemon](ci.md) for a complete CI recipe.
