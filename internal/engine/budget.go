package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/alekc/freeagent-sync/internal/api"
)

// budget stops a run before it overruns its next scheduled tick or eats the
// hourly rate allowance the user wanted for interactive work. Hitting it is
// not a failure: the archive stays consistent and there is simply more left.
type budget struct {
	maxRequests int64
	deadline    time.Time
	client      *api.Client
	tripped     atomic.Bool
}

func newBudget(maxRequests int64, deadline time.Time, client *api.Client) *budget {
	return &budget{maxRequests: maxRequests, deadline: deadline, client: client}
}

// exceeded reports why the run should stop, or nil to carry on. Checked
// between pages rather than mid-page, so the archive is never left holding
// half a response.
func (b *budget) exceeded(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.maxRequests > 0 && b.client.Requests() >= b.maxRequests {
		b.tripped.Store(true)
		return fmt.Errorf("%w: %d requests", ErrBudgetExhausted, b.maxRequests)
	}
	if !b.deadline.IsZero() && time.Now().After(b.deadline) {
		b.tripped.Store(true)
		return fmt.Errorf("%w: ran past %s", ErrBudgetExhausted,
			b.deadline.Format(time.RFC3339))
	}
	return nil
}

// hit reports whether any worker stopped on the budget.
func (b *budget) hit() bool { return b.tripped.Load() }
