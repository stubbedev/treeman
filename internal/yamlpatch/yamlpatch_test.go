package yamlpatch

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParsePath(t *testing.T) {
	cases := []struct {
		in   string
		want []Segment
	}{
		{"daemon", []Segment{{Key: "daemon"}}},
		{"daemon.gc_interval", []Segment{{Key: "daemon"}, {Key: "gc_interval"}}},
		{"databases[0]", []Segment{{Key: "databases"}, {IsIndex: true, Idx: 0}}},
		{"databases[0].engine", []Segment{{Key: "databases"}, {IsIndex: true, Idx: 0}, {Key: "engine"}}},
		{"x[0][1]", []Segment{{Key: "x"}, {IsIndex: true, Idx: 0}, {IsIndex: true, Idx: 1}}},
	}
	for _, c := range cases {
		got, err := ParsePath(c.in)
		if err != nil {
			t.Errorf("ParsePath(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParsePath(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestParsePathErrors(t *testing.T) {
	for _, in := range []string{"", ".", "a.", "a[", "a[x]"} {
		if _, err := ParsePath(in); err == nil {
			t.Errorf("ParsePath(%q): want error, got nil", in)
		}
	}
}

func TestSetMappingReplace(t *testing.T) {
	src := []byte("daemon:\n  gc_interval: 10\n  port: 9999\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	newVal, _ := ValueToNode(30)
	segs, _ := ParsePath("daemon.gc_interval")
	prev, err := Set(&doc, segs, newVal)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || prev.Value != "10" {
		t.Errorf("prev = %+v, want value=10", prev)
	}
	out, _ := Marshal(&doc)
	if !strings.Contains(string(out), "gc_interval: 30") {
		t.Errorf("missing replacement in output:\n%s", out)
	}
	if !strings.Contains(string(out), "port: 9999") {
		t.Errorf("port key dropped:\n%s", out)
	}
}

func TestSetCreateMissingMapping(t *testing.T) {
	src := []byte("repo:\n  name: foo\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	newVal, _ := ValueToNode(5)
	segs, _ := ParsePath("daemon.gc_interval")
	if _, err := Set(&doc, segs, newVal); err != nil {
		t.Fatal(err)
	}
	out, _ := Marshal(&doc)
	if !strings.Contains(string(out), "gc_interval: 5") {
		t.Errorf("missing new key in output:\n%s", out)
	}
}

func TestSetSequenceIndex(t *testing.T) {
	src := []byte("databases:\n  - engine: mysql\n  - engine: postgres\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	newVal, _ := ValueToNode("mariadb")
	segs, _ := ParsePath("databases[0].engine")
	prev, err := Set(&doc, segs, newVal)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || prev.Value != "mysql" {
		t.Errorf("prev = %+v, want value=mysql", prev)
	}
	out, _ := Marshal(&doc)
	if !strings.Contains(string(out), "engine: mariadb") {
		t.Errorf("replacement missing:\n%s", out)
	}
	if !strings.Contains(string(out), "engine: postgres") {
		t.Errorf("sibling lost:\n%s", out)
	}
}

func TestSetSequenceOutOfRange(t *testing.T) {
	src := []byte("databases:\n  - engine: mysql\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	newVal, _ := ValueToNode("x")
	segs, _ := ParsePath("databases[5].engine")
	if _, err := Set(&doc, segs, newVal); err == nil {
		t.Error("want out-of-range error")
	}
}

func TestSetPreservesComments(t *testing.T) {
	src := []byte("# header\nrepo:\n  name: foo  # trailing\ndaemon:\n  gc_interval: 10\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	newVal, _ := ValueToNode(30)
	segs, _ := ParsePath("daemon.gc_interval")
	if _, err := Set(&doc, segs, newVal); err != nil {
		t.Fatal(err)
	}
	out, _ := Marshal(&doc)
	if !strings.Contains(string(out), "# header") {
		t.Errorf("header comment lost:\n%s", out)
	}
	if !strings.Contains(string(out), "# trailing") {
		t.Errorf("trailing comment lost:\n%s", out)
	}
}
