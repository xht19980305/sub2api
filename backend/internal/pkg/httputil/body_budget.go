package httputil

import (
	"context"
	"errors"
	"sync"
)

// ErrRequestBodyMemoryBudget means this instance cannot buffer another request
// body yet. It is transient overload, not a malformed or oversized request.
var ErrRequestBodyMemoryBudget = errors.New("request body memory budget exhausted")

// BodyMemoryBudget bounds buffered ingress allocations across requests. It
// counts chunks, their contiguous copy, decoded output and the decoder window;
// it is not a bound on the application's total heap or protocol conversion.
type BodyMemoryBudget struct {
	mu    sync.Mutex
	limit int64
	used  int64
}

func NewBodyMemoryBudget(limit int64) *BodyMemoryBudget {
	return &BodyMemoryBudget{limit: limit}
}

type bodyMemoryReservation struct {
	mu     sync.Mutex
	budget *BodyMemoryBudget
	bytes  int64
	closed bool
}

type bodyMemoryReservationKey struct{}

// WithRequestBodyMemoryBudget retains reservations until the HTTP handler
// returns, including while a fully buffered request waits for a user slot.
// No goroutines or wait queue are created on exhaustion.
func WithRequestBodyMemoryBudget(ctx context.Context, budget *BodyMemoryBudget) (context.Context, func()) {
	if budget == nil || budget.limit <= 0 || ctx.Value(bodyMemoryReservationKey{}) != nil {
		return ctx, func() {}
	}
	r := &bodyMemoryReservation{budget: budget}
	return context.WithValue(ctx, bodyMemoryReservationKey{}, r), func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed {
			return
		}
		r.closed = true
		budget.mu.Lock()
		budget.used -= r.bytes
		budget.mu.Unlock()
		r.bytes = 0
	}
}

func reserveRequestBodyMemory(ctx context.Context, n int64) error {
	r, _ := ctx.Value(bodyMemoryReservationKey{}).(*bodyMemoryReservation)
	if r == nil || n <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRequestBodyMemoryBudget
	}
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	if n > r.budget.limit-r.budget.used {
		return ErrRequestBodyMemoryBudget
	}
	r.budget.used += n
	r.bytes += n
	return nil
}
