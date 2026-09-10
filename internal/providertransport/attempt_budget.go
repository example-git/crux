package providertransport

import (
	"context"
	"errors"
	"sync/atomic"
)

var ErrAttemptBudgetExhausted = errors.New("provider operation attempt budget exhausted")

type (
	attemptBudgetKey struct{}
	AttemptBudget    struct{ remaining atomic.Int64 }
)

// ContextWithAttemptBudget shares a single dispatch budget across transport and
// authentication retries. Calls without this context retain existing behavior.
func ContextWithAttemptBudget(ctx context.Context, attempts int) (context.Context, *AttemptBudget) {
	if budget, ok := ctx.Value(attemptBudgetKey{}).(*AttemptBudget); ok && budget != nil {
		return ctx, budget
	}
	budget := &AttemptBudget{}
	budget.remaining.Store(int64(max(attempts, 1)))
	return context.WithValue(ctx, attemptBudgetKey{}, budget), budget
}

func (b *AttemptBudget) Remaining() int64 { return b.remaining.Load() }

func takeAttempt(ctx context.Context) bool {
	budget, ok := ctx.Value(attemptBudgetKey{}).(*AttemptBudget)
	if !ok || budget == nil {
		return true
	}
	for {
		remaining := budget.remaining.Load()
		if remaining <= 0 {
			return false
		}
		if budget.remaining.CompareAndSwap(remaining, remaining-1) {
			return true
		}
	}
}

// OAuth token exchange is a separate operation from the inference retry budget.
func ContextWithoutAttemptBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, attemptBudgetKey{}, (*AttemptBudget)(nil))
}
