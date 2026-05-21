package containerip

import "testing"

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
