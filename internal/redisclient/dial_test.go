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
	for _, fail := range []bool{false, true} {
		name := "successful TCP connection"
		if fail {
			name = "failed TCP connection"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var attempts atomic.Int64
				failure := errors.New("connection refused")
				dial := LimitDial(func(context.Context, string, string) (net.Conn, error) {
					attempts.Add(1)
					if fail {
						return nil, failure
					}
					client, server := net.Pipe()
					_ = server.Close()
					return client, nil
				}, 50, 50)
				var throttled atomic.Int64
				start := time.Now()
				for second := range 60 {
					var wg sync.WaitGroup
					for range 128 {
						wg.Go(func() {
							for range 20 {
								conn, err := dial(context.Background(), "tcp", "redis:6379")
								switch {
								case errors.Is(err, ErrDialThrottled):
									throttled.Add(1)
								case fail:
									assert.ErrorIs(t, err, failure)
								default:
									assert.NoError(t, err)
									if conn != nil {
										_ = conn.Close()
									}
								}
							}
						})
					}
					wg.Wait()
					assert.Equal(t, int64((second+1)*50), attempts.Load(), "TCP attempts after second %d", second)
					time.Sleep(time.Second)
				}
				assert.Equal(t, time.Minute, time.Since(start), "throttled calls return without waiting")
				assert.Positive(t, throttled.Load())
				conn, err := dial(context.Background(), "tcp", "redis:6379")
				if fail {
					require.ErrorIs(t, err, failure, "the refilled budget permits another TCP attempt")
				} else {
					require.NoError(t, err)
					_ = conn.Close()
				}
			})
		})
	}
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
