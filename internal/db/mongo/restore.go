package mongo

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/stubbedev/treeman/internal/db/dumpload"
)

// Restore loads a `mongodump --archive` archive into targetDB via
// the `mongorestore` CLI. Compression (gzip/zstd/bzip2/xz) is
// auto-detected from the archive's magic bytes:
//
//   - none/gzip: passed natively to mongorestore via `--archive=<path>`
//     (plus `--gzip` for gzip). Lets mongorestore stream the file
//     directly without extra Go-side copies.
//   - zstd/bzip2/xz: decompressed in-process and piped to
//     mongorestore via stdin (`--archive` with no `=path`).
//
// `sourceDB` names the DB the archive was made from (e.g. the
// production DB the engineer ran `mongodump --db=` against).
// Treeman remaps it to `targetDB` via mongorestore's `--nsFrom`/
// `--nsTo` flags. mongorestore requires the asterisk count to
// match between from/to, so we use `<source>.*` → `<target>.*`
// (one star — collection names pass through). Pass sourceDB=""
// to restore without renaming (archive must already use the
// target name).
//
// Requires mongorestore on PATH. Returns a clear error if the
// binary isn't installed.
func Restore(ctx context.Context, uri, targetDB, sourceDB, dumpPath string) error {
	if _, err := exec.LookPath("mongorestore"); err != nil {
		return fmt.Errorf("mongorestore not found on PATH: %w", err)
	}
	rc, format, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	args := []string{
		"--uri=" + uri,
		"--drop",
		"--quiet",
	}
	if sourceDB != "" && sourceDB != targetDB {
		args = append(args,
			"--nsFrom="+sourceDB+".*",
			"--nsTo="+targetDB+".*",
		)
	}

	var cmd *exec.Cmd
	switch format {
	case dumpload.FormatNone:
		cmd = exec.CommandContext(ctx, "mongorestore", append(args, "--archive="+dumpPath)...)
	case dumpload.FormatGzip:
		// mongorestore handles gzip natively — let it do the work.
		cmd = exec.CommandContext(ctx, "mongorestore", append(args, "--archive="+dumpPath, "--gzip")...)
	default:
		// zstd/bzip2/xz: decompress in Go, pipe via stdin.
		cmd = exec.CommandContext(ctx, "mongorestore", append(args, "--archive")...)
		cmd.Stdin = rc
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mongorestore (%s): %w: %s", format, err, stderr.String())
	}
	return nil
}
