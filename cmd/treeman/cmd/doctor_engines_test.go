package cmd

import (
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

func TestWorseStatus(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"ok", "ok", "ok"},
		{"ok", "warn", "warn"},
		{"warn", "ok", "warn"},
		{"warn", "fail", "fail"},
		{"fail", "warn", "fail"},
	}
	for _, c := range cases {
		if got := worseStatus(c.a, c.b); got != c.want {
			t.Errorf("worseStatus(%q,%q)=%q want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestEngineProbes(t *testing.T) {
	cfg := &config.Config{}
	cfg.Connections.Mysql = &config.MysqlConn{} // defaults 127.0.0.1:3306
	cfg.Connections.Mongodb = &config.MongoConn{URI: "mongodb://db:27017"}
	cfg.Connections.Redis = &config.RedisConn{URL: "redis://cache:6379"}

	probes := engineProbes(cfg)
	if len(probes) != 3 {
		t.Fatalf("want 3 probes, got %d", len(probes))
	}
	byName := map[string]engineProbe{}
	for _, p := range probes {
		byName[p.name] = p
	}
	if p := byName["mysql"]; p.host != "127.0.0.1" || p.port != 3306 {
		t.Errorf("mysql defaults wrong: %+v", p)
	}
	if p := byName["mongodb"]; p.url != "mongodb://db:27017" {
		t.Errorf("mongo url wrong: %+v", p)
	}
	if p := byName["redis"]; p.url != "redis://cache:6379" {
		t.Errorf("redis url wrong: %+v", p)
	}
}
