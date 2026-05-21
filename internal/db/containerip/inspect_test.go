package containerip

import (
	"encoding/json"
	"strings"
	"testing"
)

const sampleInspect = `[{
  "Config": {"Env": ["MYSQL_ROOT_PASSWORD=secret", "PATH=/usr/bin"]},
  "NetworkSettings": {
    "IPAddress": "",
    "Networks": {
      "bridge":      {"IPAddress": "172.17.0.5"},
      "myapp_net":   {"IPAddress": "172.18.0.7"},
      "extra_net":   {"IPAddress": "172.19.0.3"}
    },
    "Ports": {
      "3306/tcp": [
        {"HostIp": "127.0.0.1", "HostPort": "33060"},
        {"HostIp": "0.0.0.0",   "HostPort": "3307"}
      ],
      "33060/tcp": null
    }
  }
}]`

func mustInspect(t *testing.T) *inspectInfo {
	t.Helper()
	var arr []inspectInfo
	if err := json.Unmarshal([]byte(sampleInspect), &arr); err != nil {
		t.Fatal(err)
	}
	return &arr[0]
}

func TestPickIPPrefersNamedNetwork(t *testing.T) {
	info := mustInspect(t)
	ip, err := info.pickIP("myapp_net")
	if err != nil || ip != "172.18.0.7" {
		t.Errorf("ip=%s err=%v", ip, err)
	}
}

func TestPickIPDeterministicOrderSkipsBridge(t *testing.T) {
	info := mustInspect(t)
	// Run many times to catch nondeterministic map iteration regressions.
	for i := 0; i < 100; i++ {
		ip, err := info.pickIP("")
		if err != nil {
			t.Fatal(err)
		}
		// extra_net sorts before myapp_net; bridge is pushed last.
		if ip != "172.19.0.3" {
			t.Fatalf("ip=%s want 172.19.0.3 (non-bridge alphabetical first)", ip)
		}
	}
}

func TestPickIPNamedNetworkMissing(t *testing.T) {
	info := mustInspect(t)
	if _, err := info.pickIP("nope_net"); err == nil {
		t.Fatal("want error for missing network")
	}
}

func TestPublishedHostPortPrefersPublicBinding(t *testing.T) {
	info := mustInspect(t)
	p, ok := info.publishedHostPort(3306)
	if !ok || p != 3307 {
		t.Errorf("port=%d ok=%v — want 3307 (0.0.0.0 binding wins over 127.0.0.1)", p, ok)
	}
}

func TestPublishedHostPortFallsBackToLoopback(t *testing.T) {
	raw := strings.Replace(sampleInspect, `"HostIp": "0.0.0.0",   "HostPort": "3307"`, `"HostIp": "127.0.0.1", "HostPort": "33099"`, 1)
	var arr []inspectInfo
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		t.Fatal(err)
	}
	p, ok := arr[0].publishedHostPort(3306)
	if !ok || (p != 33060 && p != 33099) {
		t.Errorf("port=%d ok=%v — want a loopback binding", p, ok)
	}
}

func TestPublishedHostPortMissing(t *testing.T) {
	info := mustInspect(t)
	if _, ok := info.publishedHostPort(9999); ok {
		t.Error("want false for unmapped port")
	}
}

func TestURIPort(t *testing.T) {
	cases := []struct {
		uri  string
		def  uint16
		want uint16
	}{
		{"mongodb://localhost:12345/db", 27017, 12345},
		{"mongodb://localhost/db", 27017, 27017},
		{"redis://h:6390", 6379, 6390},
		{"not a uri", 1234, 1234},
		{"http://u:p@host:8080/p", 80, 8080},
	}
	for _, c := range cases {
		got := URIPort(c.uri, c.def)
		if got != c.want {
			t.Errorf("URIPort(%q,%d)=%d want %d", c.uri, c.def, got, c.want)
		}
	}
}

func TestRewriteHostPortInURIWithPort(t *testing.T) {
	cases := []struct {
		uri, newHost string
		newPort      uint16
		want         string
	}{
		{"mongodb://u:p@old:27017/db", "127.0.0.1", 27100, "mongodb://u:p@127.0.0.1:27100/db"},
		// newPort 0 → keep existing port.
		{"mongodb://old:27017/db", "127.0.0.1", 0, "mongodb://127.0.0.1:27017/db"},
		// no port in URI; newPort 0 → no port emitted.
		{"http://h/p", "1.2.3.4", 0, "http://1.2.3.4/p"},
		// no port in URI; newPort set → port added.
		{"http://h/p", "1.2.3.4", 9200, "http://1.2.3.4:9200/p"},
	}
	for _, c := range cases {
		got := RewriteHostPortInURIWithPort(c.uri, c.newHost, c.newPort)
		if got != c.want {
			t.Errorf("Rewrite(%q,%q,%d)=%q want %q", c.uri, c.newHost, c.newPort, got, c.want)
		}
	}
}
