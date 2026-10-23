package settings

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// lookupTimeout bounds a lookup that a [resolvingDialer] runs in the
// background. No dial waits on it.
const lookupTimeout = 5 * time.Second

// resolvingDialer connects to one host:port at the addresses an earlier
// lookup of the host returned, so that once a lookup has succeeded a dial
// does not wait on DNS.
//
// Each dial uses the last answer and starts a lookup in the background for
// the next dial, unless one is already under way. A lookup that fails, or
// that returns no address, keeps the last answer. Only a dial that finds no
// answer yet looks the host up itself, under its own context. A Service
// recreated with another cluster IP keeps receiving dials at its old address
// until a lookup started after the change returns.
//
// The caller sets addr, lookup, and dial; the rest of the zero value is
// ready to use.
type resolvingDialer struct {
	addr   string // the host:port every dial connects to
	lookup func(ctx context.Context, host string) ([]string, error)
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)

	mu        sync.Mutex
	ips       []string // the last answer; nil until a lookup returns addresses
	resolving bool     // a background lookup is under way
}

// DialContext connects to the dialer's address and ignores addr, which
// go-redis sets to the client's own address, the one the dialer was made
// for. It tries the host's addresses in the order the lookup returned them
// and returns the first connection; when none connects, the error names the
// host and wraps each address's error.
func (d *resolvingDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(d.addr)
	if err != nil {
		return nil, err
	}
	ips, err := d.addresses(ctx, host)
	if err != nil {
		return nil, err
	}
	errs := make([]error, 0, len(ips))
	for _, ip := range ips {
		conn, err := d.dial(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("%s: %w", host, errors.Join(errs...))
}

// addresses returns the last answer and starts the next lookup unless one is
// under way, or looks host up on ctx when no lookup has returned addresses
// yet.
func (d *resolvingDialer) addresses(ctx context.Context, host string) ([]string, error) {
	d.mu.Lock()
	ips := d.ips
	if ips != nil && !d.resolving {
		d.resolving = true
		go d.refresh(host)
	}
	d.mu.Unlock()
	if ips != nil {
		return ips, nil
	}
	ips, err := d.lookup(ctx, host)
	if err == nil && len(ips) == 0 {
		err = fmt.Errorf("lookup %s: no addresses", host)
	}
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.ips = ips
	d.mu.Unlock()
	return ips, nil
}

// refresh looks host up and keeps the answer when it has addresses.
func (d *resolvingDialer) refresh(host string) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	ips, err := d.lookup(ctx, host)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resolving = false
	if err == nil && len(ips) > 0 {
		d.ips = ips
	}
}
