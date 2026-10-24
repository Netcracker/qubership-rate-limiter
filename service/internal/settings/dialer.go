package settings

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// lookupTimeout bounds each lookup a [resolvingDialer] runs. A dial waits on
// a lookup only while no lookup has returned addresses, and no longer than
// its own context.
const lookupTimeout = 5 * time.Second

// resolvingDialer connects to one host:port at the addresses a lookup of the
// host returned, so that once a lookup has succeeded a dial does not wait on
// DNS.
//
// The dialer runs one lookup at a time, in the background: a dial starts one
// when none is under way, for the dials after it. A dial uses the last
// answer, and a lookup that fails, or that returns no address, keeps it.
// Dials that find no answer yet, as the client's spare connections do at
// start, share the lookup under way, and each waits for it no longer than its
// own context; the lookup goes on for the dials after it. A Service recreated
// with another cluster IP keeps receiving dials at its old address until a
// lookup started after the change returns.
//
// The caller sets addr, lookup, and dial; the rest of the zero value is
// ready to use.
type resolvingDialer struct {
	addr   string // the host:port every dial connects to
	lookup func(ctx context.Context, host string) ([]string, error)
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)

	mu      sync.Mutex
	ips     []string      // the last answer; nil until a lookup returns addresses
	err     error         // why the last lookup returned no address
	pending chan struct{} // closed when the lookup under way returns; nil when none is
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

// addresses starts a lookup of host unless one is under way, and returns the
// last answer. With no answer yet it waits for the lookup under way and
// returns its answer or its error, or the error of ctx when ctx ends first.
func (d *resolvingDialer) addresses(ctx context.Context, host string) ([]string, error) {
	d.mu.Lock()
	ips, pending := d.ips, d.pending
	if pending == nil {
		pending = make(chan struct{})
		d.pending = pending
		go d.resolve(host, pending)
	}
	d.mu.Unlock()
	if ips != nil {
		return ips, nil
	}
	select {
	case <-pending:
	case <-ctx.Done():
		return nil, fmt.Errorf("lookup %s: %w", host, ctx.Err())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ips == nil {
		return nil, d.err
	}
	return d.ips, nil
}

// resolve looks host up, keeps the answer when it has addresses and the
// error when it has none, and then closes pending.
func (d *resolvingDialer) resolve(host string, pending chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	ips, err := d.lookup(ctx, host)
	if err == nil && len(ips) == 0 {
		err = fmt.Errorf("lookup %s: no addresses", host)
	}
	d.mu.Lock()
	if err == nil {
		d.ips = ips
	}
	d.err = err
	d.pending = nil
	d.mu.Unlock()
	close(pending)
}
