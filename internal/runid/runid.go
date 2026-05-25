// Package runid generates short correlation IDs and threads them
// through context so every event emitted by a single prepare /
// finalize / teardown / hook-phase invocation carries the same
// run_id. Without this, concurrent flows interleave their events
// in the SQLite events table with no way to pull them apart.
//
// Usage:
//
//	ctx = runid.With(ctx, runid.New())
//	// every downstream store.WriteEvent picks up the id via
//	// runid.From(ctx) and stamps it into the payload.
package runid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type ctxKey struct{}

// New returns a fresh 8-character hex ID. 32 bits of entropy is
// plenty for correlating a single user's concurrent flows — not
// meant to be globally unique across machines.
func New() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// With returns a child context that carries id. Subsequent store
// writes pick it up via From.
func With(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the run_id stored on ctx, or "" if none was set.
func From(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}
