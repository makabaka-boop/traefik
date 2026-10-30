package retry

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/traefik/traefik/v3/pkg/config/dynamic"
)

func TestBudgetLimit(t *testing.T) {
	testCases := []struct {
		desc       string
		percentage int
		minTokens  int
		limits     []struct {
			inFlight int64
			want     int64
		}
	}{
		{
			desc:       "1 percent with one token minimum rounds up",
			percentage: 1,
			minTokens:  1,
			limits: []struct {
				inFlight int64
				want     int64
			}{
				{1, 1},
				{99, 1},
				{100, 1},
				{101, 2},
				{200, 2},
				{250, 3},
			},
		},
		{
			desc:       "50 percent rounds up",
			percentage: 50,
			minTokens:  1,
			limits: []struct {
				inFlight int64
				want     int64
			}{
				{1, 1},
				{2, 1},
				{3, 2},
				{4, 2},
				{5, 3},
			},
		},
		{
			desc:       "100 percent tracks the load",
			percentage: 100,
			minTokens:  1,
			limits: []struct {
				inFlight int64
				want     int64
			}{
				{1, 1},
				{5, 5},
				{10, 10},
			},
		},
		{
			desc:       "minimum tokens wins over a low load",
			percentage: 10,
			minTokens:  5,
			limits: []struct {
				inFlight int64
				want     int64
			}{
				{0, 5},
				{1, 5},
				{10, 5},
				{100, 10},
				{200, 20},
			},
		},
		{
			desc:       "minimum tokens wins under 100 percent",
			percentage: 100,
			minTokens:  3,
			limits: []struct {
				inFlight int64
				want     int64
			}{
				{0, 3},
				{1, 3},
				{2, 3},
				{5, 5},
			},
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			b := &budget{percentage: test.percentage, minTokens: test.minTokens}
			for _, limit := range test.limits {
				b.inFlight.Store(limit.inFlight)
				assert.Equal(t, limit.want, b.limit(), "in-flight %d", limit.inFlight)
			}
		})
	}
}

func TestBudgetTryAcquire(t *testing.T) {
	t.Run("grants up to the limit and frees on release", func(t *testing.T) {
		b := &budget{percentage: 50, minTokens: 1}
		b.inFlight.Store(10)

		for range 5 {
			assert.True(t, b.tryAcquire())
		}
		assert.False(t, b.tryAcquire())

		b.release()
		assert.True(t, b.tryAcquire())
		assert.False(t, b.tryAcquire())
	})

	t.Run("lowering the limit does not revoke granted tokens", func(t *testing.T) {
		b := &budget{percentage: 100, minTokens: 1}
		b.inFlight.Store(10)

		for range 4 {
			require.True(t, b.tryAcquire())
		}

		// Most of the in-flight requests finish, dropping the limit to 1.
		for range 9 {
			b.leave()
		}
		require.Equal(t, int64(1), b.inFlight.Load())
		require.Equal(t, int64(1), b.limit())

		// The four granted retries keep running, but no new token is available.
		assert.Equal(t, int64(4), b.retrying.Load())
		assert.False(t, b.tryAcquire())

		for range 4 {
			b.release()
		}
		assert.Equal(t, int64(0), b.retrying.Load())
		assert.True(t, b.tryAcquire())
	})
}

func TestRetryBudgetConfigValidation(t *testing.T) {
	testCases := []struct {
		desc      string
		budget    *dynamic.RetryBudget
		expectErr string
	}{
		{
			desc:   "no budget",
			budget: nil,
		},
		{
			desc:   "lower bounds",
			budget: &dynamic.RetryBudget{Percentage: 1, MinTokens: 1},
		},
		{
			desc:   "upper bounds",
			budget: &dynamic.RetryBudget{Percentage: 100, MinTokens: 100},
		},
		{
			desc:      "zero percentage",
			budget:    &dynamic.RetryBudget{Percentage: 0, MinTokens: 1},
			expectErr: "budget percentage must be an integer between 1 and 100",
		},
		{
			desc:      "negative percentage",
			budget:    &dynamic.RetryBudget{Percentage: -1, MinTokens: 1},
			expectErr: "budget percentage must be an integer between 1 and 100",
		},
		{
			desc:      "percentage above 100",
			budget:    &dynamic.RetryBudget{Percentage: 101, MinTokens: 1},
			expectErr: "budget percentage must be an integer between 1 and 100",
		},
		{
			desc:      "zero min tokens",
			budget:    &dynamic.RetryBudget{Percentage: 10, MinTokens: 0},
			expectErr: "budget minTokens must be a positive integer",
		},
		{
			desc:      "negative min tokens",
			budget:    &dynamic.RetryBudget{Percentage: 10, MinTokens: -3},
			expectErr: "budget minTokens must be a positive integer",
		},
		{
			desc:      "empty budget",
			budget:    &dynamic.RetryBudget{},
			expectErr: "budget percentage must be an integer between 1 and 100",
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				rw.WriteHeader(http.StatusOK)
			})

			config := dynamic.Retry{Attempts: 2, Status: []string{"503"}, Budget: test.budget}
			_, err := New(t.Context(), next, config, &countingRetryListener{}, "traefikTest", nil)

			if test.expectErr != "" {
				require.Error(t, err)
				assert.EqualError(t, err, test.expectErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRetryBudgetSharedAcrossRoutes(t *testing.T) {
	registry := NewBudgetRegistry()
	config := dynamic.Retry{
		Attempts: 3,
		Status:   []string{"503"},
		Budget:   &dynamic.RetryBudget{Percentage: 1, MinTokens: 1},
	}

	releaseHolder, waitHolderDone := holdRetryToken(t, registry, "shared-retry", config)
	defer func() {
		releaseHolder()
		waitHolderDone()
	}()

	sharedBudget := registry.budgets["shared-retry"]
	require.Equal(t, int64(1), sharedBudget.inFlight.Load(), "the holder request is counted once despite its multiple attempts")
	require.Equal(t, int64(1), sharedBudget.retrying.Load())

	// A second route references the same middleware name, hence the same budget.
	// Its first attempt fails while the only token is held, so it must return
	// the current attempt as is, without waiting or retrying.
	deniedCalls := 0
	deniedNext := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		deniedCalls++
		rw.Header().Set("X-Denied", "true")
		rw.WriteHeader(http.StatusServiceUnavailable)
		_, _ = rw.Write([]byte("failure body"))
	})

	deniedListener := &countingRetryListener{}
	deniedHandler, err := New(t.Context(), deniedNext, config, deniedListener, "shared-retry", registry)
	require.NoError(t, err)

	deniedRecorder := httptest.NewRecorder()
	deniedHandler.ServeHTTP(deniedRecorder, httptest.NewRequest(http.MethodGet, "http://localhost/", nil))

	assert.Equal(t, http.StatusServiceUnavailable, deniedRecorder.Code)
	assert.Equal(t, "failure body", deniedRecorder.Body.String())
	assert.Equal(t, "true", deniedRecorder.Header().Get("X-Denied"))
	assert.Equal(t, 1, deniedCalls)
	assert.Equal(t, 0, deniedListener.timesCalled)
	assert.Equal(t, int64(1), sharedBudget.retrying.Load(), "a denied request does not hold a token")
	assert.Equal(t, int64(1), sharedBudget.inFlight.Load(), "the denied request released its in-flight count")

	// Once the holder finishes, its token is available again.
	releaseHolder()
	waitHolderDone()

	require.Equal(t, int64(0), sharedBudget.retrying.Load())
	require.Equal(t, int64(0), sharedBudget.inFlight.Load())

	retriedCalls := 0
	retriedNext := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		retriedCalls++
		if retriedCalls == 1 {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rw.WriteHeader(http.StatusOK)
	})

	retriedListener := &countingRetryListener{}
	retriedHandler, err := New(t.Context(), retriedNext, config, retriedListener, "shared-retry", registry)
	require.NoError(t, err)

	retriedRecorder := httptest.NewRecorder()
	retriedHandler.ServeHTTP(retriedRecorder, httptest.NewRequest(http.MethodGet, "http://localhost/", nil))

	assert.Equal(t, http.StatusOK, retriedRecorder.Code)
	assert.Equal(t, 2, retriedCalls)
	assert.Equal(t, 1, retriedListener.timesCalled)
	assert.Equal(t, int64(0), sharedBudget.retrying.Load())
	assert.Equal(t, int64(0), sharedBudget.inFlight.Load())
}

func TestRetryBudgetIndependentPerName(t *testing.T) {
	registry := NewBudgetRegistry()
	config := dynamic.Retry{
		Attempts: 3,
		Status:   []string{"503"},
		Budget:   &dynamic.RetryBudget{Percentage: 1, MinTokens: 1},
	}

	releaseHolder, waitHolderDone := holdRetryToken(t, registry, "first-retry", config)
	defer func() {
		releaseHolder()
		waitHolderDone()
	}()

	// A differently named middleware gets its own budget and still retries.
	calls := 0
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rw.WriteHeader(http.StatusOK)
	})

	listener := &countingRetryListener{}
	handler, err := New(t.Context(), next, config, listener, "second-retry", registry)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://localhost/", nil))

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, listener.timesCalled)
}

func TestRetryBudgetIndependentPerRegistry(t *testing.T) {
	holderRegistry := NewBudgetRegistry()
	otherRegistry := NewBudgetRegistry()
	config := dynamic.Retry{
		Attempts: 3,
		Status:   []string{"503"},
		Budget:   &dynamic.RetryBudget{Percentage: 1, MinTokens: 1},
	}

	releaseHolder, waitHolderDone := holdRetryToken(t, holderRegistry, "same-name", config)
	defer func() {
		releaseHolder()
		waitHolderDone()
	}()

	calls := 0
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rw.WriteHeader(http.StatusOK)
	})

	listener := &countingRetryListener{}
	handler, err := New(t.Context(), next, config, listener, "same-name", otherRegistry)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://localhost/", nil))

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, listener.timesCalled)
}

func TestRetryBudgetAllowsUnderPercentage(t *testing.T) {
	registry := NewBudgetRegistry()
	config := dynamic.Retry{
		Attempts: 3,
		Status:   []string{"503"},
		Budget:   &dynamic.RetryBudget{Percentage: 100, MinTokens: 1},
	}

	releaseHolder, waitHolderDone := holdRetryToken(t, registry, "percent-retry", config)
	defer func() {
		releaseHolder()
		waitHolderDone()
	}()

	calls := 0
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rw.WriteHeader(http.StatusOK)
	})

	listener := &countingRetryListener{}
	handler, err := New(t.Context(), next, config, listener, "percent-retry", registry)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://localhost/", nil))

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, listener.timesCalled)
}

func TestRetryBudgetDeniedOnNetworkError(t *testing.T) {
	registry := NewBudgetRegistry()
	statusConfig := dynamic.Retry{
		Attempts: 3,
		Status:   []string{"503"},
		Budget:   &dynamic.RetryBudget{Percentage: 1, MinTokens: 1},
	}

	releaseHolder, waitHolderDone := holdRetryToken(t, registry, "network-retry", statusConfig)
	defer func() {
		releaseHolder()
		waitHolderDone()
	}()

	// A network-error-only retry configuration, served through WrapHandler so
	// that the failed attempt looks like a network error before any byte is sent.
	networkConfig := dynamic.Retry{
		Attempts: 3,
		Budget:   &dynamic.RetryBudget{Percentage: 1, MinTokens: 1},
	}

	calls := 0
	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		calls++
		rw.Header().Set("X-Denied", "true")
		rw.WriteHeader(http.StatusBadGateway)
		_, _ = rw.Write([]byte("gateway failure"))
	})

	listener := &countingRetryListener{}
	handler, err := New(t.Context(), WrapHandler(next), networkConfig, listener, "network-retry", registry)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://localhost/", nil))

	assert.Equal(t, http.StatusBadGateway, recorder.Code)
	assert.Equal(t, "gateway failure", recorder.Body.String())
	assert.Equal(t, "true", recorder.Header().Get("X-Denied"))
	assert.Equal(t, 1, calls)
	assert.Equal(t, 0, listener.timesCalled)
}

// holdRetryToken builds a retry middleware named name on registry and serves a
// request whose first attempt is retried and whose second attempt blocks until
// the returned release function is called. It therefore holds one retry token.
// The returned wait function blocks until the parked request has completed.
func holdRetryToken(t *testing.T, registry *BudgetRegistry, name string, config dynamic.Retry) (release func(), wait func()) {
	t.Helper()

	attemptTwoStarted := make(chan struct{})
	releaseCh := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once

	attempts := 0
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		close(attemptTwoStarted)
		<-releaseCh
		rw.WriteHeader(http.StatusOK)
	})

	listener := &countingRetryListener{}
	handler, err := New(t.Context(), next, config, listener, name, registry)
	require.NoError(t, err)

	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://localhost/", nil))
	}()

	<-attemptTwoStarted
	require.Equal(t, 1, listener.timesCalled)

	return func() {
			once.Do(func() { close(releaseCh) })
		}, func() {
			<-done
		}
}
