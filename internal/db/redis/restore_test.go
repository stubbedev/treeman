package redis

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestReadRESPArray_Basic decodes a hand-written RESP frame
// `*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n` — the exact byte shape
// `redis-cli --pipe` accepts — and verifies the parser returns the
// expected command tokens. Pins the wire-protocol fallback's load-
// bearing piece.
func TestReadRESPArray_Basic(t *testing.T) {
	frame := []byte("*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n")
	r := bufio.NewReader(bytes.NewReader(frame))
	parts, err := readRESPArray(r)
	if err != nil {
		t.Fatalf("readRESPArray: %v", err)
	}
	got := []string{}
	for _, p := range parts {
		got = append(got, string(p))
	}
	want := []string{"SET", "k", "v"}
	if len(got) != len(want) {
		t.Fatalf("parts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parts[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReadRESPArray_MultipleCommands walks two commands back-to-back
// from one bytes.Buffer, confirming the parser advances its bufio
// reader past each frame's trailing \r\n cleanly. Catches off-by-one
// errors in the bulk-string trailer consumption.
func TestReadRESPArray_MultipleCommands(t *testing.T) {
	frame := []byte(
		"*3\r\n$3\r\nSET\r\n$2\r\nk1\r\n$2\r\nv1\r\n" +
			"*3\r\n$3\r\nSET\r\n$2\r\nk2\r\n$2\r\nv2\r\n",
	)
	r := bufio.NewReader(bytes.NewReader(frame))

	cmd1, err := readRESPArray(r)
	if err != nil {
		t.Fatalf("first cmd: %v", err)
	}
	if string(cmd1[1]) != "k1" || string(cmd1[2]) != "v1" {
		t.Errorf("cmd1 = %q, want SET k1 v1", cmd1)
	}
	cmd2, err := readRESPArray(r)
	if err != nil {
		t.Fatalf("second cmd: %v", err)
	}
	if string(cmd2[1]) != "k2" || string(cmd2[2]) != "v2" {
		t.Errorf("cmd2 = %q, want SET k2 v2", cmd2)
	}
	// A third read should report clean EOF, not partial-frame error.
	_, err = readRESPArray(r)
	if !errors.Is(err, io.EOF) {
		t.Errorf("third read err = %v, want EOF", err)
	}
}

// TestReadRESPArray_BinarySafe ensures a bulk-string body containing
// embedded \r\n and NUL bytes is decoded by LENGTH, not by terminator.
// Critical: redis values can carry arbitrary bytes, and a delimiter-
// based parser would split them mid-blob.
func TestReadRESPArray_BinarySafe(t *testing.T) {
	body := []byte{'a', '\r', '\n', 0, 'b'}
	frame := []byte("*2\r\n$3\r\nSET\r\n$5\r\n")
	frame = append(frame, body...)
	frame = append(frame, '\r', '\n')

	r := bufio.NewReader(bytes.NewReader(frame))
	parts, err := readRESPArray(r)
	if err != nil {
		t.Fatalf("readRESPArray: %v", err)
	}
	if !bytes.Equal(parts[1], body) {
		t.Errorf("bulk body = %q, want %q", parts[1], body)
	}
}

func TestHostPortFromURL(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"redis://127.0.0.1:16379", "127.0.0.1", 16379},
		{"redis://:pw@host:6380/0", "host", 6380},
		{"redis://localhost", "localhost", 6379}, // default port
		{"", "127.0.0.1", 6379},                  // empty URL → loopback default
		{"not a url at all !!!", "127.0.0.1", 6379},
	}
	for _, c := range cases {
		gotHost, gotPort := hostPortFromURL(c.in)
		if gotHost != c.wantHost || gotPort != c.wantPort {
			t.Errorf("hostPortFromURL(%q) = (%q,%d), want (%q,%d)",
				c.in, gotHost, gotPort, c.wantHost, c.wantPort)
		}
	}
}

func TestPasswordFromURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"redis://:secret@127.0.0.1:6379", "secret"},
		{"redis://user:pw@host:6379", "pw"},
		{"redis://127.0.0.1:6379", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := passwordFromURL(c.in); got != c.want {
			t.Errorf("passwordFromURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
