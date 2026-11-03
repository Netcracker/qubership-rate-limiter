package redisclient

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLimitDial_boundsSuccessfulAndFailedAttempts(t *testing.T) {
	for _, failure := range []error{nil, errors.New("connection refused")} {
		name := "successful TCP connection"
		if failure != nil {
			name = "failed TCP connection"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var attempts atomic.Int64
				client, server := net.Pipe()
				defer func() { _ = client.Close(); _ = server.Close() }()
				dial := LimitDial(func(context.Context, string, string) (net.Conn, error) {
					attempts.Add(1)
					return client, failure
				}, 50, 50)
				start := time.Now()
				for second := range 60 {
					exerciseDialBudget(t, dial, client, failure)
					assert.Equal(t, int64((second+1)*50), attempts.Load(), "TCP attempts after second %d", second)
					time.Sleep(time.Second)
				}
				assert.Equal(t, time.Minute, time.Since(start), "throttled calls return without waiting")
				conn, err := dial(context.Background(), "tcp", "redis:6379")
				assert.ErrorIs(t, err, failure, "the refilled budget permits another TCP attempt")
				assert.Same(t, client, conn)
			})
		})
	}
}

func exerciseDialBudget(t *testing.T, dial func(context.Context, string, string) (net.Conn, error),
	wantConn net.Conn, wantErr error,
) {
	t.Helper()
	var throttled atomic.Int64
	var wg sync.WaitGroup
	for range 128 {
		wg.Go(func() {
			for range 20 {
				conn, err := dial(context.Background(), "tcp", "redis:6379")
				if errors.Is(err, ErrDialThrottled) {
					throttled.Add(1)
					assert.Nil(t, conn)
					continue
				}
				assert.ErrorIs(t, err, wantErr)
				assert.Same(t, wantConn, conn)
			}
		})
	}
	wg.Wait()
	assert.Positive(t, throttled.Load())
}

func TestLimitDial_reportsItsBudgetWithoutAllocatingPerRejection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := LimitDial(func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		}, 50, 100)
		for range 100 {
			_, err := dial(context.Background(), "tcp", "redis:6379")
			require.NotErrorIs(t, err, ErrDialThrottled)
		}
		_, err := dial(context.Background(), "tcp", "redis:6379")
		require.ErrorIs(t, err, ErrDialThrottled)
		assert.EqualError(t, err, "redis: connection creation rate exceeded: more than 50 TCP attempts per second, burst 100")
		assert.Zero(t, testing.AllocsPerRun(100, func() {
			_, _ = dial(context.Background(), "tcp", "redis:6379")
		}))
	})
}

func TestLimitDial_canceledCallsPreserveTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attempts int
		dial := LimitDial(func(context.Context, string, string) (net.Conn, error) {
			attempts++
			return nil, errors.New("connection refused")
		}, 1, 1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := dial(ctx, "tcp", "redis:6379")
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, attempts)
		_, err = dial(context.Background(), "tcp", "redis:6379")
		assert.NotErrorIs(t, err, ErrDialThrottled)
		assert.Equal(t, 1, attempts)
	})
}
