package settings

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAnswer is what one lookup of a [fakeResolver] returns.
type fakeAnswer struct {
	ips []string
	err error
}

// fakeResolver answers each lookup with the next of its answers, in order,
// and holds a lookup it has no answer for until the lookup's context ends or
// the test does.
type fakeResolver struct {
	mu      sync.Mutex
	answers []fakeAnswer
	hosts   []string // the host of each lookup, in the order they started
	done    chan struct{}
}

func newFakeResolver(t *testing.T, answers ...fakeAnswer) *fakeResolver {
	r := &fakeResolver{answers: answers, done: make(chan struct{})}
	t.Cleanup(func() { close(r.done) })
	return r
}

func (r *fakeResolver) lookup(ctx context.Context, host string) ([]string, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, host)
	if len(r.answers) == 0 {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.done:
			return nil, errors.New("the test ended")
		}
	}
	answer := r.answers[0]
	r.answers = r.answers[1:]
	r.mu.Unlock()
	return answer.ips, answer.err
}

func (r *fakeResolver) lookups() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hosts)
}

// fakeNetwork connects to every address except the refused ones and records
// each address it was asked to dial.
type fakeNetwork struct {
	mu      sync.Mutex
	refused map[string]error
	dialed  []string
}

func (n *fakeNetwork) dial(_ context.Context, _, addr string) (net.Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dialed = append(n.dialed, addr)
	if err := n.refused[addr]; err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

func (n *fakeNetwork) addresses() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.dialed...)
}

// storeDialer returns a dialer for redis.dbaas:6379, the shape of address
// the DBaaS Redis adapter writes, over resolver and network.
func storeDialer(resolver *fakeResolver, network *fakeNetwork) *resolvingDialer {
	return &resolvingDialer{addr: "redis.dbaas:6379", lookup: resolver.lookup, dial: network.dial}
}

// dialStore dials the store through d and closes the connection it gets.
func dialStore(ctx context.Context, d *resolvingDialer) error {
	conn, err := d.DialContext(ctx, "tcp", "redis.dbaas:6379")
	if err != nil {
		return err
	}
	return conn.Close()
}

// A dial after the first connects at the address the first lookup returned
// while the lookup it started is still waiting for DNS.
func TestResolvingDialer_dialsTheLastAnswerWhileTheNextLookupWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		network := &fakeNetwork{}
		d := storeDialer(newFakeResolver(t, fakeAnswer{ips: []string{"10.0.0.1"}}), network)
		require.NoError(t, dialStore(context.Background(), d), "the first dial")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := dialStore(ctx, d)

		require.NoError(t, err, "the second dial, while the lookup it started has no answer")
		assert.Equal(t, []string{"10.0.0.1:6379", "10.0.0.1:6379"}, network.addresses())
	})
}

// The dial after a lookup returned another address connects there, as it
// does when the Service was recreated with another cluster IP.
func TestResolvingDialer_reachesTheAddressALaterLookupReturned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolver := newFakeResolver(t, fakeAnswer{ips: []string{"10.0.0.1"}}, fakeAnswer{ips: []string{"10.0.0.2"}})
		network := &fakeNetwork{}
		d := storeDialer(resolver, network)
		require.NoError(t, dialStore(context.Background(), d), "the first dial")
		require.NoError(t, dialStore(context.Background(), d), "the dial that starts the second lookup")
		synctest.Wait()

		err := dialStore(context.Background(), d)

		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.1:6379", "10.0.0.1:6379", "10.0.0.2:6379"}, network.addresses())
	})
}

// A lookup that fails, or that returns no address, leaves the dials on the
// address the last good lookup returned.
func TestResolvingDialer_keepsTheLastAnswerWhenALookupHasNone(t *testing.T) {
	for _, tt := range []struct {
		name   string
		answer fakeAnswer
	}{
		{"a lookup that fails", fakeAnswer{err: errors.New("lookup redis.dbaas: i/o timeout")}},
		{"a lookup with no address", fakeAnswer{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				network := &fakeNetwork{}
				d := storeDialer(newFakeResolver(t, fakeAnswer{ips: []string{"10.0.0.1"}}, tt.answer), network)
				require.NoError(t, dialStore(context.Background(), d), "the first dial")
				require.NoError(t, dialStore(context.Background(), d), "the dial that starts the second lookup")
				synctest.Wait()

				err := dialStore(context.Background(), d)

				require.NoError(t, err, "the dial after the second lookup")
				assert.Equal(t, []string{"10.0.0.1:6379", "10.0.0.1:6379", "10.0.0.1:6379"}, network.addresses())
			})
		})
	}
}

// Dials while a background lookup has no answer start no other lookup.
func TestResolvingDialer_runsOneLookupAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolver := newFakeResolver(t, fakeAnswer{ips: []string{"10.0.0.1"}})
		d := storeDialer(resolver, &fakeNetwork{})
		for i := range 3 {
			require.NoError(t, dialStore(context.Background(), d), "dial %d", i+1)
		}

		synctest.Wait()

		assert.Equal(t, 2, resolver.lookups(), "lookups started")
	})
}

// A background lookup that gets no answer ends after lookupTimeout, and the
// next dial starts a new one.
func TestResolvingDialer_endsABackgroundLookupAfterTheLookupTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolver := newFakeResolver(t, fakeAnswer{ips: []string{"10.0.0.1"}})
		d := storeDialer(resolver, &fakeNetwork{})
		require.NoError(t, dialStore(context.Background(), d), "the first dial")
		require.NoError(t, dialStore(context.Background(), d), "the dial that starts the second lookup")
		time.Sleep(lookupTimeout)
		synctest.Wait()

		require.NoError(t, dialStore(context.Background(), d), "the dial after the timeout")

		synctest.Wait()
		assert.Equal(t, 3, resolver.lookups(), "lookups started")
	})
}

// The first dial waits for its lookup, and a lookup that fails fails that
// dial with the lookup's error, before any address is dialed.
func TestResolvingDialer_failsTheFirstDialWithTheLookupError(t *testing.T) {
	errLookup := errors.New("lookup redis.dbaas: no such host")
	network := &fakeNetwork{}
	d := storeDialer(newFakeResolver(t, fakeAnswer{err: errLookup}), network)

	err := dialStore(context.Background(), d)

	assert.ErrorIs(t, err, errLookup)
	assert.Empty(t, network.addresses(), "addresses dialed")
}

// A first lookup that returns no address fails the dial with an error that
// names the host, before any address is dialed.
func TestResolvingDialer_failsTheFirstDialWhenTheLookupHasNoAddress(t *testing.T) {
	network := &fakeNetwork{}
	d := storeDialer(newFakeResolver(t, fakeAnswer{}), network)

	err := dialStore(context.Background(), d)

	assert.ErrorContains(t, err, "lookup redis.dbaas: no addresses")
	assert.Empty(t, network.addresses(), "addresses dialed")
}

// The first dial's lookup ends at the dial's deadline.
func TestResolvingDialer_endsTheFirstLookupAtTheDialsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		network := &fakeNetwork{}
		d := storeDialer(newFakeResolver(t), network)
		ctx, cancel := context.WithTimeout(context.Background(), decisionTimeout)
		defer cancel()

		err := dialStore(ctx, d)

		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Empty(t, network.addresses(), "addresses dialed")
	})
}

// A dial tries the host's addresses in the order the lookup returned them
// and stops at the first that accepts.
func TestResolvingDialer_stopsAtTheFirstAddressThatAccepts(t *testing.T) {
	resolver := newFakeResolver(t, fakeAnswer{ips: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}})
	network := &fakeNetwork{refused: map[string]error{"10.0.0.1:6379": errors.New("connection refused")}}
	d := storeDialer(resolver, network)

	err := dialStore(context.Background(), d)

	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.1:6379", "10.0.0.2:6379"}, network.addresses())
}

// A dial that no address accepts fails with the error of every address, and
// the message names the host.
func TestResolvingDialer_reportsEveryAddressWhenNoneAccepts(t *testing.T) {
	errFirst := errors.New("dial tcp 10.0.0.1:6379: connection refused")
	errSecond := errors.New("dial tcp 10.0.0.2:6379: i/o timeout")
	resolver := newFakeResolver(t, fakeAnswer{ips: []string{"10.0.0.1", "10.0.0.2"}})
	network := &fakeNetwork{refused: map[string]error{"10.0.0.1:6379": errFirst, "10.0.0.2:6379": errSecond}}
	d := storeDialer(resolver, network)

	err := dialStore(context.Background(), d)

	assert.ErrorIs(t, err, errFirst)
	assert.ErrorIs(t, err, errSecond)
	assert.ErrorContains(t, err, "redis.dbaas")
}
