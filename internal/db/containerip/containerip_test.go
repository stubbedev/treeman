package containerip

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

func TestRewriteHostPortInURI(t *testing.T) {
	cases := []struct {
		uri, newHost, want string
	}{
		{"mongodb://user:pw@old:27017/db", "1.2.3.4", "mongodb://user:pw@1.2.3.4:27017/db"},
		{"redis://localhost:6379/0", "172.17.0.2", "redis://172.17.0.2:6379/0"},
		{"http://elasticsearch:9200", "10.0.0.5", "http://10.0.0.5:9200"},
		{"mongodb://a/db", "host2", "mongodb://host2/db"},
		// userinfo with @ but slash before @ in path — must not be
		// treated as userinfo.
		{"http://h/path@x", "n", "http://n/path@x"},
	}
	for _, c := range cases {
		got := RewriteHostPortInURI(c.uri, c.newHost)
		if got != c.want {
			t.Errorf("RewriteHostPortInURI(%q, %q)=%q want %q", c.uri, c.newHost, got, c.want)
		}
	}
}

func TestRewriteHostPortInURIHostForms(t *testing.T) {
	cases := []struct {
		name    string
		uri     string
		newHost string
		newPort uint16
		want    string
	}{
		{"ipv4 to ipv4 keeps port", "mysql://u:p@10.0.0.1:3306/db", "10.0.0.2", 0, "mysql://u:p@10.0.0.2:3306/db"},
		{"ipv4 without port", "http://10.0.0.1/p", "10.0.0.2", 0, "http://10.0.0.2/p"},
		{"hostname to hostname with new port", "redis://cache:6379/0", "db", 6380, "redis://db:6380/0"},
		{"hostname without port", "mongodb://mongo/db", "db", 0, "mongodb://db/db"},
		{"ipv6 host keeps port", "mysql://u:p@db:3306/db", "fd00::2", 0, "mysql://u:p@[fd00::2]:3306/db"},
		{"ipv6 host with new port", "mysql://db:3306/db", "fd00::2", 3307, "mysql://[fd00::2]:3307/db"},
		{"ipv6 host without port", "http://es/p", "fd00::2", 0, "http://[fd00::2]/p"},
		{"ipv6 host adds port", "http://es/p", "fd00::2", 9200, "http://[fd00::2]:9200/p"},
		{"bracketed source keeps port", "redis://[::1]:6379", "10.0.0.1", 0, "redis://10.0.0.1:6379"},
		{"bracketed source without port", "redis://[::1]/0", "10.0.0.1", 0, "redis://10.0.0.1/0"},
		{"bracketed source to ipv6", "redis://u@[::1]:6379?x=1", "fd00::5", 0, "redis://u@[fd00::5]:6379?x=1"},
		{"bracketed source without port to ipv6", "redis://[::1]", "::1", 0, "redis://[::1]"},
		{"unescaped @ in password", "mysql://u:p@ss@db:3306/db", "10.0.0.2", 0, "mysql://u:p@ss@10.0.0.2:3306/db"},
		{"@ in query after host", "mongodb://db:27017/x?authSource=a@b", "10.0.0.2", 0, "mongodb://10.0.0.2:27017/x?authSource=a@b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RewriteHostPortInURIWithPort(c.uri, c.newHost, c.newPort); got != c.want {
				t.Errorf("RewriteHostPortInURIWithPort(%q, %q, %d)=%q want %q", c.uri, c.newHost, c.newPort, got, c.want)
			}
		})
	}
}

func TestURIPortHostForms(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want uint16
	}{
		{"ipv4 with port", "mysql://10.0.0.1:3307/db", 3307},
		{"ipv4 without port", "mysql://10.0.0.1/db", 3306},
		{"hostname with port", "mysql://u:p@db:3308", 3308},
		{"unescaped @ in password", "mysql://u:p@ss@db:3310/db", 3310},
		{"hostname without port", "mysql://db", 3306},
		{"bracketed ipv6 with port", "mysql://u:p@[fd00::2]:3309/db", 3309},
		{"bracketed ipv6 without port", "mysql://[fd00::2]/db", 3306},
		{"bracketed loopback without port", "mysql://[::1]", 3306},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := URIPort(c.uri, 3306); got != c.want {
				t.Errorf("URIPort(%q, 3306)=%d want %d", c.uri, got, c.want)
			}
		})
	}
}

func TestRewritePreservesUnknownSchemes(t *testing.T) {
	// No `://` → return as-is so non-URI strings stay untouched.
	if got := RewriteHostPortInURI("not-a-uri", "x"); got != "not-a-uri" {
		t.Errorf("got %q", got)
	}
}

func TestResolveEmptyContainerReturnsEmpty(t *testing.T) {
	ip, err := Resolve(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "" {
		t.Errorf("ip=%q want empty", ip)
	}
}

// container_engine: an unset engine resolves to docker; an explicit one
// is passed through verbatim. This is the per-engine `container_engine`
// option's resolution, shared identically by all five DB drivers.
func TestFromRefCarriesEveryContainerRefField(t *testing.T) {
	ref := config.ContainerRef{
		Container:       "db",
		ComposeService:  "mysql",
		ComposeProject:  "app",
		ContainerEngine: "podman",
		Network:         "backend",
	}
	want := Opts{
		Container:      "db",
		ComposeService: "mysql",
		ComposeProject: "app",
		Engine:         "podman",
		Network:        "backend",
		InternalPort:   3306,
	}
	if got := FromRef(ref, 3306); !reflect.DeepEqual(got, want) {
		t.Errorf("FromRef(%+v, 3306) = %+v, want %+v", ref, got, want)
	}
	if n := reflect.TypeFor[config.ContainerRef]().NumField(); n != 5 {
		t.Errorf("config.ContainerRef has %d fields; FromRef maps 5, so map the new one", n)
	}
}

func TestEngineBinDefaultsToDocker(t *testing.T) {
	if got := (Opts{}).EngineBin(); got != "docker" {
		t.Errorf("unset engine EngineBin()=%q want docker", got)
	}
	if got := (Opts{Engine: "podman"}).EngineBin(); got != "podman" {
		t.Errorf("explicit engine EngineBin()=%q want podman", got)
	}
}

// container_engine actually selects the CLI binary invoked. Point it at
// a binary that cannot exist and assert the error names it — proves the
// field is honored without needing podman/nerdctl installed.
func TestContainerEngineSelectsBinary(t *testing.T) {
	_, err := ContainerID(context.Background(), Opts{ComposeService: "svc", Engine: "tm-bogus-engine-xyz"})
	if err == nil {
		t.Fatal("expected error for non-existent engine binary")
	}
	if !strings.Contains(err.Error(), "tm-bogus-engine-xyz") {
		t.Errorf("error %q should name the configured container_engine", err)
	}
}

// compose_project: explicit value is used in the compose label filter;
// when unset it falls back to $COMPOSE_PROJECT_NAME. The resolved
// project appears in the lookup error (built before exec), so a bogus
// engine lets us assert the resolution order without Docker.
func TestComposeProjectResolution(t *testing.T) {
	const eng = "tm-bogus-engine-xyz"

	mustErrWithProject := func(opts Opts, wantProject string) {
		t.Helper()
		_, err := ContainerID(context.Background(), opts)
		if err == nil {
			t.Fatalf("expected error (bogus engine) for %+v", opts)
		}
		if !strings.Contains(err.Error(), "project="+wantProject) {
			t.Errorf("error %q should report project=%q", err, wantProject)
		}
	}

	// Explicit project is used as-is.
	mustErrWithProject(Opts{ComposeService: "svc", ComposeProject: "explicitproj", Engine: eng}, "explicitproj")

	// Unset project falls back to the compose env var.
	t.Setenv("COMPOSE_PROJECT_NAME", "envproj")
	mustErrWithProject(Opts{ComposeService: "svc", Engine: eng}, "envproj")

	// Explicit still wins over the env var.
	mustErrWithProject(Opts{ComposeService: "svc", ComposeProject: "explicitproj", Engine: eng}, "explicitproj")
}
