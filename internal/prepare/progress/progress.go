// Package progress carries a liveness reporter on a context. Work that
// can run for a long time without returning (a finalize's hooks, a
// prepare's migrate and dump-load phases, an Elasticsearch shard
// recovery) reports every observable step forward through Report; the
// one watching the work for stalls (the daemon's finalize lease)
// installs the reporter with With.
package progress

import "context"

type reporterKey struct{}

// With returns ctx carrying report: Report under the returned context
// calls it. report must be safe for concurrent use.
func With(ctx context.Context, report func()) context.Context {
	return context.WithValue(ctx, reporterKey{}, report)
}

// Report tells the reporter on ctx that the work made observable
// progress. Work nobody watches carries no reporter, and Report does
// nothing.
func Report(ctx context.Context) {
	if report, ok := ctx.Value(reporterKey{}).(func()); ok {
		report()
	}
}
