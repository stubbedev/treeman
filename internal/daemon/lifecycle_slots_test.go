package daemon

import (
	"context"
	"errors"
	"testing"
)

func TestLifecycleSlotsQueueBeyondCapacity(t *testing.T) {
	slots := newLifecycleSlots(2)
	free := func() { t.Error("acquiring a free slot must not report a queued wait") }

	first, err := slots.acquire(t.Context(), free)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slots.acquire(t.Context(), free); err != nil {
		t.Fatal(err)
	}

	waiting := make(chan struct{})
	admitted := make(chan func())
	go func() {
		release, err := slots.acquire(context.Background(), func() { close(waiting) })
		if err != nil {
			t.Error(err)
		}
		admitted <- release
	}()
	<-waiting
	select {
	case <-admitted:
		t.Fatal("a third acquire on two busy slots must wait")
	default:
	}
	first()
	first()
	(<-admitted)()
	if len(slots) != 1 {
		t.Errorf("slots in use = %d, want 1 (a repeated release must free only one slot)", len(slots))
	}
}

func TestLifecycleSlotsWaitEndsWithContext(t *testing.T) {
	slots := newLifecycleSlots(1)
	if _, err := slots.acquire(t.Context(), func() {}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := slots.acquire(ctx, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire on a cancelled wait = %v, want context.Canceled", err)
	}
}
