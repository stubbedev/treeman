package mcp

import "testing"

func TestHTTPConfig(t *testing.T) {
	// Env vars must not leak between cases.
	t.Setenv("TREEMAN_MCP_HTTP_ADDR", "")
	t.Setenv("TREEMAN_MCP_HTTP", "")
	t.Setenv("TREEMAN_MCP_HTTP_PATH", "")

	cases := []struct {
		name     string
		args     []string
		wantAddr string
		wantPath string
	}{
		{"no flags -> stdio", nil, "", defaultHTTPPath},
		{"bare --http", []string{"--http"}, defaultHTTPAddr, defaultHTTPPath},
		{"--http=addr", []string{"--http=0.0.0.0:9000"}, "0.0.0.0:9000", defaultHTTPPath},
		{"--http addr", []string{"--http", "127.0.0.1:9100"}, "127.0.0.1:9100", defaultHTTPPath},
		{"--http then unrelated flag", []string{"--http", "--foo"}, defaultHTTPAddr, defaultHTTPPath},
		{"--http then non-addr colon arg", []string{"--http", "key:value"}, defaultHTTPAddr, defaultHTTPPath},
		{"--http then :port", []string{"--http", ":9200"}, ":9200", defaultHTTPPath},
		{"custom path", []string{"--http", "--http-path=/treeman"}, defaultHTTPAddr, "/treeman"},
		{"path missing slash", []string{"--http", "--http-path", "treeman"}, defaultHTTPAddr, "/treeman"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, path := HTTPConfig(c.args)
			if addr != c.wantAddr || path != c.wantPath {
				t.Fatalf("HTTPConfig(%v) = (%q,%q), want (%q,%q)", c.args, addr, path, c.wantAddr, c.wantPath)
			}
		})
	}
}

func TestLooksLikeListenAddr(t *testing.T) {
	ok := []string{"127.0.0.1:8787", ":8787", "0.0.0.0:9000", "[::1]:8787"}
	bad := []string{"", "--foo", "key:value", "host:", "/tmp/sock", "noport"}
	for _, s := range ok {
		if !looksLikeListenAddr(s) {
			t.Errorf("looksLikeListenAddr(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if looksLikeListenAddr(s) {
			t.Errorf("looksLikeListenAddr(%q) = true, want false", s)
		}
	}
}

func TestAddrIsLoopback(t *testing.T) {
	loop := []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"}
	notLoop := []string{":8787", "0.0.0.0:8787", "192.168.1.5:8787", "bogus"}
	for _, s := range loop {
		if !addrIsLoopback(s) {
			t.Errorf("addrIsLoopback(%q) = false, want true", s)
		}
	}
	for _, s := range notLoop {
		if addrIsLoopback(s) {
			t.Errorf("addrIsLoopback(%q) = true, want false", s)
		}
	}
}

func TestHTTPConfigEnv(t *testing.T) {
	t.Setenv("TREEMAN_MCP_HTTP_PATH", "")

	t.Run("addr from env", func(t *testing.T) {
		t.Setenv("TREEMAN_MCP_HTTP_ADDR", "127.0.0.1:7000")
		if addr, _ := HTTPConfig(nil); addr != "127.0.0.1:7000" {
			t.Fatalf("env addr not honored: %q", addr)
		}
	})

	t.Run("truthy TREEMAN_MCP_HTTP", func(t *testing.T) {
		t.Setenv("TREEMAN_MCP_HTTP_ADDR", "")
		t.Setenv("TREEMAN_MCP_HTTP", "true")
		if addr, _ := HTTPConfig(nil); addr != defaultHTTPAddr {
			t.Fatalf("truthy env should enable default addr, got %q", addr)
		}
	})

	t.Run("flag overrides env path", func(t *testing.T) {
		t.Setenv("TREEMAN_MCP_HTTP_PATH", "/from-env")
		_, path := HTTPConfig([]string{"--http", "--http-path=/from-flag"})
		if path != "/from-flag" {
			t.Fatalf("flag should override env path, got %q", path)
		}
	})
}
