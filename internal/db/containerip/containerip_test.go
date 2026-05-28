package containerip

import (
	"strings"
	"testing"
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
		// IPv6-looking bracket (won't be touched as a host but should
		// not crash).
		{"redis://[::1]:6379", "10.0.0.1", "redis://10.0.0.1:6379"},
	}
	for _, c := range cases {
		got := RewriteHostPortInURI(c.uri, c.newHost)
		if got != c.want {
			t.Errorf("RewriteHostPortInURI(%q, %q)=%q want %q", c.uri, c.newHost, got, c.want)
		}
	}
}

func TestRewritePreservesUnknownSchemes(t *testing.T) {
	// No `://` → return as-is so non-URI strings stay untouched.
	if got := RewriteHostPortInURI("not-a-uri", "x"); got != "not-a-uri" {
		t.Errorf("got %q", got)
	}
}

func TestResolveEmptyContainerReturnsEmpty(t *testing.T) {
	ip, err := Resolve("", "")
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
func TestNormEngineDefaultsToDocker(t *testing.T) {
	if got := (Opts{}).normEngine(); got != "docker" {
		t.Errorf("unset engine normEngine()=%q want docker", got)
	}
	if got := (Opts{Engine: "podman"}).normEngine(); got != "podman" {
		t.Errorf("explicit engine normEngine()=%q want podman", got)
	}
}

// container_engine actually selects the CLI binary invoked. Point it at
// a binary that cannot exist and assert the error names it — proves the
// field is honored without needing podman/nerdctl installed.
func TestContainerEngineSelectsBinary(t *testing.T) {
	_, err := ContainerID(Opts{ComposeService: "svc", Engine: "tm-bogus-engine-xyz"})
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
		_, err := ContainerID(opts)
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
