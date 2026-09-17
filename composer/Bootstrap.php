<?php

declare(strict_types=1);

namespace Stubbedev\Treeman;

use RuntimeException;
use Throwable;

final class Bootstrap
{
    private const MAX_ARCHIVE = 268435456;

    public static function version(string $packageRoot): string
    {
        $root = realpath($packageRoot);
        if ($root === false) {
            throw new RuntimeException('Package directory does not exist.');
        }
        for ($dir = dirname($root); $dir !== dirname($dir); $dir = dirname($dir)) {
            $file = $dir . '/composer/installed.json';
            if (!is_file($file)) {
                continue;
            }
            $data = json_decode((string) file_get_contents($file), true, 512, JSON_THROW_ON_ERROR);
            foreach ($data['packages'] ?? [] as $package) {
                if (($package['name'] ?? '') !== 'stubbedev/treeman') {
                    continue;
                }
                if (!isset($package['install-path']) || realpath(dirname($file) . '/' . $package['install-path']) !== $root) {
                    throw new RuntimeException('Composer package install path does not match this bootstrap.');
                }
                return self::releaseVersion($package['version'] ?? '');
            }
        }
        throw new RuntimeException('Cannot resolve installed stubbedev/treeman version. Install a tagged release with Composer 2.2+ (not a source checkout).');
    }

    public static function releaseVersion(string $version): string
    {
        if (!preg_match('/^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$/D', $version)) {
            throw new RuntimeException('A tagged release is required; development versions and branch aliases cannot select release binaries: ' . $version);
        }
        return ltrim($version, 'v');
    }

    public static function platform(string $os, string $arch): string
    {
        $oses = ['linux' => 'linux', 'darwin' => 'darwin'];
        $arches = ['x86_64' => 'amd64', 'amd64' => 'amd64', 'aarch64' => 'arm64', 'arm64' => 'arm64'];
        $os = strtolower($os);
        $arch = strtolower($arch);
        if (!isset($oses[$os], $arches[$arch])) {
            throw new RuntimeException("Unsupported platform $os/$arch; releases support Linux/macOS on amd64/arm64.");
        }
        return $oses[$os] . '-' . $arches[$arch];
    }

    public static function cacheRoot(): string
    {
        $path = getenv('TREEMAN_COMPOSER_CACHE');
        if ($path === false || $path === '') {
            $base = getenv('XDG_CACHE_HOME');
            if ($base === false || $base === '') {
                $home = getenv('HOME');
                if ($home === false || $home === '') {
                    throw new RuntimeException('Set TREEMAN_COMPOSER_CACHE to an absolute writable cache directory.');
                }
                $base = $home . '/.cache';
            }
            $path = $base . '/treeman/composer';
        }
        if (!str_starts_with($path, '/')) {
            throw new RuntimeException('TREEMAN_COMPOSER_CACHE/XDG_CACHE_HOME must be an absolute path.');
        }
        return rtrim($path, '/');
    }

    public static function ensure(string $version, string $platform, string $cache, bool $offline, ?callable $download = null): string
    {
        $version = self::releaseVersion($version);
        if (!in_array($platform, ['linux-amd64', 'linux-arm64', 'darwin-amd64', 'darwin-arm64'], true)) {
            throw new RuntimeException('Unsupported release platform: ' . $platform);
        }
        clearstatcache();
        if (is_link($cache) || (is_dir($cache) && (fileperms($cache) & 0022) !== 0)) {
            throw new RuntimeException('A private cache directory without symlinks is required: ' . $cache);
        }
        $stage = "treeman-$version-$platform";
        $target = "$cache/$stage";
        if (self::validCache($target)) {
            return $target;
        }
        if ($offline) {
            throw new RuntimeException("Offline: no verified cache for $stage at $target. Warm it online with vendor/bin/treeman --version first.");
        }
        self::directory($cache);
        $lockPath = "$cache/$stage.lock";
        if (is_link($lockPath)) {
            throw new RuntimeException('Refusing symlink cache lock.');
        }
        $lock = fopen($lockPath, 'c');
        if ($lock === false || !flock($lock, LOCK_EX)) {
            throw new RuntimeException('Cannot lock Composer binary cache: ' . $cache);
        }
        $temporary = null;
        try {
            if (self::validCache($target)) {
                return $target;
            }
            if (file_exists($target) || is_link($target)) {
                throw new RuntimeException("Invalid binary cache at $target; remove that entry and retry online.");
            }
            $temporary = $cache . '/.download-' . bin2hex(random_bytes(12));
            self::directory($temporary);
            $asset = "$stage.tar.gz";
            $base = "https://github.com/stubbedev/treeman/releases/download/v$version/$asset";
            fwrite(STDERR, "treeman: downloading verified release v$version ($platform) to $target\n");
            $download ??= self::download(...);
            $download($base . '.sha256', "$temporary/checksum", 4096);
            $checksum = (string) file_get_contents("$temporary/checksum");
            if (!preg_match('/^([a-fA-F0-9]{64})[ \t]+\*?' . preg_quote($asset, '/') . '\s*$/D', $checksum, $matches)) {
                throw new RuntimeException('Invalid published SHA256 file for ' . $asset);
            }
            $download($base, "$temporary/archive.tar.gz", self::MAX_ARCHIVE);
            if (!hash_equals(strtolower($matches[1]), hash_file('sha256', "$temporary/archive.tar.gz"))) {
                throw new RuntimeException('SHA256 mismatch for ' . $asset);
            }
            self::extract("$temporary/archive.tar.gz", $stage, $temporary);
            $hashes = [];
            foreach (['treeman', 'treemand'] as $binary) {
                $hashes[$binary] = hash_file('sha256', "$temporary/$binary");
            }
            self::put("$temporary/verified.json", json_encode($hashes, JSON_THROW_ON_ERROR));
            if (!chmod("$temporary/verified.json", 0600)) {
                throw new RuntimeException('Cannot secure cache metadata.');
            }
            unlink("$temporary/checksum");
            unlink("$temporary/archive.tar.gz");
            if (!rename($temporary, $target)) {
                throw new RuntimeException('Cannot publish binary cache: ' . $target);
            }
            $temporary = null;
            return $target;
        } finally {
            if ($temporary !== null) {
                foreach (scandir($temporary) ?: [] as $name) {
                    if ($name !== '.' && $name !== '..') {
                        unlink($temporary . '/' . $name);
                    }
                }
                rmdir($temporary);
            }
            flock($lock, LOCK_UN);
            fclose($lock);
        }
    }

    private static function validCache(string $path): bool
    {
        clearstatcache();
        if (!is_dir($path) || is_link($path) || (fileperms($path) & 0022) !== 0
            || is_link("$path/verified.json") || !is_file("$path/verified.json") || (fileperms("$path/verified.json") & 0022) !== 0) {
            return false;
        }
        $hashes = json_decode((string) file_get_contents("$path/verified.json"), true);
        foreach (['treeman', 'treemand'] as $binary) {
            $file = "$path/$binary";
            if (!isset($hashes[$binary]) || !is_string($hashes[$binary]) || is_link($file) || !is_file($file)
                || !is_executable($file) || (fileperms($file) & 0022) !== 0) {
                return false;
            }
            $actual = hash_file('sha256', $file);
            if ($actual === false || !hash_equals($hashes[$binary], $actual)) {
                return false;
            }
        }
        return true;
    }

    private static function directory(string $path): void
    {
        if (is_link($path) || (!is_dir($path) && !@mkdir($path, 0700, true) && !is_dir($path))) {
            throw new RuntimeException('Cannot create private cache directory: ' . $path);
        }
        if ((fileperms($path) & 0022) !== 0) {
            throw new RuntimeException('Cache directory must not be group/world writable: ' . $path);
        }
    }

    private static function put(string $path, string $contents): void
    {
        if (file_put_contents($path, $contents) !== strlen($contents)) {
            throw new RuntimeException('Cannot write ' . $path);
        }
    }

    public static function download(string $url, string $destination, int $limit): void
    {
        $file = fopen($destination, 'xb');
        if ($file === false) {
            throw new RuntimeException('Cannot create download: ' . $destination);
        }
        $curl = curl_init($url);
        $received = 0;
        curl_setopt_array($curl, [
            CURLOPT_FOLLOWLOCATION => true,
            CURLOPT_MAXREDIRS => 5,
            CURLOPT_PROTOCOLS => CURLPROTO_HTTPS,
            CURLOPT_REDIR_PROTOCOLS => CURLPROTO_HTTPS,
            CURLOPT_SSL_VERIFYPEER => true,
            CURLOPT_SSL_VERIFYHOST => 2,
            CURLOPT_CONNECTTIMEOUT => 20,
            CURLOPT_TIMEOUT => 300,
            CURLOPT_FAILONERROR => true,
            CURLOPT_USERAGENT => 'stubbedev-treeman-composer',
            CURLOPT_WRITEFUNCTION => static function ($handle, string $chunk) use ($file, $limit, &$received): int {
                $received += strlen($chunk);
                return $received <= $limit ? (int) fwrite($file, $chunk) : 0;
            },
        ]);
        try {
            if (!curl_exec($curl) || curl_getinfo($curl, CURLINFO_RESPONSE_CODE) !== 200) {
                throw new RuntimeException("Download failed: $url (" . curl_error($curl) . '). Network is required on first use; the exact tag and assets must be published.');
            }
        } finally {
            fclose($file);
        }
    }

    public static function extract(string $archive, string $stage, string $destination): void
    {
        $stream = gzopen($archive, 'rb');
        if ($stream === false) {
            throw new RuntimeException('Cannot open release archive.');
        }
        $seen = [];
        $total = 0;
        try {
            while (true) {
                $header = self::read($stream, 512);
                if ($header === str_repeat("\0", 512)) {
                    if (self::read($stream, 512) !== $header) {
                        throw new RuntimeException('Invalid tar terminator.');
                    }
                    break;
                }
                $sum = array_sum(unpack('C*', substr_replace($header, str_repeat(' ', 8), 148, 8)) ?: []);
                if ($sum !== self::octal(substr($header, 148, 8))) {
                    throw new RuntimeException('Invalid tar header checksum.');
                }
                $name = rtrim(substr($header, 0, 100), "\0");
                $prefix = rtrim(substr($header, 345, 155), "\0");
                $type = $header[156];
                $size = self::octal(substr($header, 124, 12));
                $total += $size + 512;
                if ($total > self::MAX_ARCHIVE || $prefix !== '' || isset($seen[$name])) {
                    throw new RuntimeException('Unsafe or oversized release archive.');
                }
                $seen[$name] = true;
                $allowed = [$stage . '/treeman', $stage . '/treemand', $stage . '/README.md', $stage . '/LICENSE-MIT', $stage . '/LICENSE-APACHE'];
                if (($name === $stage . '/' || $name === $stage) && $type === '5' && $size === 0) {
                    continue;
                }
                if (!in_array($name, $allowed, true) || !in_array($type, ["\0", '0'], true) || trim(substr($header, 157, 100), "\0") !== '') {
                    throw new RuntimeException('Unsafe archive entry: ' . $name);
                }
                $binary = basename($name);
                $isBinary = $binary === 'treeman' || $binary === 'treemand';
                if ($isBinary && $size === 0) {
                    throw new RuntimeException('Empty release binary: ' . $binary);
                }
                $output = $isBinary ? fopen("$destination/$binary", 'xb') : null;
                if ($output === false) {
                    throw new RuntimeException('Cannot create extracted binary: ' . $binary);
                }
                try {
                    $remaining = $size;
                    while ($remaining > 0) {
                        $chunk = self::read($stream, min(1048576, $remaining));
                        $remaining -= strlen($chunk);
                        if ($output !== null && fwrite($output, $chunk) !== strlen($chunk)) {
                            throw new RuntimeException('Cannot write extracted binary: ' . $binary);
                        }
                    }
                } finally {
                    if ($output !== null) {
                        fclose($output);
                    }
                }
                self::read($stream, (512 - $size % 512) % 512);
                if ($isBinary && !chmod("$destination/$binary", 0700)) {
                    throw new RuntimeException('Cannot mark binary executable.');
                }
            }
            foreach (['treeman', 'treemand'] as $binary) {
                if (!isset($seen["$stage/$binary"])) {
                    throw new RuntimeException('Release archive missing ' . $binary);
                }
            }
        } finally {
            gzclose($stream);
        }
    }

    private static function octal(string $value): int
    {
        $value = trim($value, " \0");
        if ($value === '' || !preg_match('/^[0-7]+$/D', $value)) {
            throw new RuntimeException('Invalid tar numeric field.');
        }
        return intval($value, 8);
    }

    private static function read($stream, int $length): string
    {
        $data = '';
        while (strlen($data) < $length) {
            $chunk = gzread($stream, min(1048576, $length - strlen($data)));
            if ($chunk === false || $chunk === '') {
                throw new RuntimeException('Truncated release archive.');
            }
            $data .= $chunk;
        }
        return $data;
    }

    public static function run(string $binary, array $arguments): never
    {
        try {
            if (!function_exists('pcntl_exec')) {
                throw new RuntimeException('PHP CLI with pcntl_exec enabled is required to preserve signals and exit status.');
            }
            $version = self::version(dirname(__DIR__));
            $platform = self::platform(PHP_OS, php_uname('m'));
            $cache = self::ensure($version, $platform, self::cacheRoot(), getenv('TREEMAN_COMPOSER_OFFLINE') === '1');
            $path = getenv('PATH');
            if (!putenv('PATH=' . $cache . ($path === false ? '' : PATH_SEPARATOR . $path))) {
                throw new RuntimeException('Cannot expose cached binaries on PATH.');
            }
            pcntl_exec($cache . '/' . $binary, $arguments);
            throw new RuntimeException('Cannot execute cached binary: ' . $cache . '/' . $binary);
        } catch (Throwable $error) {
            fwrite(STDERR, 'treeman Composer bootstrap: ' . $error->getMessage() . "\n");
            exit(1);
        }
    }
}
