//! Parse `DATABASE_URL`-style connection strings.
//!
//! Supported schemes (with their conventional aliases):
//!   * `mysql://`            (also `mariadb`, `tidb`)
//!   * `postgres://`         (`postgresql`, `pg`, `cockroachdb`)
//!   * `mongodb://`          (`mongodb+srv`)
//!   * `redis://`            (`rediss`)
//!   * `elasticsearch://`    (`http`/`https` when host hints at ES; user opts in)
//!   * `clickhouse://`
//!   * `meilisearch://`      (`meili`)
//!   * `typesense://`
//!   * `qdrant://`           (also `qdrants` for TLS)
//!   * `weaviate://`
//!   * `milvus://`
//!   * `neo4j://`            (`bolt`, `bolt+s`, `neo4j+s`)
//!   * `influxdb://`
//!   * `kafka://`
//!   * `nats://`
//!   * `amqp://`             (RabbitMQ)
//!   * `etcd://`
//!   * `memcached://`
//!   * `s3://`               (`minio`)
//!   * `duckdb://`           (file path embedded; e.g. `duckdb:///tmp/x.db`)
//!   * `sqlite://`           (`file:`)

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Engine {
    Mysql,
    Mariadb,
    Tidb,
    Postgres,
    Cockroach,
    Mongodb,
    Redis,
    Elasticsearch,
    Opensearch,
    Clickhouse,
    Meilisearch,
    Typesense,
    Qdrant,
    Weaviate,
    Milvus,
    Neo4j,
    Influxdb,
    Kafka,
    Nats,
    Rabbitmq,
    Etcd,
    Memcached,
    S3,
    Duckdb,
    Sqlite,
}

impl Engine {
    pub fn as_str(&self) -> &'static str {
        match self {
            Engine::Mysql => "mysql",
            Engine::Mariadb => "mariadb",
            Engine::Tidb => "tidb",
            Engine::Postgres => "postgres",
            Engine::Cockroach => "cockroach",
            Engine::Mongodb => "mongodb",
            Engine::Redis => "redis",
            Engine::Elasticsearch => "elasticsearch",
            Engine::Opensearch => "opensearch",
            Engine::Clickhouse => "clickhouse",
            Engine::Meilisearch => "meilisearch",
            Engine::Typesense => "typesense",
            Engine::Qdrant => "qdrant",
            Engine::Weaviate => "weaviate",
            Engine::Milvus => "milvus",
            Engine::Neo4j => "neo4j",
            Engine::Influxdb => "influxdb",
            Engine::Kafka => "kafka",
            Engine::Nats => "nats",
            Engine::Rabbitmq => "rabbitmq",
            Engine::Etcd => "etcd",
            Engine::Memcached => "memcached",
            Engine::S3 => "s3",
            Engine::Duckdb => "duckdb",
            Engine::Sqlite => "sqlite",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DbUrl {
    pub engine: Engine,
    pub host: Option<String>,
    pub port: Option<u16>,
    pub user: Option<String>,
    pub password: Option<String>,
    pub database: Option<String>,
    /// Anything after `?`: `?sslmode=require&pool_max=8`.
    pub params: Vec<(String, String)>,
    /// Raw URL the caller passed in.
    pub raw: String,
}

#[derive(Debug, thiserror::Error)]
pub enum ParseError {
    #[error("missing scheme in url: {0}")]
    NoScheme(String),
    #[error("unknown scheme: {0}")]
    UnknownScheme(String),
    #[error("invalid port: {0}")]
    BadPort(String),
}

pub fn parse(url: &str) -> Result<DbUrl, ParseError> {
    let raw = url.to_string();
    // JDBC-style URLs (`jdbc:mysql://h:3306/db`) are Spring Boot's
    // canonical form. Strip the `jdbc:` prefix so the rest parses as
    // a normal driver URL.
    let url = url.strip_prefix("jdbc:").unwrap_or(url);
    let (scheme, rest) = url
        .split_once("://")
        .ok_or_else(|| ParseError::NoScheme(url.into()))?;

    // userinfo + authority split.
    let (authority, path_and_query) = match rest.find('/') {
        Some(idx) => (&rest[..idx], &rest[idx + 1..]),
        None => (rest, ""),
    };
    let (userinfo, hostport) = match authority.find('@') {
        Some(i) => (Some(&authority[..i]), &authority[i + 1..]),
        None => (None, authority),
    };
    let (user, password) = match userinfo {
        Some(u) => match u.find(':') {
            Some(i) => (
                Some(percent_decode(&u[..i])),
                Some(percent_decode(&u[i + 1..])),
            ),
            None => (Some(percent_decode(u)), None),
        },
        None => (None, None),
    };

    // host[:port]; also handle bracketed IPv6 `[::1]:5432`.
    let (host, port) = if let Some(stripped) = hostport.strip_prefix('[') {
        if let Some(end) = stripped.find(']') {
            let host = &stripped[..end];
            let after = &stripped[end + 1..];
            let port = after.strip_prefix(':').map(parse_port).transpose()?;
            (Some(host.to_string()), port)
        } else {
            (Some(stripped.to_string()), None)
        }
    } else {
        match hostport.rsplit_once(':') {
            Some((h, p)) if !p.is_empty() => (Some(h.to_string()), Some(parse_port(p)?)),
            _ if !hostport.is_empty() => (Some(hostport.to_string()), None),
            _ => (None, None),
        }
    };

    let (path, query) = match path_and_query.find('?') {
        Some(i) => (&path_and_query[..i], &path_and_query[i + 1..]),
        None => (path_and_query, ""),
    };
    let database = if path.is_empty() {
        None
    } else {
        Some(percent_decode(path))
    };

    let mut params = Vec::new();
    for kv in query.split('&').filter(|p| !p.is_empty()) {
        let (k, v) = kv.split_once('=').unwrap_or((kv, ""));
        params.push((percent_decode(k), percent_decode(v)));
    }

    let engine =
        engine_from_scheme(scheme).ok_or_else(|| ParseError::UnknownScheme(scheme.into()))?;

    Ok(DbUrl {
        engine,
        host: host.filter(|h| !h.is_empty()),
        port,
        user,
        password,
        database,
        params,
        raw,
    })
}

fn parse_port(s: &str) -> Result<u16, ParseError> {
    s.parse::<u16>().map_err(|_| ParseError::BadPort(s.into()))
}

fn engine_from_scheme(scheme: &str) -> Option<Engine> {
    Some(match scheme {
        "mysql" => Engine::Mysql,
        "mariadb" => Engine::Mariadb,
        "tidb" => Engine::Tidb,
        "postgres" | "postgresql" | "pg" => Engine::Postgres,
        "cockroachdb" | "cockroach" => Engine::Cockroach,
        "mongodb" | "mongodb+srv" => Engine::Mongodb,
        "redis" | "rediss" => Engine::Redis,
        "elasticsearch" | "elastic" | "es" => Engine::Elasticsearch,
        "opensearch" => Engine::Opensearch,
        "clickhouse" => Engine::Clickhouse,
        "meili" | "meilisearch" => Engine::Meilisearch,
        "typesense" => Engine::Typesense,
        "qdrant" | "qdrants" => Engine::Qdrant,
        "weaviate" => Engine::Weaviate,
        "milvus" => Engine::Milvus,
        "neo4j" | "neo4j+s" | "bolt" | "bolt+s" => Engine::Neo4j,
        "influxdb" | "influx" => Engine::Influxdb,
        "kafka" => Engine::Kafka,
        "nats" => Engine::Nats,
        "amqp" | "amqps" | "rabbitmq" => Engine::Rabbitmq,
        "etcd" => Engine::Etcd,
        "memcached" | "memcache" => Engine::Memcached,
        "s3" | "minio" => Engine::S3,
        "duckdb" => Engine::Duckdb,
        "sqlite" | "file" => Engine::Sqlite,
        _ => return None,
    })
}

fn percent_decode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    let bytes = s.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let (Some(h), Some(l)) = (hex_nibble(bytes[i + 1]), hex_nibble(bytes[i + 2])) {
                out.push(((h << 4) | l) as char);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i] as char);
        i += 1;
    }
    out
}

fn hex_nibble(b: u8) -> Option<u8> {
    match b {
        b'0'..=b'9' => Some(b - b'0'),
        b'a'..=b'f' => Some(b - b'a' + 10),
        b'A'..=b'F' => Some(b - b'A' + 10),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mysql_full() {
        let u = parse("mysql://root:pw@127.0.0.1:3306/myapp?ssl=true").unwrap();
        assert_eq!(u.engine, Engine::Mysql);
        assert_eq!(u.host.as_deref(), Some("127.0.0.1"));
        assert_eq!(u.port, Some(3306));
        assert_eq!(u.user.as_deref(), Some("root"));
        assert_eq!(u.password.as_deref(), Some("pw"));
        assert_eq!(u.database.as_deref(), Some("myapp"));
        assert_eq!(u.params, vec![("ssl".into(), "true".into())]);
    }

    #[test]
    fn pg_aliases() {
        for s in ["postgres", "postgresql", "pg"] {
            let u = parse(&format!("{s}://x:y@h/d")).unwrap();
            assert_eq!(u.engine, Engine::Postgres);
        }
    }

    #[test]
    fn cockroach_alias() {
        let u = parse("cockroachdb://x@h:26257/d").unwrap();
        assert_eq!(u.engine, Engine::Cockroach);
    }

    #[test]
    fn ipv6() {
        let u = parse("postgres://u@[::1]:5432/d").unwrap();
        assert_eq!(u.host.as_deref(), Some("::1"));
        assert_eq!(u.port, Some(5432));
    }

    #[test]
    fn percent_decoded_password() {
        let u = parse("mysql://u:p%40ss@h/d").unwrap();
        assert_eq!(u.password.as_deref(), Some("p@ss"));
    }

    #[test]
    fn neo4j_bolt() {
        let u = parse("bolt+s://neo4j:secret@h:7687/").unwrap();
        assert_eq!(u.engine, Engine::Neo4j);
    }

    #[test]
    fn duckdb_file() {
        let u = parse("duckdb:///tmp/x.db").unwrap();
        assert_eq!(u.engine, Engine::Duckdb);
        assert_eq!(u.database.as_deref(), Some("tmp/x.db"));
    }
}
