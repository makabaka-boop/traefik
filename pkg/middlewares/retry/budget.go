package retry

import (
	"sync"
	"sync/atomic"

	"github.com/traefik/traefik/v3/pkg/config/dynamic"
)

// BudgetRegistry stores the retry budgets of the retry middlewares belonging
// to one dynamic configuration.
// Middleware instances sharing the same name share the same budget, while
// differently named middlewares get independent budgets.
// A new registry is created for every applied configuration.
type BudgetRegistry struct {
	mu      sync.Mutex
	budgets map[string]*budget
}

// NewBudgetRegistry creates a new BudgetRegistry.
func NewBudgetRegistry() *BudgetRegistry {
	return &BudgetRegistry{budgets: make(map[string]*budget)}
}

// get returns the budget shared by the middleware instances named name,
// creating it on first use.
func (r *BudgetRegistry) get(name string, config dynamic.RetryBudget) *budget {
	r.mu.Lock()
	defer r.mu.Unlock()

	if b, ok := r.budgets[name]; ok {
		return b
	}

	b := &budget{percentage: config.Percentage, minTokens: config.MinTokens}
	r.budgets[name] = b
	return b
}

// budget limits the number of requests that may be retried concurrently.
// A token is acquired on the first decision to retry and held until the
// request processing ends, which covers the backoff waits and the response
// transmission.
type budget struct {
	percentage int
	minTokens  int

	// inFlight counts the client requests the middleware has not finished handling.
	inFlight atomic.Int64
	// retrying counts the requests that hold a retry token.
	retrying atomic.Int64
}

// enter increments the count of in-flight client requests.
func (b *budget) enter() { b.inFlight.Add(1) }

// leave decrements the count of in-flight client requests.
func (b *budget) leave() { b.inFlight.Add(-1) }

// limit returns the maximum number of concurrent retries for the current load:
// the largest of minTokens and the in-flight request count scaled by percentage,
// rounded up.
func (b *budget) limit() int64 {
	inFlight := b.inFlight.Load()
	fromPercentage := (inFlight*int64(b.percentage) + 99) / 100
	return max(int64(b.minTokens), fromPercentage)
}

// tryAcquire reserves a retry token without waiting for one to free up.
// It reports whether a token was available.
func (b *budget) tryAcquire() bool {
	for {
		current := b.retrying.Load()
		if current >= b.limit() {
			return false
		}
		if b.retrying.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// release gives a reserved token back.
func (b *budget) release() { b.retrying.Add(-1) }
