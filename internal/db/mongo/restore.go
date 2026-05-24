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
// Every collection from the archive is restored into targetDB
// regardless of its original DB name (via `--nsFrom='*.*'` +
// `--nsTo='<targetDB>.*'`). Multi-DB archives are flattened.
//
// Requires mongorestore on PATH. Returns a clear error if the
// binary isn't installed.
func Restore(ctx context.Context, uri, targetDB, dumpPath string) error {
	if _, err := exec.LookPath("mongorestore"); err != nil {
		return fmt.Errorf("mongorestore not found on PATH: %w", err)
	}
	rc, format, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return err
	}
	defer rc.Close()

	args := []string{
		"--uri=" + uri,
		"--nsFrom=*.*",
		"--nsTo=" + targetDB + ".*",
		"--drop",
		"--quiet",
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
