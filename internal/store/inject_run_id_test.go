package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/stubbedev/treeman/internal/runid"
)

func TestInjectRunID_NoRunIDOnCtx_PassthroughUnchanged(t *testing.T) {
	in := map[string]string{"k": "v"}
	got := injectRunID(context.Background(), in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("payload mutated without run_id on ctx: %#v", got)
	}
}

func TestInjectRunID_NilPayloadBecomesMap(t *testing.T) {
	ctx := runid.With(context.Background(), "abc12345")
	got := injectRunID(ctx, nil)
	m, ok := got.(map[string]string)
	if !ok {
		t.Fatalf("nil payload should become map[string]string, got %T", got)
	}
	if m["run_id"] != "abc12345" {
		t.Fatalf("run_id not set: %#v", m)
	}
}

func TestInjectRunID_StringMapGetsRunIDAdded(t *testing.T) {
	ctx := runid.With(context.Background(), "abc12345")
	in := map[string]string{"engine": "mysql"}
	got := injectRunID(ctx, in)
	m, ok := got.(map[string]string)
	if !ok {
		t.Fatalf("want map[string]string, got %T", got)
	}
	if m["engine"] != "mysql" || m["run_id"] != "abc12345" {
		t.Fatalf("expected both engine + run_id; got %#v", m)
	}
}

func TestInjectRunID_AnyMapGetsRunIDAdded(t *testing.T) {
	ctx := runid.With(context.Background(), "abc12345")
	in := map[string]any{"clones": 4}
	got := injectRunID(ctx, in)
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("want map[string]any, got %T", got)
	}
	if m["clones"] != 4 || m["run_id"] != "abc12345" {
		t.Fatalf("expected both clones + run_id; got %#v", m)
	}
}

func TestInjectRunID_ExistingRunIDPreserved(t *testing.T) {
	// If a call site has already stamped its own run_id, the
	// auto-injection must not clobber it. Lets the dispatch layer
	// override what the ctx says when needed.
	ctx := runid.With(context.Background(), "fromctx0")
	in := map[string]string{"run_id": "explicit"}
	got := injectRunID(ctx, in).(map[string]string)
	if got["run_id"] != "explicit" {
		t.Fatalf("existing run_id overwritten: %#v", got)
	}
}

func TestInjectRunID_NonMapPayloadGetsWrapped(t *testing.T) {
	ctx := runid.With(context.Background(), "abc12345")
	got := injectRunID(ctx, []string{"a", "b"})
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("non-map should be wrapped, got %T", got)
	}
	if m["run_id"] != "abc12345" {
		t.Fatalf("run_id not set in wrapper: %#v", m)
	}
	if _, ok := m["payload"]; !ok {
		t.Fatalf("original payload not preserved under 'payload': %#v", m)
	}
}
