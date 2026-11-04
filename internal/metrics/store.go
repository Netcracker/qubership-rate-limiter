package metrics

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/netcracker/qubership-rate-limiter/engine/store"
	"github.com/netcracker/qubership-rate-limiter/internal/redisclient"
)

// InstrumentStore wraps a counter store with the roundtrip histogram and the
// error counter of one domain. The wrapper carries the domain label so no key
// parsing happens on the hot path; only Decide is observed — Peek and Reset
// belong to the management API, not to the traffic the histograms describe.
func InstrumentStore(domain string, next store.Store) store.Store {
	return instrumentedStore{domain: domain, next: next}
}

type instrumentedStore struct {
	domain string
	next   store.Store
}

func (s instrumentedStore) Decide(ctx context.Context, buckets []store.Bucket) ([]store.Verdict, error) {
	start := time.Now()
	verdicts, err := s.next.Decide(ctx, buckets)
	StoreRoundtrip.WithLabelValues(s.domain).Observe(time.Since(start).Seconds())
	if err != nil {
		StoreErrors.WithLabelValues(s.domain, storeErrorReason(err)).Inc()
	}
	return verdicts, err
}

func (s instrumentedStore) Peek(ctx context.Context, buckets []store.Bucket) ([]store.Verdict, error) {
	return s.next.Peek(ctx, buckets)
}

func (s instrumentedStore) Reset(ctx context.Context, keys []string) error {
	return s.next.Reset(ctx, keys)
}

// storeErrorReason folds a store error into the fixed reason set. Timeout is
// load or distance, a wait for a free pooled connection included; server means
// the store answered an error, a script or command problem that no retry cures;
// dial_throttled means the decision client spent its connection budget, and
// port_exhausted means the pod cannot allocate a local address (EADDRNOTAVAIL);
// other is any other connectivity failure.
func storeErrorReason(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, redisclient.ErrDialThrottled):
		return "dial_throttled"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return "port_exhausted"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, goredis.ErrPoolTimeout):
		return "timeout"
	default:
	}
	var redisErr goredis.Error
	if errors.As(err, &redisErr) {
		return "server"
	}
	return "other"
}
