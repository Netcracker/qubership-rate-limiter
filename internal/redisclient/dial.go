// Package redisclient bounds connection creation for Redis clients.
package redisclient

import (
	"context"
	"errors"
	"fmt"
	"net"

	"golang.org/x/time/rate"
)

// ErrDialThrottled reports that the client's connection budget is exhausted.
var ErrDialThrottled = errors.New("redis: connection creation rate exceeded")

// LimitDial limits TCP attempts to limit per second with the given burst.
// Excess attempts fail immediately with [ErrDialThrottled]. The returned
// function is safe for concurrent use; successful dials also spend the budget.
func LimitDial(
	dial func(context.Context, string, string) (net.Conn, error), limit rate.Limit, burst int,
) func(context.Context, string, string) (net.Conn, error) {
	budget := rate.NewLimiter(limit, burst)
	throttled := fmt.Errorf("%w: more than %v TCP attempts per second, burst %d", ErrDialThrottled, float64(limit), burst)
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !budget.Allow() {
			return nil, throttled
		}
		return dial(ctx, network, addr)
	}
}
