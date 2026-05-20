package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/store"
)

func setup(t *testing.T) (*State, func()) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	st := NewState(s)
	return st, func() { _ = s.Close() }
}

func TestDispatchPing(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	shut := make(chan struct{}, 1)
	resp := Dispatch(context.Background(), st, shut, rpc.Request{Method: rpc.MethodPing})
	if resp.Kind != rpc.KindPong {
		t.Errorf("kind: %s", resp.Kind)
	}
}

func TestDispatchStatus(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	shut := make(chan struct{}, 1)
	resp := Dispatch(context.Background(), st, shut, rpc.Request{Method: rpc.MethodStatus})
	if resp.Kind != rpc.KindStatus {
		t.Errorf("kind: %s", resp.Kind)
	}
	if resp.ProtocolVersion != rpc.ProtocolVersion {
		t.Errorf("protocol: %d", resp.ProtocolVersion)
	}
}

func TestDispatchShutdownSignals(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	shut := make(chan struct{}, 1)
	resp := Dispatch(context.Background(), st, shut, rpc.Request{Method: rpc.MethodShutdown})
	if resp.Kind != rpc.KindOk {
		t.Errorf("kind: %s", resp.Kind)
	}
	select {
	case <-shut: // ok
	default:
		t.Error("shutdown channel should have a value")
	}
}

func TestDispatchRepoRegister(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	shut := make(chan struct{}, 1)
	resp := Dispatch(context.Background(), st, shut, rpc.Request{
		Method:       rpc.MethodRepoRegister,
		RepoRegister: &rpc.RepoRegisterArgs{Path: "/repos/foo", Name: "foo"},
	})
	if resp.Kind != rpc.KindRepoRegistered {
		t.Errorf("kind: %s msg=%s", resp.Kind, resp.Message)
	}
	if resp.RepoID == 0 {
		t.Error("repo id should be set")
	}
}

func TestDispatchUnknownMethod(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	shut := make(chan struct{}, 1)
	resp := Dispatch(context.Background(), st, shut, rpc.Request{Method: "nope"})
	if resp.Kind != rpc.KindError {
		t.Errorf("kind: %s", resp.Kind)
	}
}
