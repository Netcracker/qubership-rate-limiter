package redis_test

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis_rate/v10"
	goredis "github.com/redis/go-redis/v9"

	"github.com/netcracker/qubership-rate-limiter/engine/algo"
	"github.com/netcracker/qubership-rate-limiter/engine/store"
	"github.com/netcracker/qubership-rate-limiter/engine/store/memory"
	redisstore "github.com/netcracker/qubership-rate-limiter/engine/store/redis"
	"github.com/netcracker/qubership-rate-limiter/engine/store/storetest"
)

// tolerance absorbs the clock difference between this process and the store
// when durations are compared across the two.
const tolerance = 2 * time.Second

// client resolves the store under test in three steps: REDIS_ADDR when set
// (the CI service, a cluster, anything explicit); a disposable container this
// test binary starts itself when Docker is available; a skip otherwise. An
// explicit address that stays unreachable is a failure, never a silent skip.
func client(t testing.TB) goredis.UniversalClient {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		var reason string
		addr, reason = disposableRedis()
		if addr == "" {
			t.Skipf("REDIS_ADDR is not set and no disposable Redis: %s", reason)
		}
	}
	c := goredis.NewUniversalClient(&goredis.UniversalOptions{Addrs: strings.Split(addr, ",")})
	t.Cleanup(func() { _ = c.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := c.Ping(t.Context()).Err()
		if err == nil {
			requireCluster(t, c, addr)
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("Redis at %s is unreachable: %v", addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// disposable is the one throwaway container shared by every test in this
// binary; TestMain stops it. The testcontainers behavior without the
// dependency swarm: docker CLI via os/exec, a random host port, --rm.
var disposable struct {
	once sync.Once
	addr string
	err  string
	name string
}

func disposableRedis() (addr, reason string) {
	disposable.once.Do(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			disposable.err = "docker is not in PATH"
			return
		}

		// A published port lands on the machine running the daemon, which is not
		// necessarily this one: DOCKER_HOST and docker contexts both point at
		// remote daemons routinely. Binding such a container to the loopback of
		// the daemon would publish it where no test can reach it, so the bind
		// address follows where the daemon lives.
		host := daemonHost()
		bind := "127.0.0.1"
		if host != "" {
			bind = "0.0.0.0"
		}

		name := fmt.Sprintf("ratelimit-test-redis-%d-%d", os.Getpid(), time.Now().UnixNano())
		if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
			"-p", bind+":0:6379", "redis:8-alpine").CombinedOutput(); err != nil {
			disposable.err = fmt.Sprintf("docker run failed: %v: %s", err, strings.TrimSpace(string(out)))
			return
		}
		disposable.name = name
		out, err := exec.Command("docker", "port", name, "6379/tcp").Output()
		if err != nil {
			disposable.err = fmt.Sprintf("docker port failed: %v", err)
			return
		}

		published := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if host == "" {
			disposable.addr = published
			return
		}
		// docker port reports the bind address the daemon used, which is
		// 0.0.0.0 here; only the port number is ours to keep.
		_, port, err := net.SplitHostPort(published)
		if err != nil {
			disposable.err = fmt.Sprintf("docker port returned %q: %v", published, err)
			return
		}
		disposable.addr = net.JoinHostPort(host, port)
	})
	return disposable.addr, disposable.err
}

// daemonHost returns the host the Docker daemon runs on, or "" when it is this
// machine and a published port is therefore reachable as docker port reports it.
func daemonHost() string {
	endpoint := os.Getenv("DOCKER_HOST")
	if endpoint == "" {
		// DOCKER_HOST overrides the context, so the context is only consulted
		// when it is unset.
		if out, err := exec.Command("docker", "context", "inspect",
			"--format", "{{.Endpoints.docker.Host}}").Output(); err == nil {
			endpoint = strings.TrimSpace(string(out))
		}
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	switch parsed.Scheme {
	case "tcp", "ssh", "http", "https":
		return parsed.Hostname()
	default:
		// A unix socket, a named pipe, or something unrecognized: treat the
		// daemon as local rather than guessing at an address.
		return ""
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if disposable.name != "" {
		_ = exec.Command("docker", "stop", disposable.name).Run()
	}
	os.Exit(code)
}

// The conformance suite the in-memory store passes is the acceptance bar,
// here against a live Redis.
func TestConformsToTheStoreContract(t *testing.T) {
	c := client(t)
	storetest.Run(t, func(t *testing.T) store.Store {
		return redisstore.New(c)
	})
}

// Both stores run through one scenario, and their verdicts are compared field
// for field: the Lua script and the in-memory reference must be the same math.
func TestRedisAndMemoryReturnTheSameVerdicts(t *testing.T) {
	r := redisstore.New(client(t))
	m := memory.New()
	waitOutBoundary(time.Hour, 10*time.Second)

	uniq := fmt.Sprintf("diff:{%d}", time.Now().UnixNano())
	buckets := []store.Bucket{
		{Key: uniq + ":g", Algorithm: algo.GCRAID, Cost: 1,
			Window: algo.Window{Requests: 10, Period: time.Hour, Burst: 5}},
		{Key: uniq + ":f", Algorithm: algo.FixedWindowID, Cost: 1,
			Window: algo.Window{Requests: 3, Period: time.Hour}},
		{Key: uniq + ":s", Algorithm: algo.GCRAID, Shadow: true, Cost: 1,
			Window: algo.Window{Requests: 2, Period: time.Hour, Burst: 2}},
	}

	// Steps 1-3 admit; from step 4 the fixed window refuses and all-or-nothing
	// holds both stores in the refused shape, shadow exhaustion included.
	for step := 1; step <= 6; step++ {
		compareBoth(t, fmt.Sprintf("decide step %d", step), buckets, func(s store.Store) ([]store.Verdict, error) {
			return s.Decide(t.Context(), buckets)
		}, r, m)
	}
	compareBoth(t, "peek after the run", buckets, func(s store.Store) ([]store.Verdict, error) {
		return s.Peek(t.Context(), buckets)
	}, r, m)
	compareBoth(t, "peek at a different cost per bucket", buckets, func(s store.Store) ([]store.Verdict, error) {
		return s.Peek(t.Context(), withCosts(buckets, 4, 1, 2))
	}, r, m)
	compareBoth(t, "impossible cost", buckets, func(s store.Store) ([]store.Verdict, error) {
		return s.Decide(t.Context(), withCosts(buckets, 100, 100, 100))
	}, r, m)
}

// withCosts returns copies of buckets, each charging the cost at its index.
func withCosts(buckets []store.Bucket, costs ...int64) []store.Bucket {
	out := slices.Clone(buckets)
	for i := range out {
		out[i].Cost = costs[i]
	}
	return out
}

// compareBoth runs op on both stores and compares the verdicts bucket by
// bucket: Allowed, CostExceedsCapacity, and Remaining exactly, RetryAfter and
// ResetAfter within tolerance. Every call of the scenario is valid, so an
// error from either store fails the comparison.
func compareBoth(t *testing.T, label string, buckets []store.Bucket,
	op func(store.Store) ([]store.Verdict, error), r, m store.Store) {
	t.Helper()
	rv, rerr := op(r)
	mv, merr := op(m)
	if rerr != nil || merr != nil {
		t.Fatalf("%s: redis err = %v, memory err = %v, want neither", label, rerr, merr)
	}
	if len(rv) != len(buckets) || len(mv) != len(buckets) {
		t.Fatalf("%s: redis %d verdicts, memory %d, want %d", label, len(rv), len(mv), len(buckets))
	}
	for i, b := range buckets {
		if rv[i].Allowed != mv[i].Allowed || rv[i].CostExceedsCapacity != mv[i].CostExceedsCapacity ||
			rv[i].Remaining != mv[i].Remaining {
			t.Errorf("%s, bucket %s: redis %+v, memory %+v", label, b.Key, rv[i], mv[i])
		}
		if diff := rv[i].RetryAfter - mv[i].RetryAfter; diff > tolerance || diff < -tolerance {
			t.Errorf("%s, bucket %s: RetryAfter diverged: redis %s, memory %s", label, b.Key,
				rv[i].RetryAfter, mv[i].RetryAfter)
		}
		if diff := rv[i].ResetAfter - mv[i].ResetAfter; diff > tolerance || diff < -tolerance {
			t.Errorf("%s, bucket %s: ResetAfter diverged: redis %s, memory %s", label, b.Key,
				rv[i].ResetAfter, mv[i].ResetAfter)
		}
	}
}

// The Lua GCRA is a port of redis_rate, which serves as the oracle on single
// buckets: the admission pattern matches request for request, and remaining
// within one unit, because the oracle rounds where the script floors.
func TestTheGCRAScriptAdmitsAsRedisRateDoes(t *testing.T) {
	c := client(t)
	r := redisstore.New(c)
	oracle := redis_rate.NewLimiter(c)

	uniq := fmt.Sprintf("oracle:{%d}", time.Now().UnixNano())
	b := store.Bucket{Key: uniq + ":g", Algorithm: algo.GCRAID, Cost: 1,
		Window: algo.Window{Requests: 10, Period: time.Hour, Burst: 5}}
	limit := redis_rate.Limit{Rate: 10, Period: time.Hour, Burst: 5}

	for step := 1; step <= 8; step++ {
		ours, err := r.Decide(t.Context(), []store.Bucket{b})
		if err != nil {
			t.Fatalf("step %d: Decide: %v", step, err)
		}
		theirs, err := oracle.Allow(t.Context(), uniq+":oracle", limit)
		if err != nil {
			t.Fatalf("step %d: oracle: %v", step, err)
		}
		if ours[0].Allowed != (theirs.Allowed > 0) {
			t.Fatalf("step %d: admission diverged: ours %v, oracle %d", step, ours[0].Allowed, theirs.Allowed)
		}
		if ours[0].Allowed {
			diff := ours[0].Remaining - int64(theirs.Remaining)
			if diff > 1 || diff < -1 {
				t.Errorf("step %d: remaining diverged: ours %d, oracle %d", step, ours[0].Remaining, theirs.Remaining)
			}
		}
	}
}

// A GCRA bucket of 100000 requests a second stores its state as exact
// integers. Lua's tostring formats numbers as %.14g, which rounds a 16-digit
// microsecond timestamp by about 100 microseconds and at this rate forgives
// debt silently. The in-script verdict is exact to the request.
func TestAHighFrequencyBucketStoresItsStateAsExactIntegers(t *testing.T) {
	c := client(t)
	r := redisstore.New(c)

	uniq := fmt.Sprintf("hf:{%d}", time.Now().UnixNano())
	gcra := store.Bucket{Key: uniq + ":g", Algorithm: algo.GCRAID, Cost: 9000,
		Window: algo.Window{Requests: 100_000, Period: time.Second, Burst: 10_000}}
	fixed := store.Bucket{Key: uniq + ":f", Algorithm: algo.FixedWindowID, Cost: 9000,
		Window: algo.Window{Requests: 100_000, Period: time.Second}}

	// The fixed-window state can expire as soon as the second boundary
	// passes, and the GET below has to find it: the decision starts clear of
	// the boundary.
	waitOutBoundary(time.Second, 200*time.Millisecond)

	// Charged in-script at one instant: the verdict is exact, no clock races.
	v, err := r.Decide(t.Context(), []store.Bucket{gcra, fixed})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !v[0].Allowed || v[0].Remaining != 1000 {
		t.Fatalf("Decide(cost 9000) verdict of %s = %+v, want allowed with exactly 1000 remaining", gcra.Key, v[0])
	}

	nowUS := time.Now().UnixMicro()
	for _, b := range []store.Bucket{gcra, fixed} {
		raw, err := c.Get(t.Context(), b.Key).Result()
		if err != nil {
			t.Fatalf("GET %s: %v", b.Key, err)
		}
		assertExactTimestampState(t, b.Key, raw, nowUS)
	}

	// The next decision reads the stored integer state back. Time passes
	// between the two calls and refills the bucket, so the verdict cannot
	// show whether a rounding forgave part of the debt; the format check
	// above covers that.
	gcra.Cost = 1
	if v, err = r.Decide(t.Context(), []store.Bucket{gcra}); err != nil || !v[0].Allowed {
		t.Fatalf("Decide(cost 1) after the stored state = %+v, %v; want allowed", v, err)
	}
}

// assertExactTimestampState fails on tostring precision loss: every state
// segment is a plain integer, and the leading one parses to a microsecond
// timestamp within two seconds of now.
func assertExactTimestampState(t *testing.T, key, raw string, nowUS int64) {
	t.Helper()
	for part := range strings.SplitSeq(raw, ":") {
		if part == "" || strings.ContainsAny(part, "eE.+") {
			t.Errorf("state of %s = %q, want integer segments; tostring precision loss", key, raw)
			return
		}
	}
	ts, err := strconv.ParseInt(strings.Split(raw, ":")[0], 10, 64)
	if err != nil {
		t.Errorf("state of %s = %q, want a leading timestamp: %v", key, raw, err)
		return
	}
	if ts < nowUS-2_000_000 || ts > nowUS+2_000_000 {
		t.Errorf("state of %s = %q: timestamp %d, want within 2 s of now, %d", key, raw, ts, nowUS)
	}
}

// State vanishes on its own once its window drains: the store has no cleanup
// process to fall back on. A fixed-window key outlives its window by less than
// a tenth of the period. Both windows last one second, so the keys live at
// most 1.1 s, and the poll allows 1.2 s for them to go.
func TestStateIsGoneOnceItsWindowDrains(t *testing.T) {
	c := client(t)
	r := redisstore.New(c)

	uniq := fmt.Sprintf("ttl:{%d}", time.Now().UnixNano())
	buckets := []store.Bucket{
		{Key: uniq + ":g", Algorithm: algo.GCRAID, Cost: 1,
			Window: algo.Window{Requests: 1, Period: time.Second, Burst: 1}},
		{Key: uniq + ":f", Algorithm: algo.FixedWindowID, Cost: 1,
			Window: algo.Window{Requests: 1, Period: time.Second}},
	}
	if _, err := r.Decide(t.Context(), buckets); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	for _, b := range buckets {
		ttl, err := c.PTTL(t.Context(), b.Key).Result()
		if err != nil || ttl <= 0 || ttl > 1100*time.Millisecond {
			t.Errorf("PTTL(%s) = %v, %v; want within (0, 1.1s]", b.Key, ttl, err)
		}
	}

	deadline := time.Now().Add(1200 * time.Millisecond)
	for live := liveKeys(t, c, buckets); len(live) > 0; live = liveKeys(t, c, buckets) {
		if time.Now().After(deadline) {
			t.Fatalf("keys %v exist 1.2 s after the decision, want none once both windows drained", live)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// liveKeys returns the keys of the buckets that still exist in c.
func liveKeys(t *testing.T, c goredis.UniversalClient, buckets []store.Bucket) []string {
	t.Helper()
	var live []string
	for _, b := range buckets {
		exists, err := c.Exists(t.Context(), b.Key).Result()
		if err != nil {
			t.Fatalf("Exists(%s): %v", b.Key, err)
		}
		if exists != 0 {
			live = append(live, b.Key)
		}
	}
	return live
}

// The keys of one fixed-window rate expire at different moments: each outlives
// the window boundary by an offset below a tenth of the period, taken from the
// key, while the verdict's reset still names the boundary. Twenty keys written
// in one decision of an hourly rate therefore expire over several minutes, not
// in the same second.
func TestFixedWindowKeysExpireAtOffsetsSpreadPastTheBoundary(t *testing.T) {
	c := client(t)
	r := redisstore.New(c)

	const period = time.Hour
	waitOutBoundary(period, tolerance)
	uniq := fmt.Sprintf("linger:{%d}", time.Now().UnixNano())
	buckets := make([]store.Bucket, 20)
	for i := range buckets {
		buckets[i] = store.Bucket{Key: fmt.Sprintf("%s:%d", uniq, i), Algorithm: algo.FixedWindowID, Cost: 1,
			Window: algo.Window{Requests: 10, Period: period}}
	}

	now, err := c.Time(t.Context()).Result()
	if err != nil {
		t.Fatalf("TIME: %v", err)
	}
	untilBoundary := now.Truncate(period).Add(period).Sub(now)
	v, err := r.Decide(t.Context(), buckets)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	lowest, highest := untilBoundary+period, time.Duration(0)
	for i, b := range buckets {
		if v[i].ResetAfter < untilBoundary-tolerance || v[i].ResetAfter > untilBoundary {
			t.Errorf("Decide verdict of %s: ResetAfter = %v, want within [%v, %v]: the boundary, without the offset",
				b.Key, v[i].ResetAfter, untilBoundary-tolerance, untilBoundary)
		}
		ttl, err := c.PTTL(t.Context(), b.Key).Result()
		if err != nil {
			t.Fatalf("PTTL(%s): %v", b.Key, err)
		}
		// PX rounds the lifetime up to the millisecond.
		if ttl < untilBoundary-tolerance || ttl > untilBoundary+period/10+time.Millisecond {
			t.Errorf("PTTL(%s) = %v, want within [%v, %v]: the boundary, %v away, plus less than a tenth of %v",
				b.Key, ttl, untilBoundary-tolerance, untilBoundary+period/10+time.Millisecond, untilBoundary, period)
		}
		lowest, highest = min(lowest, ttl), max(highest, ttl)
	}
	if spread := highest - lowest; spread < time.Minute {
		t.Errorf("PTTL of %d keys spans %v (%v to %v), want at least a minute of the %v the offsets cover",
			len(buckets), spread, lowest, highest, period/10)
	}
}

// A fixed-window state left from an earlier window counts nothing, so a key
// that outlives its window refuses no request of the next one. The control is
// the same full count stamped with the current window's start, which refuses.
func TestAFixedWindowStateFromAnEarlierWindowCountsNothing(t *testing.T) {
	c := client(t)
	r := redisstore.New(c)

	const period = time.Hour
	waitOutBoundary(period, tolerance)
	now, err := c.Time(t.Context()).Result()
	if err != nil {
		t.Fatalf("TIME: %v", err)
	}
	start := now.Truncate(period)

	uniq := fmt.Sprintf("stale:{%d}", time.Now().UnixNano())
	window := algo.Window{Requests: 5, Period: period}
	earlier := store.Bucket{Key: uniq + ":earlier", Algorithm: algo.FixedWindowID, Cost: 1, Window: window}
	current := store.Bucket{Key: uniq + ":current", Algorithm: algo.FixedWindowID, Cost: 1, Window: window}
	// The script stores a fixed window as "<start in microseconds>:<count>".
	for key, state := range map[string]string{
		earlier.Key: fmt.Sprintf("%d:5", start.Add(-period).UnixMicro()),
		current.Key: fmt.Sprintf("%d:5", start.UnixMicro()),
	} {
		if err := c.Set(t.Context(), key, state, period).Err(); err != nil {
			t.Fatalf("SET %s %s: %v", key, state, err)
		}
	}

	v, err := r.Peek(t.Context(), []store.Bucket{earlier, current})
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if !v[0].Allowed || v[0].Remaining != 5 {
		t.Errorf("Peek verdict of %s, full in the previous window = %+v, want allowed with 5 remaining",
			earlier.Key, v[0])
	}
	if v[1].Allowed || v[1].Remaining != 0 {
		t.Errorf("Peek verdict of %s, full in the current window = %+v, want refused with 0 remaining",
			current.Key, v[1])
	}
}

// waitOutBoundary keeps a test clear of the next boundary of a fixed window
// of the given period, best effort on the local clock: a window's state can
// expire as soon as its boundary passes, so a decision taken within a round
// trip of it may leave nothing to read back, and a run that straddles one
// counts in two windows. margin is how close to the boundary the test may
// start.
func waitOutBoundary(period, margin time.Duration) {
	now := time.Now()
	boundary := now.Truncate(period).Add(period)
	if wait := boundary.Sub(now); wait < margin {
		time.Sleep(wait + margin/10)
	}
}

// A cursor is opaque to the caller and checked by the store: text that is not
// a cursor, and a cursor naming a node this store does not have, are both
// refused with store.ErrBadCursor rather than resumed anywhere.
func TestScan_rejectsACursorItDidNotMint(t *testing.T) {
	r := redisstore.New(client(t))
	prefix := fmt.Sprintf("cursor:{%d}:", time.Now().UnixNano())

	for _, cursor := range []string{"not-a-cursor", "10.0.0.1:1@5"} {
		_, _, err := r.Scan(t.Context(), prefix, cursor, 10)
		if !errors.Is(err, store.ErrBadCursor) {
			t.Errorf("Scan(%q, %q, 10) = %v, want store.ErrBadCursor", prefix, cursor, err)
		}
	}
}

// Keys without a hash tag spread over the slots, and on a Cluster over the
// masters; a walk of such a prefix has to reach every master, or a key on
// the second one is reported as absent. On a standalone store the same walk
// reads its one node.
func TestScan_anUntaggedPrefixReachesEveryMaster(t *testing.T) {
	r := redisstore.New(client(t))
	prefix := fmt.Sprintf("untagged:%d:", time.Now().UnixNano())

	want := make([]string, 0, 12)
	for i := range 12 {
		b := store.Bucket{Key: fmt.Sprintf("%s%02d", prefix, i), Algorithm: algo.GCRAID, Cost: 1,
			Window: algo.Window{Requests: 10, Period: time.Hour, Burst: 10}}
		// One bucket per decision: the keys are on different slots on purpose,
		// and a decision spanning two slots is refused with CROSSSLOT.
		if _, err := r.Decide(t.Context(), []store.Bucket{b}); err != nil {
			t.Fatalf("Decide(%q): %v", b.Key, err)
		}
		want = append(want, b.Key)
	}

	var seen []string
	cursor := ""
	for step := 0; ; step++ {
		keys, next, err := r.Scan(t.Context(), prefix, cursor, 4)
		if err != nil {
			t.Fatalf("Scan(%q, %q, 4): %v", prefix, cursor, err)
		}
		seen = append(seen, keys...)
		if next == "" {
			break
		}
		if step > 100000 {
			t.Fatalf("Scan(%q) walk did not end; the last cursor is %q", prefix, next)
		}
		cursor = next
	}
	for _, k := range want {
		if !slices.Contains(seen, k) {
			t.Errorf("Scan(%q) walk = %v, missing %q", prefix, seen, k)
		}
	}
}

// requireCluster fails the test when REDIS_REQUIRE_CLUSTER is set and the
// server at addr runs with cluster support disabled. A cluster run that
// reached a standalone server passes every test it exists to fail.
func requireCluster(t testing.TB, c goredis.UniversalClient, addr string) {
	t.Helper()
	if os.Getenv("REDIS_REQUIRE_CLUSTER") == "" {
		return
	}
	info, err := c.Info(t.Context(), "cluster").Result()
	if err != nil {
		t.Fatalf("read the cluster section of INFO at %s: %v", addr, err)
	}
	if !strings.Contains(info, "cluster_enabled:1") {
		t.Fatalf("REDIS_REQUIRE_CLUSTER is set, but the Redis at %s is not a cluster", addr)
	}
}
