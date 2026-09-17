<?php

declare(strict_types=1);

require dirname(__DIR__) . '/Bootstrap.php';

use Stubbedev\Treeman\Bootstrap;

$root = sys_get_temp_dir() . '/treeman-composer-test-' . bin2hex(random_bytes(8));
mkdir($root, 0700);
$tests = 0;

function check(bool $condition, string $message): void
{
    if (!$condition) {
        throw new RuntimeException($message);
    }
}

function fails(callable $action, string $message): void
{
    try {
        $action();
    } catch (Throwable $error) {
        check(str_contains($error->getMessage(), $message), 'Unexpected error: ' . $error->getMessage());
        return;
    }
    throw new RuntimeException('Expected failure: ' . $message);
}

function test(string $name, callable $action): void
{
    global $tests;
    $action();
    ++$tests;
    echo "ok $tests - $name\n";
}

function entry(string $name, string $contents, string $type = '0', string $link = ''): string
{
    $header = str_pad($name, 100, "\0") . sprintf('%07o', 0700) . "\0" . str_repeat("0000000\0", 2)
        . sprintf('%011o', strlen($contents)) . "\0" . "00000000000\0" . str_repeat(' ', 8)
        . $type . str_pad($link, 100, "\0") . "ustar\00000";
    $header = str_pad($header, 512, "\0");
    $sum = array_sum(unpack('C*', $header));
    $header = substr_replace($header, sprintf('%06o', $sum) . "\0 ", 148, 8);
    return $header . $contents . str_repeat("\0", (512 - strlen($contents) % 512) % 512);
}

function archive(string $stage, string $extra = '', bool $daemon = true): string
{
    return gzencode(entry($stage . '/', '', '5')
        . entry($stage . '/treeman', "#!/bin/sh\nprintf '%s\\n' \"\$@\"\nexit 37\n")
        . ($daemon ? entry($stage . '/treemand', "#!/bin/sh\nexit 23\n") : '')
        . $extra . str_repeat("\0", 1024));
}

function downloader(string $archive, array &$urls, ?string $checksum = null): Closure
{
    return static function (string $url, string $destination, int $limit) use ($archive, &$urls, $checksum): void {
        $urls[] = $url;
        $data = str_ends_with($url, '.sha256')
            ? ($checksum ?? hash('sha256', $archive) . '  ' . basename(substr($url, 0, -7)) . "\n")
            : $archive;
        check(strlen($data) <= $limit, 'Download limit');
        file_put_contents($destination, $data);
    };
}

function removeTree(string $path): void
{
    if (is_dir($path) && !is_link($path)) {
        foreach (scandir($path) as $entry) {
            if ($entry !== '.' && $entry !== '..') {
                removeTree($path . '/' . $entry);
            }
        }
        rmdir($path);
    } else {
        unlink($path);
    }
}

try {
    test('platform matrix and unsupported systems', static function (): void {
        foreach (['Linux' => 'linux', 'Darwin' => 'darwin'] as $os => $expected) {
            foreach (['x86_64' => 'amd64', 'amd64' => 'amd64', 'aarch64' => 'arm64', 'arm64' => 'arm64'] as $arch => $want) {
                check(Bootstrap::platform($os, $arch) === "$expected-$want", 'Platform mapping');
            }
        }
        fails(fn () => Bootstrap::platform('Windows', 'AMD64'), 'Unsupported');
        fails(fn () => Bootstrap::platform('Linux', 'i686'), 'Unsupported');
    });
    test('exact versions, next patch, prereleases and no dev aliases', static function (): void {
        foreach (['v2.5.88' => '2.5.88', '2.5.89' => '2.5.89', 'v3.0.0-rc.1' => '3.0.0-rc.1'] as $input => $want) {
            check(Bootstrap::releaseVersion($input) === $want, 'Version mapping');
        }
        foreach (['dev-main', '2.5.x-dev', '2.5.88.0', '2.5.88 as 9.0.0', '../2.5.88', 'latest', '02.5.88'] as $bad) {
            fails(fn () => Bootstrap::releaseVersion($bad), 'tagged release');
        }
    });
    test('installed metadata pins package not root or stale lock', static function () use ($root): void {
        $vendor = "$root/app/custom-vendor";
        $package = "$vendor/stubbedev/treeman";
        mkdir($package, 0700, true);
        mkdir("$vendor/composer", 0700);
        $data = ['packages' => [['name' => 'stubbedev/treeman', 'version' => 'v2.5.89', 'install-path' => '../stubbedev/treeman']]];
        file_put_contents("$vendor/composer/installed.json", json_encode($data));
        file_put_contents("$root/app/composer.lock", json_encode(['packages-dev' => [['name' => 'stubbedev/treeman', 'version' => 'v1.0.0']]]));
        check(Bootstrap::version($package) === '2.5.89', 'Installed version wins');
        $data['packages'][0]['install-path'] = '../wrong';
        file_put_contents("$vendor/composer/installed.json", json_encode($data));
        fails(fn () => Bootstrap::version($package), 'install path');
        fails(fn () => Bootstrap::version(__DIR__), 'Cannot resolve');
    });
    $stage = 'treeman-2.5.88-linux-amd64';
    $bytes = archive($stage);
    $urls = [];
    $download = downloader($bytes, $urls);
    test('verified install, exact asset URL, both executables', static function () use ($root, $stage, $download, &$urls): void {
        $path = Bootstrap::ensure('v2.5.88', 'linux-amd64', "$root/cache", false, $download);
        check($path === "$root/cache/$stage", 'Cache key');
        check(is_executable("$path/treeman") && is_executable("$path/treemand"), 'Executables');
        check($urls === [
            "https://github.com/stubbedev/treeman/releases/download/v2.5.88/$stage.tar.gz.sha256",
            "https://github.com/stubbedev/treeman/releases/download/v2.5.88/$stage.tar.gz",
        ], 'Exact URLs');
    });
    test('offline cached use never downloads and misses fail', static function () use ($root, $stage): void {
        $never = static function (): void { throw new RuntimeException('Unexpected network'); };
        check(Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/cache", true, $never) === "$root/cache/$stage", 'Offline cache');
        check(Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/cache", false, $never) === "$root/cache/$stage", 'Warm cache');
        fails(fn () => Bootstrap::ensure('2.5.89', 'linux-amd64', "$root/cache", true, $never), 'Offline');
        fails(fn () => Bootstrap::ensure('2.5.88', 'linux-arm64', "$root/cache", true, $never), 'Offline');
        fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/missing", true, $never), 'Offline');
    });
    test('concurrent first use publishes one complete download', static function () use ($root, $bytes): void {
        $counter = "$root/download-count";
        file_put_contents($counter, '');
        file_put_contents("$root/concurrent.tar.gz", $bytes);
        $script = '<?php require ' . var_export(dirname(__DIR__) . '/Bootstrap.php', true) . ';'
            . '$root = ' . var_export($root, true) . ';'
            . <<<'PHP'
        \Stubbedev\Treeman\Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/concurrent-cache", false, static function ($url, $path, $limit) use ($root): void {
            file_put_contents("$root/download-count", "download\n", FILE_APPEND | LOCK_EX);
            usleep(50000);
            $bytes = file_get_contents("$root/concurrent.tar.gz");
            file_put_contents($path, str_ends_with($url, '.sha256') ? hash('sha256', $bytes) . '  ' . basename(substr($url, 0, -7)) . "\n" : $bytes);
        });
        PHP;
        file_put_contents("$root/concurrent.php", $script);
        $children = [];
        for ($i = 0; $i < 2; ++$i) {
            $children[] = proc_open([PHP_BINARY, "$root/concurrent.php"], [STDIN, STDOUT, STDERR], $pipes);
        }
        foreach ($children as $child) {
            check(proc_close($child) === 0, 'Concurrent bootstrap failed');
        }
        check(file_get_contents($counter) === "download\ndownload\n", 'Expected one checksum/archive pair');
    });
    test('checksum mismatch, malformed checksum and network failure leave no installation', static function () use ($root, $bytes, $stage): void {
        foreach ([str_repeat('0', 64) . "  $stage.tar.gz\n" => 'SHA256 mismatch', 'garbage' => 'Invalid published'] as $checksum => $message) {
            $urls = [];
            fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/failure", false, downloader($bytes, $urls, $checksum)), $message);
            check(!file_exists("$root/failure/$stage"), 'Partial cache published');
            check(glob("$root/failure/.download-*") === [], 'Temporary files leaked');
        }
        fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/failure", false, static function (): void {
            throw new RuntimeException('network unavailable');
        }), 'network unavailable');
    });
    test('unsafe archives reject traversal, links, duplicates and missing files', static function () use ($root, $stage): void {
        foreach ([
            archive($stage, entry('../escape', 'bad')),
            archive($stage, entry('/tmp/escape', 'bad')),
            archive($stage, entry($stage . '/link', '', '2', '/tmp/escape')),
            archive($stage, entry($stage . '/LICENSE-MIT', '', '1', $stage . '/treeman')),
            archive($stage, entry($stage . '/treeman', 'duplicate')),
            archive($stage, '', false),
            gzencode('truncated'),
            gzencode(str_repeat('x', 512)),
        ] as $index => $bad) {
            mkdir("$root/extract-$index", 0700);
            file_put_contents("$root/bad.tar.gz", $bad);
            $failed = false;
            try {
                Bootstrap::extract("$root/bad.tar.gz", $stage, "$root/extract-$index");
            } catch (RuntimeException) {
                $failed = true;
            }
            check($failed, "Unsafe archive accepted: $index");
        }
        check(!file_exists("$root/escape"), 'Path traversal');
    });
    test('corrupt and symlinked cached binaries are rejected offline', static function () use ($root, $stage): void {
        file_put_contents("$root/cache/$stage/treeman", 'corrupt');
        fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/cache", true), 'Offline');
        fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/cache", false), 'Invalid binary cache');
        unlink("$root/cache/$stage/treeman");
        symlink('/bin/sh', "$root/cache/$stage/treeman");
        fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/cache", true), 'Offline');
    });
    test('cache directory and lock symlinks rejected', static function () use ($root): void {
        symlink("$root/cache", "$root/cache-link");
        fails(fn () => Bootstrap::ensure('2.5.89', 'linux-amd64', "$root/cache-link", false), 'private cache');
        symlink('/dev/null', "$root/cache/treeman-2.5.89-linux-amd64.lock");
        fails(fn () => Bootstrap::ensure('2.5.89', 'linux-amd64', "$root/cache", false), 'symlink cache lock');
    });
    test('group writable cache is refused', static function () use ($root): void {
        mkdir("$root/public-cache", 0700);
        chmod("$root/public-cache", 0770);
        fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', "$root/public-cache", true), 'private cache');
    });
    test('group/world writable restored entries and files are rejected online and offline', static function () use ($root, $download): void {
        $cache = "$root/permissions-cache";
        $path = Bootstrap::ensure('2.5.88', 'linux-amd64', $cache, false, $download);
        $never = static function (): void { throw new RuntimeException('Unexpected network'); };
        foreach ([[$path, 0700], ["$path/verified.json", 0600], ["$path/treeman", 0700], ["$path/treemand", 0700]] as [$file, $mode]) {
            foreach ([0020, 0002] as $writable) {
                check(chmod($file, $mode | $writable), 'Cannot change fixture permissions');
                fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', $cache, true, $never), 'Offline');
                fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', $cache, false, $never), 'Invalid binary cache');
                check(chmod($file, $mode), 'Cannot restore fixture permissions');
                check(Bootstrap::ensure('2.5.88', 'linux-amd64', $cache, true, $never) === $path, 'Private entry rejected');
            }
        }
        foreach (['verified.json', 'treeman', 'treemand'] as $name) {
            rename("$path/$name", "$path/$name.saved");
            symlink("$path/$name.saved", "$path/$name");
            fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', $cache, true, $never), 'Offline');
            unlink("$path/$name");
            rename("$path/$name.saved", "$path/$name");
        }
        rename($path, "$path.saved");
        symlink("$path.saved", $path);
        fails(fn () => Bootstrap::ensure('2.5.88', 'linux-amd64', $cache, true, $never), 'Offline');
        unlink($path);
        rename("$path.saved", $path);
        foreach (['verified.json' => 0400, 'treeman' => 0500, 'treemand' => 0500] as $name => $mode) {
            chmod("$path/$name", $mode);
        }
        chmod($path, 0500);
        chmod($cache, 0500);
        try {
            check(Bootstrap::ensure('2.5.88', 'linux-amd64', $cache, true, $never) === $path, 'Read-only cache rejected');
        } finally {
            chmod($cache, 0700);
            chmod($path, 0700);
        }
    });
    test('native release tar format is accepted', static function () use ($root, $stage): void {
        mkdir("$root/native/$stage", 0700, true);
        mkdir("$root/native-out", 0700);
        file_put_contents("$root/native/$stage/treeman", 'binary');
        file_put_contents("$root/native/$stage/treemand", 'daemon');
        $process = proc_open(['tar', '--format=ustar', '-czf', "$root/native.tar.gz", '-C', "$root/native", $stage], [STDIN, STDOUT, STDERR], $pipes, null, ['COPYFILE_DISABLE' => '1', 'PATH' => getenv('PATH')]);
        check(proc_close($process) === 0, 'tar failed');
        Bootstrap::extract("$root/native.tar.gz", $stage, "$root/native-out");
        check(file_get_contents("$root/native-out/treeman") === 'binary', 'Native tar extraction');
    });
    test('Composer generated proxies preserve arguments, environment, cwd and exit status offline', static function () use ($root): void {
        $project = "$root/integration";
        mkdir($project, 0700);
        $source = dirname(__DIR__, 2);
        $manifest = [
            'name' => 'test/consumer',
            'require-dev' => ['stubbedev/treeman' => '2.5.89'],
            'repositories' => [['type' => 'path', 'url' => $source, 'options' => ['symlink' => false, 'versions' => ['stubbedev/treeman' => '2.5.89']]], ['packagist.org' => false]],
            'config' => ['vendor-dir' => 'custom-vendor', 'bin-dir' => 'tools', 'allow-plugins' => false],
        ];
        file_put_contents("$project/composer.json", json_encode($manifest));
        $process = proc_open(['composer', 'update', '--no-interaction', '--no-plugins', '--no-scripts', '--no-audit'], [STDIN, STDOUT, STDERR], $pipes, $project);
        check(proc_close($process) === 0, 'Composer fixture install failed');
        removeTree("$project/custom-vendor");
        removeTree("$project/tools");
        $process = proc_open(['composer', 'install', '--no-interaction', '--no-plugins', '--no-scripts'], [STDIN, STDOUT, STDERR], $pipes, $project);
        check(proc_close($process) === 0, 'Composer locked install failed');
        $platform = Bootstrap::platform(PHP_OS, php_uname('m'));
        $stage = "treeman-2.5.89-$platform";
        $payload = gzencode(entry($stage . '/treeman', "#!/bin/sh\nprintf '%s\\n' \"\$PWD\" \"\$TREEMAN_TEST_ENV\" \"\$@\"\nexit 37\n") . entry($stage . '/treemand', "#!/bin/sh\nexit 23\n") . str_repeat("\0", 1024));
        $urls = [];
        Bootstrap::ensure('2.5.89', $platform, "$root/integration-cache", false, downloader($payload, $urls));
        putenv('TREEMAN_COMPOSER_CACHE=' . "$root/integration-cache");
        putenv('TREEMAN_COMPOSER_OFFLINE=1');
        putenv('TREEMAN_TEST_ENV=inherited');
        foreach (['treeman' => 37, 'treemand' => 23] as $binary => $expected) {
            $process = proc_open([PHP_BINARY, "$project/tools/$binary", 'space argument', '; touch not-executed', ''], [0 => STDIN, 1 => ['pipe', 'w'], 2 => STDERR], $pipes, $project);
            $output = stream_get_contents($pipes[1]);
            fclose($pipes[1]);
            check(proc_close($process) === $expected, 'Exit status not preserved');
            if ($binary === 'treeman') {
                check($output === "$project\ninherited\nspace argument\n; touch not-executed\n\n", 'Arguments/environment/cwd not preserved');
            }
        }
        check(!file_exists("$project/not-executed"), 'Shell injection');
    });
    test('cached treeman resolves exact-version sibling daemon before unrelated PATH executable', static function () use ($root): void {
        $project = "$root/integration";
        $platform = Bootstrap::platform(PHP_OS, php_uname('m'));
        $stage = "treeman-2.5.89-$platform";
        $payload = gzencode(entry($stage . '/treeman', "#!/bin/sh\nexec treemand \"\$@\"\n")
            . entry($stage . '/treemand', "#!/bin/sh\nprintf '%s\\n' sibling \"\$PATH\" \"\$@\"\nexit 23\n") . str_repeat("\0", 1024));
        $urls = [];
        $cache = "$root/sibling-cache";
        $path = Bootstrap::ensure('2.5.89', $platform, $cache, false, downloader($payload, $urls));
        mkdir("$root/unrelated", 0700);
        file_put_contents("$root/unrelated/treemand", "#!/bin/sh\nprintf '%s\\n' unrelated\nexit 91\n");
        chmod("$root/unrelated/treemand", 0700);
        $originalPath = getenv('PATH');
        $inheritedPath = "$root/unrelated" . ($originalPath === false ? '' : PATH_SEPARATOR . $originalPath);
        $environment = getenv();
        $environment['PATH'] = $inheritedPath;
        $environment['TREEMAN_COMPOSER_CACHE'] = $cache;
        $environment['TREEMAN_COMPOSER_OFFLINE'] = '1';
        $process = proc_open([PHP_BINARY, "$project/tools/treeman", 'daemon', 'start'], [0 => STDIN, 1 => ['pipe', 'w'], 2 => STDERR], $pipes, $project, $environment);
        $output = stream_get_contents($pipes[1]);
        fclose($pipes[1]);
        check(proc_close($process) === 23, 'Unrelated daemon selected');
        check($output === "sibling\n$path" . PATH_SEPARATOR . "$inheritedPath\ndaemon\nstart\n", 'Sibling/PATH/arguments not preserved');
    });
    echo "$tests tests passed\n";
} finally {
    removeTree($root);
}
