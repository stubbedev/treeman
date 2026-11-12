package redis

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/fakeserver"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// replyWith answers every command on the connection with reply.
func replyWith(reply string) func(net.Conn) {
	return func(c net.Conn) {
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if len(line) > 0 && line[0] != '*' {
				continue
			}
			var n int
			if _, err := fmt.Sscanf(line, "*%d", &n); err != nil {
				return
			}
			for range 2 * n {
				if _, err := r.ReadString('\n'); err != nil {
					return
				}
			}
			if _, err := c.Write([]byte(reply)); err != nil {
				return
			}
		}
	}
}

// Regression for #212: a redis that drops the connection or is still
// loading its dataset is an engine-unreachable failure; one that
// rejects the client is not.
func TestConnectClassifiesPingFailures(t *testing.T) {
	cases := []struct {
		name   string
		handle func(net.Conn)
		reason reachability.Reason
	}{
		{"connection dropped", fakeserver.Close, reachability.HandshakeDropped},
		{"loading the dataset", replyWith("-LOADING Redis is loading the dataset in memory\r\n"), reachability.Starting},
		{"authentication required", replyWith("-NOAUTH Authentication required.\r\n"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port := fakeserver.Start(t, tc.handle)
			url := "redis://" + net.JoinHostPort(host, strconv.Itoa(int(port))) + "/0?max_retries=-1&protocol=2"
			_, err := Connect(t.Context(), config.RedisConn{URL: url})
			if err == nil {
				t.Fatal("Connect succeeded against a server that never answers PING")
			}
			ue, unreachable := errors.AsType[*reachability.UnreachableError](err)
			if tc.reason == 0 {
				if unreachable {
					t.Errorf("Connect = %v, want a failure the engine reported, not engine-unreachable", err)
				}
				return
			}
			if !unreachable || ue.Reason != tc.reason {
				t.Errorf("Connect = %v, want an *UnreachableError with reason %s", err, tc.reason)
			}
		})
	}
}
