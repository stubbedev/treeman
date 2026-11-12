package mysql

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type sharedOnce[T any] struct {
	mu      sync.Mutex
	settled bool
	value   T
	err     error
	running chan struct{}
}

func (s *sharedOnce[T]) do(ctx context.Context, compute func(context.Context) (T, error)) (T, error) {
	for {
		s.mu.Lock()
		if s.settled {
			value, err := s.value, s.err
			s.mu.Unlock()
			return value, err
		}
		running := s.running
		if running == nil {
			s.running = make(chan struct{})
			s.mu.Unlock()
			return s.lead(ctx, compute)
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			var zero T
			return zero, fmt.Errorf("wait for a concurrent run: %w", ctx.Err())
		case <-running:
		}
	}
}

func (s *sharedOnce[T]) lead(ctx context.Context, compute func(context.Context) (T, error)) (value T, err error) {
	keep := false
	defer func() {
		s.mu.Lock()
		if keep {
			s.settled, s.value, s.err = true, value, err
		}
		close(s.running)
		s.running = nil
		s.mu.Unlock()
	}()
	value, err = compute(ctx)
	ctxErr := ctx.Err()
	if err == nil || ctxErr == nil {
		keep = true
		return value, err
	}
	if errors.Is(err, ctxErr) {
		return value, err
	}
	return value, fmt.Errorf("%w: %w", ctxErr, err)
}

func (s *sharedOnce[T]) peek() (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.settled || s.err != nil {
		var zero T
		return zero, false
	}
	return s.value, true
}
