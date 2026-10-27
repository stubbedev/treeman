package dumpload

import (
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

func TestDockerExecMysqlArgs(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		wantTCP bool
	}{
		{name: "tcp host dials tcp", host: "127.0.0.1", wantTCP: true},
		{name: "empty host dials tcp", host: "", wantTCP: true},
		{name: "bridge ip dials tcp", host: "172.18.0.4", wantTCP: true},
		{name: "socket host keeps client default socket", host: "/var/run/mysqld/mysqld.sock", wantTCP: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn := &config.MysqlConn{Host: c.host, User: "root"}
			args := dockerExecMysqlArgs("cid123", conn, "root", "target_db")
			joined := strings.Join(args, " ")
			hasTCP := strings.Contains(joined, "--protocol=TCP --host=127.0.0.1")
			if hasTCP != c.wantTCP {
				t.Fatalf("args %q: tcp flags present=%v want %v", joined, hasTCP, c.wantTCP)
			}
			if got, want := joined, "cid123 mysql"; !strings.Contains(got, want) {
				t.Errorf("args %q should exec mysql in container cid123", got)
			}
			if !strings.HasSuffix(joined, "-u root target_db") {
				t.Errorf("args %q should end with the user and database", joined)
			}
		})
	}
}
