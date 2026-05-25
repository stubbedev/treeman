//go:build e2e

// Package compression_e2e drives a real MySQL container against
// dump fixtures compressed with each supported format. Detection is
// magic-byte-based, so file extension is irrelevant — every fixture
// is named `seed.sql` and only the byte content changes.
package compression_e2e

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

const seedSQL = `
CREATE TABLE widgets (
  id INT PRIMARY KEY AUTO_INCREMENT,
  name VARCHAR(64) NOT NULL
) ENGINE=InnoDB;
INSERT INTO widgets (name) VALUES ('alpha'), ('beta'), ('gamma');
`

func TestCompressionFormatsLoadEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "mysql:13346", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13346", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	cases := []struct {
		name     string
		compress func(t *testing.T, in []byte) []byte
	}{
		{"plain", func(_ *testing.T, in []byte) []byte { return in }},
		{"gzip", func(t *testing.T, in []byte) []byte {
			var b bytes.Buffer
			w := gzip.NewWriter(&b)
			_, _ = w.Write(in)
			_ = w.Close()
			return b.Bytes()
		}},
		{"zstd", func(t *testing.T, in []byte) []byte {
			var b bytes.Buffer
			w, err := zstd.NewWriter(&b)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(in)
			_ = w.Close()
			return b.Bytes()
		}},
		{"xz", func(t *testing.T, in []byte) []byte {
			var b bytes.Buffer
			w, err := xz.NewWriter(&b)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(in)
			_ = w.Close()
			return b.Bytes()
		}},
		{"bzip2", bzip2Compress},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wt := t.TempDir()
			body := c.compress(t, []byte(seedSQL))
			if err := os.WriteFile(filepath.Join(wt, "seed.sql"), body, 0o644); err != nil {
				t.Fatal(err)
			}

			cfg := &config.Config{
				Connections: config.ConnectionsConfig{
					Mysql: &config.MysqlConn{
						Host: "127.0.0.1", Port: 13346,
						User: "root", Password: "rootpw",
					},
				},
				Databases: []config.DatabaseConfig{
					{
						Engine:       "mysql",
						NameTemplate: fmt.Sprintf("tm_cmp_%s_{slug}", c.name),
						Dump:         &config.DumpSpec{Path: "seed.sql"},
					},
				},
			}
			env := harness.NewEnv(t, wt)
			outs := env.RunPrepare(t, cfg)
			o := harness.AssertOutcome(t, outs, "mysql", false)
			t.Logf("%s: sourceDB=%s", c.name, o.SourceDB)

			db := openMySQL(t, o.SourceDB)
			defer db.Close()
			var n int
			if err := db.QueryRow("SELECT COUNT(*) FROM widgets").Scan(&n); err != nil {
				t.Fatalf("count widgets: %v", err)
			}
			if n != 3 {
				t.Errorf("%s: widgets count = %d, want 3", c.name, n)
			}
		})
	}
}

// bzip2Compress shells to the bzip2 binary (stdlib has no writer).
// Skips if bzip2 isn't installed.
func bzip2Compress(t *testing.T, want []byte) []byte {
	t.Helper()
	if _, err := exec.LookPath("bzip2"); err != nil {
		t.Skip("bzip2 binary not installed")
	}
	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = bytes.NewReader(want)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bzip2: %v", err)
	}
	// Sanity: stdlib bzip2 reader must decompress to the input.
	dec, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(stdout.Bytes())))
	if err != nil {
		t.Fatalf("bzip2 readback: %v", err)
	}
	if !bytes.Equal(dec, want) {
		t.Fatal("bzip2 roundtrip mismatch")
	}
	return stdout.Bytes()
}

func openMySQL(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("root:rootpw@tcp(127.0.0.1:13346)/%s", dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}
