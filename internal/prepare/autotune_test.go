package prepare

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestAutoTuneOuterMySQL walks the formula across realistic
// max_connections values. Inner cost for MySQL is 6 (mysqlCloneFanout),
// reserved budget is max(5, maxConns/10), so the expected outer is
// (maxConns - reserved) / 6 clamped to [2, fanOutLimits["mysql"]*2].
func TestAutoTuneOuterMySQL(t *testing.T) {
	cases := []struct {
		maxConn int
		want    int
	}{
		{0, fanOutLimits["mysql"]}, // probe failure → conservative default
		{30, 4},                    // (30-5)/6 = 4
		{151, 8},                   // (151-15)/6 = 22 → clamped to hardCap (4*2 = 8)
		{500, 8},                   // clamp
		{20, 2},                    // (20-5)/6 = 2
		{6, 2},                     // floor
	}
	for _, c := range cases {
		got := autoTuneOuter("mysql", c.maxConn)
		if got != c.want {
			t.Errorf("autoTuneOuter(mysql, %d) = %d, want %d", c.maxConn, got, c.want)
		}
	}
}

// TestAutoTuneOuterPostgres — inner cost is 1, so the divisor is far
// gentler. The hard ceiling (fanOutLimits["postgres"]*2 = 16) becomes
// the dominant clamp on a host with a typical 100+ max_connections.
func TestAutoTuneOuterPostgres(t *testing.T) {
	cases := []struct {
		maxConn int
		want    int
	}{
		{0, fanOutLimits["postgres"]},
		{20, 15}, // (20-5)/1 = 15
		{100, 16},
		{1000, 16},
		{7, 2}, // (7-5)/1 = 2
	}
	for _, c := range cases {
		got := autoTuneOuter("postgres", c.maxConn)
		if got != c.want {
			t.Errorf("autoTuneOuter(postgres, %d) = %d, want %d", c.maxConn, got, c.want)
		}
	}
}

// TestAutoTuneOuterUnknownEngine collapses to the safe defaults.
func TestAutoTuneOuterUnknownEngine(t *testing.T) {
	got := autoTuneOuter("clickhouse", 1000)
	if got < 2 {
		t.Errorf("unknown engine should still produce a usable cap; got %d", got)
	}
}

// TestFanOutClonesHonoursMaxConnsProbe — when maxConns is supplied
// (and no explicit override) the limit is computed by autoTuneOuter
// and the in-flight concurrency stays under it.
//
// Deterministic synchronisation: each goroutine blocks on `release`
// after recording inflight, so the limiter's cap forces serialisation
// and the test observes the real peak. The old version spin-looped
// 200k times to "linger" — under CPU pressure (parallel test packages
// on an 8-core host) goroutines could finish the spin before the next
// one started and the test reported peak=1.
func TestFanOutClonesHonoursMaxConnsProbe(t *testing.T) {
	var inflight atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})
	restore := func(ctx context.Context, template, target string) error {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		return nil
	}
	clones := make([]string, 40)
	for i := range clones {
		clones[i] = "c"
	}
	// maxConns=30, mysql inner=6, reserved=5 → outer should be 4.
	want := autoTuneOuter("mysql", 30)

	// Release once the limiter has filled to the expected cap (or a
	// safety deadline elapses). Closing the channel unblocks every
	// goroutine — both the cap-many already in-flight and the ones
	// the limiter will admit as those drain.
	deadline := time.Now().Add(5 * time.Second)
	go func() {
		for peak.Load() < int32(want) { //nolint:gosec // test cap is small and non-negative
			if time.Now().After(deadline) {
				close(release)
				return
			}
			runtime.Gosched()
		}
		close(release)
	}()

	if err := fanOutClones(context.Background(), nil, 0, 0, restore, "tmpl", clones, "mysql", 0, 30); err != nil {
		t.Fatal(err)
	}
	got := peak.Load()
	if got > int32(want) { //nolint:gosec // test cap is small and non-negative
		t.Errorf("peak %d exceeded auto-tuned cap %d", got, want)
	}
	if got < int32(want) { //nolint:gosec // test cap is small and non-negative
		t.Errorf("limiter never reached its cap: peak=%d want=%d", got, want)
	}
}

// TestFanOutClonesExplicitOverrideWinsOverProbe — when the operator
// pins databases[].fanout, the auto-tuner is bypassed entirely.
func TestFanOutClonesExplicitOverrideWinsOverProbe(t *testing.T) {
	var peak atomic.Int32
	var inflight atomic.Int32
	release := make(chan struct{})
	restore := func(ctx context.Context, template, target string) error {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		return nil
	}
	clones := make([]string, 16)
	for i := range clones {
		clones[i] = "c"
	}
	// override=3 must beat autoTuneOuter("mysql", 1000)=8.
	deadline := time.Now().Add(5 * time.Second)
	go func() {
		for peak.Load() < 3 {
			if time.Now().After(deadline) {
				close(release)
				return
			}
			runtime.Gosched()
		}
		close(release)
	}()
	if err := fanOutClones(context.Background(), nil, 0, 0, restore, "tmpl", clones, "mysql", 3, 1000); err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 3 {
		t.Errorf("explicit override=3 ignored: peak=%d", peak.Load())
	}
}
