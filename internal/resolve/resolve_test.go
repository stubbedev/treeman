package resolve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/envfile"
)

func TestYamlWins(t *testing.T) {
	cfg := &config.Config{}
	cfg.Connections.Mysql = &config.MysqlConn{Host: "h", Port: 3306, User: "u", PoolMax: 8}
	r := Resolve(cfg, "")
	if r.Mysql == nil {
		t.Fatal("nil")
	}
	if r.Mysql.Source.Kind != SourceYaml {
		t.Errorf("source: %v", r.Mysql.Source)
	}
}

func TestLaravelDbTestOverridesWin(t *testing.T) {
	env := envfile.Parse("DB_HOST=base\nDB_TEST_HOST=test-host\nDB_TEST_PORT=3307\n")
	cfg := &config.Config{}
	r := resolveMysql(cfg, env)
	if r == nil {
		t.Fatal("nil")
		return
	}
	if r.Conn.Host != "test-host" || r.Conn.Port != 3307 {
		t.Errorf("got host=%s port=%d", r.Conn.Host, r.Conn.Port)
	}
}

func TestYamlMysqlPicksUpPasswordFromEnvFile(t *testing.T) {
	cfg := &config.Config{}
	cfg.Connections.Mysql = &config.MysqlConn{
		Host:     "127.0.0.1",
		Port:     3306,
		User:     "myapp",
		Password: "$DB_TEST_PASSWORD",
		PoolMax:  8,
	}
	env := envfile.Parse("DB_TEST_PASSWORD=secret-pw\n")
	r := resolveMysql(cfg, env)
	if r.Conn.Password != "secret-pw" {
		t.Errorf("password: %q", r.Conn.Password)
	}
}

func TestSpringBootJDBCMysqlURL(t *testing.T) {
	env := envfile.Parse(
		"SPRING_DATASOURCE_URL=jdbc:mysql://db.internal:3307/app\n" +
			"SPRING_DATASOURCE_USERNAME=spring\n" +
			"SPRING_DATASOURCE_PASSWORD=letmein\n",
	)
	r := resolveMysql(&config.Config{}, env)
	if r == nil {
		t.Fatal("nil")
		return
	}
	if r.Conn.Host != "db.internal" || r.Conn.Port != 3307 || r.Conn.User != "spring" || r.Conn.Password != "letmein" {
		t.Errorf("got %+v", r.Conn)
	}
}

func TestSpringBootJDBCPostgresURL(t *testing.T) {
	env := envfile.Parse(
		"SPRING_DATASOURCE_URL=jdbc:postgresql://pg.internal/app\n" +
			"SPRING_DATASOURCE_USERNAME=pg\n" +
			"SPRING_DATASOURCE_PASSWORD=pgsecret\n",
	)
	r := resolvePostgres(&config.Config{}, env)
	if r == nil {
		t.Fatal("nil")
		return
	}
	if r.Conn.Host != "pg.internal" || r.Conn.User != "pg" || r.Conn.Password != "pgsecret" {
		t.Errorf("got %+v", r.Conn)
	}
}

func TestSpringBootMongoComposed(t *testing.T) {
	env := envfile.Parse(
		"SPRING_DATA_MONGODB_HOST=mh\n" +
			"SPRING_DATA_MONGODB_PORT=27020\n" +
			"SPRING_DATA_MONGODB_USERNAME=u\n" +
			"SPRING_DATA_MONGODB_PASSWORD=p\n" +
			"SPRING_DATA_MONGODB_DATABASE=appdb\n",
	)
	r := resolveMongodb(&config.Config{}, env)
	if r.Conn.URI != "mongodb://u:p@mh:27020/appdb" {
		t.Errorf("got %s", r.Conn.URI)
	}
}

func TestRedisComposedFromComponentizedEnv(t *testing.T) {
	env := envfile.Parse("REDIS_HOST=127.0.0.1\nREDIS_PORT=6390\nREDIS_PASSWORD=hello/world\n")
	r := resolveRedis(&config.Config{}, env)
	if r.Conn.URL != "redis://:hello%2Fworld@127.0.0.1:6390" {
		t.Errorf("got %s", r.Conn.URL)
	}
}

func TestRedisSkipsLiteralNullPassword(t *testing.T) {
	env := envfile.Parse("REDIS_HOST=127.0.0.1\nREDIS_PORT=6379\nREDIS_PASSWORD=null\n")
	r := resolveRedis(&config.Config{}, env)
	if r.Conn.URL != "redis://127.0.0.1:6379" {
		t.Errorf("got %s", r.Conn.URL)
	}
}

func TestElasticsearchHostsGainsHttpScheme(t *testing.T) {
	env := envfile.Parse("ELASTICSEARCH_HOSTS=localhost:9200\n")
	r := resolveElasticsearch(&config.Config{}, env)
	if r.Conn.URL != "http://localhost:9200" {
		t.Errorf("got %s", r.Conn.URL)
	}
}

func TestMongoUriComposedFromHostPort(t *testing.T) {
	env := envfile.Parse("MONGO_DB_HOST=localhost\nMONGO_DB_PORT=27017\n")
	r := resolveMongodb(&config.Config{}, env)
	if r.Conn.URI != "mongodb://localhost:27017" {
		t.Errorf("got %s", r.Conn.URI)
	}
}

func TestNullPasswordInEnvYieldsNoPassword(t *testing.T) {
	cfg := &config.Config{}
	cfg.Connections.Mysql = &config.MysqlConn{
		Host:     "h",
		Port:     3306,
		User:     "u",
		Password: "$DB_PASSWORD",
		PoolMax:  8,
	}
	env := envfile.Parse("DB_PASSWORD=null\n")
	r := resolveMysql(cfg, env)
	if r.Conn.Password != "" {
		t.Errorf("password should be empty, got %q", r.Conn.Password)
	}
}

func TestS3FieldsPickUpEnvRefs(t *testing.T) {
	cfg := &config.Config{}
	cfg.Connections.S3 = &config.S3Conn{
		Endpoint:  "$STORAGE_ENDPOINT",
		Region:    "${STORAGE_REGION}",
		AccessKey: "$STORAGE_KEY",
		SecretKey: "$STORAGE_SECRET",
	}
	env := envfile.Parse("STORAGE_ENDPOINT=http://127.0.0.1:9000\nSTORAGE_REGION=garage\nSTORAGE_KEY=GKabc\nSTORAGE_SECRET=shh\n")
	r := resolveS3(cfg, env)
	if r.Conn.Endpoint != "http://127.0.0.1:9000" || r.Conn.Region != "garage" ||
		r.Conn.AccessKey != "GKabc" || r.Conn.SecretKey != "shh" {
		t.Errorf("got %+v", r.Conn)
	}
}

func TestS3LiteralFieldsPassThrough(t *testing.T) {
	cfg := &config.Config{}
	cfg.Connections.S3 = &config.S3Conn{
		Endpoint:  "http://127.0.0.1:9000",
		Region:    "garage",
		AccessKey: "GKabc",
		SecretKey: "shh",
	}
	r := resolveS3(cfg, envfile.Parse(""))
	if r.Conn.Endpoint != "http://127.0.0.1:9000" || r.Conn.Region != "garage" ||
		r.Conn.AccessKey != "GKabc" || r.Conn.SecretKey != "shh" {
		t.Errorf("got %+v", r.Conn)
	}
}

func TestEnvRootsWorktreeFallsBackToMain(t *testing.T) {
	main, wt := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(main, ".env"), []byte("STORAGE_SECRET=frommain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := loadRepoEnvRoots([]string{main, wt}, []string{".env"})
	if v, _ := env.Get("STORAGE_SECRET"); v != "frommain" {
		t.Errorf("missing wt .env should fall back to main, got %q", v)
	}
	if err := os.WriteFile(filepath.Join(wt, ".env"), []byte("STORAGE_SECRET=fromwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = loadRepoEnvRoots([]string{main, wt}, []string{".env"})
	if v, _ := env.Get("STORAGE_SECRET"); v != "fromwt" {
		t.Errorf("wt .env should override main, got %q", v)
	}
}

func TestYamlURIFieldsPickUpEnvRefs(t *testing.T) {
	cfg := &config.Config{}
	cfg.Connections.Mongodb = &config.MongoConn{URI: "$MONGO_REF"}
	cfg.Connections.Redis = &config.RedisConn{URL: "${REDIS_REF}"}
	cfg.Connections.Elasticsearch = &config.EsConn{URL: "$ES_REF"}
	env := envfile.Parse("MONGO_REF=mongodb://h:27017\nREDIS_REF=redis://h:6379\nES_REF=http://h:9200\n")
	if r := resolveMongodb(cfg, env); r.Conn.URI != "mongodb://h:27017" {
		t.Errorf("mongo: %q", r.Conn.URI)
	}
	if r := resolveRedis(cfg, env); r.Conn.URL != "redis://h:6379" {
		t.Errorf("redis: %q", r.Conn.URL)
	}
	if r := resolveElasticsearch(cfg, env); r.Conn.URL != "http://h:9200" {
		t.Errorf("es: %q", r.Conn.URL)
	}
}

func TestYamlURILiteralsPassThrough(t *testing.T) {
	cfg := &config.Config{}
	cfg.Connections.Mongodb = &config.MongoConn{URI: "mongodb://lit:27017"}
	if r := resolveMongodb(cfg, envfile.Parse("")); r.Conn.URI != "mongodb://lit:27017" {
		t.Errorf("mongo literal: %q", r.Conn.URI)
	}
}
