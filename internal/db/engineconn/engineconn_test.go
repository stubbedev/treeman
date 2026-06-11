package engineconn

import (
	"context"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/engine"
)

// TestConfiguredMatchesConnect pins the contract callers rely on:
// Configured(fam) is true exactly when Connect would report
// configured=true — for every family, with and without a connection
// block. Connect's not-configured path must return (nil, false, nil).
func TestConfiguredMatchesConnect(t *testing.T) {
	families := []engine.Family{
		engine.FamilyMySQL, engine.FamilyPostgres, engine.FamilyMongo,
		engine.FamilyRedis, engine.FamilyES,
	}
	empty := &config.Config{}
	for _, fam := range families {
		if Configured(empty, fam) {
			t.Errorf("%s: Configured = true on empty config", fam)
		}
		conn, configured, err := Connect(context.Background(), empty, fam)
		if conn != nil || configured || err != nil {
			t.Errorf("%s: Connect on empty config = (%v, %v, %v), want (nil, false, nil)", fam, conn, configured, err)
		}
	}

	full := &config.Config{Connections: config.ConnectionsConfig{
		Mysql:         &config.MysqlConn{Host: "127.0.0.1", Port: 1, User: "u"},
		Postgres:      &config.PostgresConn{Host: "127.0.0.1", Port: 1, User: "u"},
		Mongodb:       &config.MongoConn{URI: "mongodb://127.0.0.1:1"},
		Redis:         &config.RedisConn{URL: "redis://127.0.0.1:1"},
		Elasticsearch: &config.EsConn{URL: "http://127.0.0.1:1"},
	}}
	for _, fam := range families {
		if !Configured(full, fam) {
			t.Errorf("%s: Configured = false with connection block present", fam)
		}
	}

	if Configured(full, engine.Family("nope")) {
		t.Error("unknown family must not report configured")
	}
}
