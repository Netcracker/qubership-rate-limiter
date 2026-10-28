# Load testing

The kit under [`tests/load`](../tests/load) load-tests the rate limit through a gateway on a cluster of your own and
checks its own results. It installs as one Helm chart beside the rate limit charts:

- a backend that answers 200 to every request of the test, and an `HTTPRoute` from the gateway to it;
- the `RateLimitPolicy` of the gateway's domain: one block per API group, each with a per-user rule over the windows
  you set;
- a Job of JMeter generators. Each one sends READ, LIST, and SEARCH requests of test users at the rates of the load
  steps, builds the JMeter HTML report, and checks the results with `loadkit verify`.

The image, `ghcr.io/netcracker/qubership-rate-limiter-loadtest`, carries JMeter, the plan
[`ratelimit-load.jmx`](../tests/load/jmeter/ratelimit-load.jmx) with its script `pick.groovy`, and `loadkit`. Its
tags follow the operator and service images built from the same commit. The
[load test workflow](../.github/workflows/load-test.yml) runs the whole kit on kind for every pull request that
changes it.

## What a run does

Each generator pod goes through these steps:

1. It gets a token for every test user of its slice: from Keycloak with the password grant, renewed before each token
   expires, or self-signed for a gateway that checks no token.
2. It sends probe requests until the gateway answers one with `x-ratelimit-remaining`. That header shows the route
   works and the rate limit service decides the test paths. The probe waits 300 seconds at most.
3. It runs the plan. The steps start `load.startDelaySeconds` after the chart was rendered, the same moment for every
   generator; a generator ready later starts 15 seconds after it is ready, says so in its log, and its steps lag the
   others'. Requests arrive as a Poisson process at the rate of each step, from a fixed pool of threads with keep-alive
   connections. Each request carries a randomly picked user, group, and operation. The hot users, if any, send to one
   group each at their own rate on top of the steps. Meanwhile the first generator samples the statistics of the calls
   to the rate limit service, which [timing the call](#timing-the-call-to-the-rate-limit-service) describes.
4. It builds the HTML report and runs `loadkit verify`, which writes `summary.md` and `summary.json`.
5. It writes the status of the run to `/results/done`: 0 when the check passed, 1 when a check failed, 2 when the check
   could not run, and 3 when JMeter failed and nothing was checked. It then waits for `/results/release`, or for
   `load.holdSeconds`, so you can copy the results out, and exits with that status. A generator that stops in step 1
   or 2 skips the rest and exits with 2 at once, as [what the check reports](#what-the-check-reports) describes.

A test user is `<tokens.userPrefix><n>`, numbered from 1 to `load.users` and zero-padded to the width of
`load.users`: `svt-user-0001` to `svt-user-2000` by default. The first `load.hotUsers` users are the hot ones. The
users in Keycloak need the same prefix and the same width, as [creating the test users](#creating-the-test-users)
shows.

## Before you start

- **The rate limit is installed.** The namespace runs the operator and the service charts, and the gateway's
  EnvoyFilter sends the domain you name in `gateway.domain`.
- **The test may own the domain's policy.** A domain has one `RateLimitPolicy`, named after it. The chart creates
  it, so the install fails on a namespace whose domain already has one. Install on a stand where the test owns the
  domain, or set `policy.create: false` and add the chart's blocks to the existing policy by hand.
- **The gateway passes the token to the rate limit.** The rule counts per user by the `sub` claim of the bearer
  token. A gateway that drops the `Authorization` header before the rate limit filter, or a Keycloak client that
  issues lightweight access tokens without `sub`, leaves every request undecided, and the probe of step 2 never
  succeeds.
- **The test users exist.** With `tokens.source: keycloak`, the realm needs the test users and a client with Direct
  access grants enabled. [Creating the test users](#creating-the-test-users) shows how. The load test workflow runs
  on [`tests/load/kind/keycloak.yaml`](../tests/load/kind/keycloak.yaml), a Keycloak in dev mode with the realm
  `loadtest` and its public client `loadtest`, and on [`tests/load/kind/values.yaml`](../tests/load/kind/values.yaml),
  a few minutes at low rates; both are a working start for a stand of your own.
- **The gateway keeps the statistics of its calls to the rate limit service.** Istio drops them by default; patch the
  Gateway once as [timing the call](#timing-the-call-to-the-rate-limit-service) shows. Without them the gateway's
  columns of the summary's table of those calls show `-`, and every other check works.
- **Token introspection is part of the measurement.** A gateway that introspects tokens in Keycloak calls it for every
  request it does not answer from a cache, so Keycloak can saturate before the rate limit does. Measure the same steps
  once with the rate limit off, as the baseline scenario does, and compare.

## Creating the test users

`loadkit keycloak-users` creates the test users in a realm through the admin REST API. Each user gets the given
password and a complete profile (a name and a verified email), since Keycloak refuses the password grant to an
incomplete profile. Rerunning the command resets the password of a user that exists. Run it from the image:

```bash
docker run --rm --user "$(id -u)" --entrypoint loadkit -v "$PWD/secrets:/secrets:ro" \
  ghcr.io/netcracker/qubership-rate-limiter-loadtest:<tag> keycloak-users \
  -keycloak-url https://keycloak.example.com -realm <realm> \
  -admin-user <admin> -admin-password-file /secrets/admin-password \
  -users 2000 -password-file /secrets/users-password
```

The `--user` option lets the command read password files that only you can read. The admin user needs the
`manage-users` role of the realm; `-admin-realm` (default `master`) names the realm it logs
in to. Add `-delete` to remove the same users after the test. `-ca-file` trusts a private CA, and `-insecure` skips
the certificate check.

`-users` (default `2000`) and `-prefix` (default `svt-user-`) have to equal `load.users` and `tokens.userPrefix` of
the run. The command and the generator each pad the number to the width of their own count, so users created with
`-users 2000` are `svt-user-0001` to `svt-user-2000`, and a run with `load.users: 500` logs in as `svt-user-001`, a
user that does not exist. Create the users again with the count of the run whenever `load.users` changes its number
of digits.

The client the generators log in through needs Direct access grants enabled. A confidential client also needs its
secret in a Secret that `tokens.keycloak.clientSecret` names. Store the users' password for the chart:

```bash
kubectl create secret generic loadtest-users -n <namespace> --from-file=password=secrets/users-password
```

## Running a test

1. Write a values file for the scenario. The [scenarios](#scenarios) list the values that differ between them.

   ```yaml
   image:
     tag: <tag>
   tokens:
     keycloak:
       url: https://keycloak.example.com
       realm: <realm>
       clientId: <client>
       passwordSecret:
         name: loadtest-users
   load:
     steps:
       - rate: 10
         seconds: 3600
   ```

2. Install the chart into the namespace of the rate limit, and check that the policy is `Ready`:

   ```bash
   helm upgrade --install loadtest tests/load/chart -n <namespace> -f values.yaml
   kubectl wait ratelimitpolicies.ratelimit.netcracker.com/gateway.public -n <namespace> --for=condition=Ready
   ```

3. Wait for the run. A generator logs its summary at the end. `kubectl exec <pod> -- cat /results/done` prints the
   status of the run once it is over. A pod that has exited without that file stopped before the plan ran, and
   `kubectl logs <pod>` gives the reason.

4. Copy the results of every generator out, and release the pods. The chart's notes print the loop:

   ```bash
   kubectl cp -n <namespace> <pod>:/results results/<pod>
   kubectl exec -n <namespace> <pod> -- touch /results/release
   ```

5. Read `results/<pod>/summary.md` for the verdict and `results/<pod>/report/index.html` for the JMeter report.
   Uninstall the chart before the next run: the Job runs once.

The windows count in the counter store across runs. A second run within a day starts from the counters the first one
charged, so the hot users of a rerun are refused early. Wait for the windows to pass, or reset the counters with the
[management API](management-api.md) before a run whose limits are checked.

## Scenarios

The values below differ between the scenarios of a typical test campaign; everything else keeps its default.

| Scenario | Values | What the summary shows |
| --- | --- | --- |
| Baseline without the rate limit | `runtime.enabledPercent: 0` in the rate-limiter chart; here `load.requireDecisions: false` and the steps of the capacity ladder | The latency and the ceiling of the gateway, the backend, and token introspection alone |
| Nominal load | `load.steps` at the expected rate, such as `rate: 10` for 600 requests a minute, for an hour | The latency the rate limit adds at the expected load, and no refusal of a user who keeps to the limits |
| Limits at work | `load.hotUsers: 20`, `load.hotRatePerMinute: 60`, steps of at least 5 hours to fill a daily window of 5000 | Every window of every hot user filled to its requests and not past them, and no false refusal |
| Capacity ladder | `policy.multiplier: 1000`, `load.checkLimits: false`, rising `load.steps` of 10 minutes each, `load.generators` and `load.threads` sized as below | The rate at which latency, errors, or undecided responses appear: the capacity |

The multiplier keeps the work of the counter store per request the same, three windows charged in one script, while
no limit fires. A run with real limits refuses most of its requests at a high rate, and a refusal charges nothing, so
it would report a capacity above the real one.

## What the check reports

`summary.md` has one row per step, measured without the first `load.warmupSeconds` of the step. The run fails when
any of these holds:

| Check | Failure in the summary | Usual cause |
| --- | --- | --- |
| Samples | `the results hold no sample of the background or hot users` | `results.jtl` holds no request of the plan: JMeter sent nothing, or the file comes from another plan |
| Errors, above `-max-error-ratio` (default 0: one such sample fails the run) | `<n> of <m> samples were answered with neither 200 nor 429` | The gateway or the backend failed, or requests timed out |
| Undecided responses, above `-max-undecided-ratio` (default 0; `load.requireDecisions: false` sets it to 1) | `<n> of <m> samples passed the gateway without a rate limit decision` | The rate limit filter timed out, or the service could not reach the counter store, and the gateway let the request through |
| Limits | `<n> sample sequences break the limits` | An instance of a window admitted more than its requests (`over-admission`), or a 429 arrived while no window could have been full (`false refusal`) |
| Generator lag | `step <n> fell <ms> ms behind its schedule at p99, above 1s: give the generator more threads or add generators` | Every thread of a thread group was busy, of `load.threads` or of `load.hotThreads` for the hot users; the step measured the generator, not the rate limit |
| Rate, with `-min-rate-ratio` | `step <n> achieved <rate> requests per second of the <rate> targeted` | The generator could not send the target rate |
| Latency, with `-max-p99` | `step <n> has a p99 of <ms> ms, above <limit>` | The step's response time passed the bound you set |
| Calls to the rate limit service, with `metrics.required` | `step <n> has no statistics of the gateway's calls to the rate limit service: <reason>`, or the same of `the rate limit service's checks` | No sample of the step holds a call of that end; [timing the call](#timing-the-call-to-the-rate-limit-service) lists the reasons |

The limit check needs fixed windows, which start at a multiple of their period since the epoch: a daily window starts
at 00:00 UTC. It allows the generator's clock and the counter store's clock to differ by up to 100 ms (`-skew`). It
reports only what no decision moment within that tolerance explains. The service may have charged the counters for
a request that got no response or an undecided one, so the check counts such a request as a possible admission before
a 429, and never as an admission past a window's requests. `load.checkLimits` passes the windows to the check only
with `policy.algorithm: FixedWindow`. The summary also lists, per window, how many instances reached their
requests, counting only the admissions that cannot fall in a neighboring instance. A hot user that sends more than a
window allows reaches its requests in most instances; the instances cut by the start or the end of the run, and those
a longer window keeps closed, do not. A count of zero means no window was ever seen full, and a run meant to show the
limits at work has not shown them.

`loadkit verify` exits with 1 when a check fails and with 2 when it cannot read its input; `/results/done` and the
pod's exit status carry that value, or 3 when JMeter failed before the check.

A generator that stops before the plan runs, because it got no tokens or the probe of step 2 timed out, also exits
with 2, but at once: it writes no `/results/done` and skips the wait for `/results/release`. Its log gives the
reason. When the probe of step 2 never gets a decided response, it logs:

```text
no response from <url> matched <pattern> within <n> seconds: check the route, the RateLimitPolicy, and the tokens
```

When the password grant fails for a test user, it logs:

```text
loadkit tokens: <n> of <m> users could not log in; first error: user <name>: token endpoint returned <code>: <error> <description>
```

There, `invalid_grant Account is not fully set up` names a user created without a full profile,
`invalid_grant Invalid user credentials` a wrong password or a user that does not exist, such as one whose number
was padded to another width than `load.users` gives, and `unauthorized_client` a client without Direct access grants.

## Timing the call to the rate limit service

The response times of the summary cover the whole request. A second table times the call to the rate limit service in
each step, as the two ends of the call see it: the gateway's Envoy times its gRPC call, the round trip included, and
the service times its own check, the counter store included. The difference is the path between the gateway and the
service, with the mTLS hop through ztunnel in ambient mode. Both count every call through that gateway and service in
the step, not only the test's.

| Column | What it shows |
| --- | --- |
| Gateway calls | The calls the gateway's statistics grew by between the first and the last sample inside the measured part of the step, which can fall up to one sampling interval short of its start and of its end |
| Gateway p50, p90, p99 ms | The bound each quantile lies under. Envoy records whole milliseconds, so `<1` means under a millisecond and `<3` under 3 ms. `past the buckets` means the quantile lies above the largest bucket of the histogram |
| Timeouts | The calls the gateway stopped waiting for at `filter.timeout` of the rate-limiter chart; the summary counts their requests as undecided |
| Service checks | The checks of `gateway.domain` the service's statistics grew by |
| Check p50, p99, mean ms | The service's check time; the quantiles interpolate inside a bucket the way PromQL's `histogram_quantile` does, and the mean is exact |
| Store p99 ms | The counter store's share of the check |

The first generator samples both ends every `metrics.intervalSeconds` through the run. It reaches every gateway pod
on Envoy's statistics port 15090, and every rate limit service pod on its metrics port 8080, through two headless
Services the chart creates: the gateway's in `gateway.namespace`, the service's in the release namespace. It keeps
the samples in `/results/metrics.jsonl` and the reason a source or a pod gave nothing in `/results/metrics.log`. A
step without statistics of one end shows `-` in the columns of that end and the reason under the table. A step that
may miss calls of a pod keeps its numbers and adds the reason under the table. `metrics.required: true` fails a step
without statistics. `metrics.enabled: false` creates no Service and samples nothing. The reasons:

- an address and its error: the first generator could not read that pod, or the pod holds no statistics of the
  cluster or the domain;
- `the <source>'s statistics did not grow in the step`: the pods answered, and no call reached them;
- `the <source> was not sampled`: the chart gave the generator no address of that end;
- `fewer than two samples of the metrics fall inside the step`: the step is shorter than two sampling intervals;
- `the metrics could not be read: <error>`: the check could not read `metrics.jsonl`, and the step has fewer than
  two samples of it;
- `a pod whose histogram has other buckets is left out`, or
  `a pod gave nothing in some samples, so the step may miss calls: <error>`: the step counts the other pods only,
  and its numbers stay in the table.

The gateway keeps these statistics only when its proxy configuration asks for them. Patch the Gateway the test runs
through before the run:

```bash
kubectl patch gateway public-gateway -n <namespace> --type merge --patch-file tests/load/gateway-stats.yaml
kubectl rollout status deployment/public-gateway-istio -n <namespace>
```

[`gateway-stats.yaml`](../tests/load/gateway-stats.yaml) sets two annotations, and Istio copies them to the gateway's
pods, which restart with them. `proxyStatsMatcher` keeps the statistics of the rate limit service's cluster and of the
rate limit filter. `sidecar.istio.io/statsHistogramBuckets` counts the calls in steps of 1 ms up to 10 ms; Envoy's
default buckets start at 0.5, 1, 5, and 10 ms, and a quantile between 1 and 5 ms is all they tell. The patch replaces
a `proxy.istio.io/config` annotation the Gateway already has, so merge the two by hand there. Where a chart such as
mesh-config owns the Gateway, its next upgrade can drop the annotations; `meshConfig.defaultConfig.proxyStatsMatcher`
of istiod keeps the statistics for every proxy instead. The patch keeps the statistics of the cluster
`outbound|9000||rate-limiter.*`; a `metrics.rlsCluster` of another name needs its own. Without the statistics the
gateway's columns show `-` and the reason:

```text
http://<address>:15090/stats/prometheus: no envoy_cluster_upstream_rq_time of cluster outbound|9000||rate-limiter.<namespace>.svc.cluster.local: enable the gateway's statistics of the rate limit service
```

The generators run outside the mesh, so they read both ports in plain HTTP. A `PeerAuthentication` in `STRICT` mode or
a NetworkPolicy can refuse that; the reason under the table then names the connection error. Allow the generators'
pods, or read the same statistics from Prometheus.

Prometheus reads the gateway's statistics from `:15020/stats/prometheus`. To read them by hand, port-forward to
Envoy's `:15090`, the port the generators read, rather than `kubectl exec` into the gateway's pod. Each exec starts a
process in the gateway's container, and that process can push the container into CPU throttling. During a test the
requests that arrive meanwhile then wait in the gateway past `filter.timeout` and count as undecided, although the
rate limit service decides their checks in time.

```bash
kubectl port-forward -n <namespace> <gateway pod> 15090:15090 &
curl -s http://127.0.0.1:15090/stats/prometheus | grep -E 'ratelimit|rate-limiter'
```

- `envoy_cluster_upstream_rq_time` with the `cluster_name` `outbound|9000||rate-limiter.<namespace>.svc.cluster.local`:
  the time of the gateway's call to the service, in milliseconds.
- `envoy_cluster_upstream_rq_timeout` of the same cluster: the calls cut at `filter.timeout`.
- `envoy_cluster_ratelimit_<prefix>_ok`, `_over_limit`, `_error`, and `_failure_mode_allowed`, with the
  `cluster_name` of the test route's cluster: the outcomes of the filter. The prefix is the gateway's name with `_` for
  `-`, such as `public_gateway`. Envoy creates each counter at its first increment, and drops the counters of the test
  route when `helm uninstall` removes the route's cluster.

```promql
histogram_quantile(0.99, sum by (le) (rate(
  envoy_cluster_upstream_rq_time_bucket{cluster_name=~"outbound\\|9000\\|\\|rate-limiter\\..*"}[1m])))
```

## Sizing the generators

- **Threads.** A generator thread holds one request at a time, so a generator needs more than its rate times the
  response time in seconds: 200 threads carry 10000 requests per second at 20 ms. Too few threads show up as generator
  lag, never as a slower rate limit. The hot users send from a thread group of their own, `load.hotThreads` threads
  per generator, 4 by default, and their lag counts in the step's. When a run with hot users lags, size
  `load.hotThreads` by the same rule for their rate, `load.hotUsers` times `load.hotRatePerMinute` / 60 per second
  shared among the generators.
- **Generators.** `load.generators` pods share the rate equally, and each takes its own slice of the users, so every
  user's windows are counted by one generator and checked by it. Each generator's summary covers its own share. For
  the whole run, pass the `results.jtl` of every generator to one `loadkit verify`. Give each step there the rate of
  `load.steps` plus the hot users' rate, `load.hotUsers` times `load.hotRatePerMinute` / 60, and set `-start` to the
  moment in `the schedule starts at <ms> ms since the epoch` of the generators' logs. A generator that started late
  logs a later moment, and its steps do not line up with the others' in that check. Add a generator when more threads
  no longer remove the lag: the generator has run out of CPU.
- **Results.** A sample takes about 250 bytes of `results.jtl`, so a step of 10 minutes at 5000 requests per second
  needs about 750 MB. Raise `load.resultsSizeLimit` with the rate and the length of the steps. The check keeps no
  sample in memory unless it checks the limits, and about 70 bytes per sample when it does: a run of 10 million
  samples with `load.checkLimits` needs about 700 MB of the generator's memory.
- **Backend.** The backend answers every request and must not be the ceiling of the run. A backend pod short of
  CPU fails its readiness probe, and the gateway then answers `503` with the `UH` response flag. The baseline
  scenario shows where the backend's ceiling is; add replicas, or raise `backend.resources` together with
  `backend.workerProcesses`, until the baseline steps run without errors.
- **Placement.** The generators run outside the mesh (`load.podLabels`), the way clients outside the cluster do. Keep
  them off the nodes of the gateway and the rate limit service, or they compete for the CPU they measure.

## Chart values

The [values file](../tests/load/chart/values.yaml) describes every value; the ones a run usually sets:

| Value | Default | What it sets |
| --- | --- | --- |
| `image.tag` | required | The tag of the load test image |
| `gateway.name`, `gateway.namespace` | `public-gateway`, the release namespace | The Gateway the route attaches to |
| `gateway.domain` | `gateway.public` | The rate limit domain of that gateway, and the name of the policy |
| `gateway.host`, `gateway.port` | `<name>-istio.<namespace>.svc.cluster.local`, `8080` | Where the generators connect |
| `route.pathPrefix` | `/ratelimit-loadtest` | The path every request of the test goes under |
| `policy.create` | `true` | Whether the chart creates the domain's policy |
| `policy.groups` | five groups | The API groups: one block of the policy and one path segment each |
| `policy.windows`, `policy.algorithm` | 50 a minute, 1000 an hour, 5000 a day; `FixedWindow` | The windows of the per-user rule |
| `policy.multiplier` | `1` | A factor on the requests of every window |
| `backend.replicas`, `backend.workerProcesses` | `2`, `2` | The backend's pods and the nginx workers of each; keep the workers at the CPU limit |
| `tokens.source` | `keycloak` | `keycloak` or `self-signed` |
| `tokens.keycloak.*` | none | The server, the realm, the client, and the Secrets of the password and the client secret |
| `tokens.keycloak.insecureSkipVerify` | `false` | Whether the generators skip the check of Keycloak's certificate; `tokens.keycloak.caConfigMap` trusts a private CA instead |
| `load.users`, `load.hotUsers`, `load.hotRatePerMinute` | `2000`, `0`, `60` | The test users and the hot ones |
| `load.mix` | READ 60, LIST 30, SEARCH 10 | The shares of the operations |
| `load.steps` | 10 per second for an hour | The load steps, in requests per second summed over the generators |
| `load.warmupSeconds` | `30` | The start of each step left out of its statistics |
| `load.startDelaySeconds` | `120` | How long after the chart is rendered the steps of every generator start |
| `load.generators`, `load.threads` | `1`, `200` | The generator pods and the threads of each |
| `load.hotThreads` | `4` | The threads of each generator that send the requests of its hot users |
| `load.checkLimits`, `load.requireDecisions` | `true`, `true` | Whether the check covers the limits and expects every request decided |
| `load.verifyArgs` | none | More flags of `loadkit verify`, such as `-max-error-ratio 0.001 -max-undecided-ratio 0.01 -min-rate-ratio 0.95 -max-p99 50ms`; without the two ratios one error or one undecided response fails the run |
| `load.holdSeconds` | `3600` | How long a generator keeps its results for `kubectl cp` |
| `metrics.enabled`, `metrics.intervalSeconds` | `true`, `5` | Whether the first generator samples the calls to the rate limit service, and how often |
| `metrics.required` | `false` | Whether a step without the statistics of either end fails the run |
| `metrics.servicePodLabels`, `metrics.servicePort` | `name: rate-limiter`, `8080` | The rate limit service's pods and the port of their metrics |
| `metrics.gatewayPort`, `metrics.rlsCluster` | `15090`, `outbound\|9000\|\|rate-limiter.<namespace>.svc.cluster.local` | Envoy's statistics port on the gateway's pods, and the gateway's cluster of the rate limit service |

## The generator's environment

The entrypoint of the image, [`run.sh`](../tests/load/run.sh), reads its settings from environment variables, and the
chart's Job sets them from the values. Set them yourself to run the image outside the chart; a required variable left
unset stops the generator before its first request.

| Variable | Default | Chart value | What it sets |
| --- | --- | --- | --- |
| `LT_HOST` | required | `gateway.host`, or the gateway's Service when it is empty | The host the generator connects to |
| `LT_PORT`, `LT_SCHEME` | `8080`, `http` | `gateway.port`; the chart does not set `LT_SCHEME` | The port and the scheme of that host |
| `LT_PREFIX` | `/ratelimit-loadtest` | `route.pathPrefix` | The path every request goes under |
| `LT_GROUPS` | `customers,orders,incidents,billing,products` | `policy.groups` | The groups, comma-separated |
| `LT_MIX` | `60,30,10` | `load.mix` | The shares of READ, LIST, and SEARCH |
| `LT_STEPS` | required | `load.steps` | The steps as `rate:seconds` pairs, comma-separated, with the rates summed over the generators |
| `LT_WARMUP_SECONDS` | `30` | `load.warmupSeconds` | The start of each step left out of its statistics |
| `LT_START_AT` | `0` | The time of the render plus `load.startDelaySeconds` | The moment the steps start, in seconds since the epoch; `0`, or a moment no later than 5 seconds after the probe succeeds, starts them 15 seconds after it |
| `LT_USERS`, `LT_USER_PREFIX` | `2000`, `svt-user-` | `load.users`, `tokens.userPrefix` | The test users |
| `LT_HOT_USERS`, `LT_HOT_RATE_PER_MINUTE` | `0`, `60` | `load.hotUsers`, `load.hotRatePerMinute` | The hot users and the rate of each |
| `LT_THREADS`, `LT_HOT_THREADS` | `200`, `4` | `load.threads`, `load.hotThreads` | The threads for the other users and for the hot users |
| `LT_GENERATORS` | `1` | `load.generators` | The generators that share the rate and the users |
| `JOB_COMPLETION_INDEX` | `0` | Set by Kubernetes for the Indexed Job | The index of this generator, from 0 |
| `LT_PREFLIGHT_SECONDS` | `300` | Not set | How long the probe of step 2 waits for the gateway, in seconds |
| `LT_WINDOWS` | empty | `policy.windows` times `policy.multiplier`; empty unless `load.checkLimits` is `true` and `policy.algorithm` is `FixedWindow` | The windows of the limit check as `requests/period` pairs, such as `50/60s`; empty checks no limit |
| `LT_REQUIRE_DECISIONS` | `true` | `load.requireDecisions` | `false` makes the probe wait for a 200, checks no limit, and passes `-max-undecided-ratio 1` to the check |
| `LT_VERIFY_ARGS` | empty | `load.verifyArgs` | More flags of `loadkit verify`, split on spaces |
| `LT_HOLD_SECONDS` | `3600` | `load.holdSeconds` | How long the generator waits for `/results/release` after the run, in seconds |
| `LT_TOKEN_SOURCE` | `keycloak` | `tokens.source` | `keycloak` or `self-signed` |
| `LT_KEYCLOAK_URL`, `LT_KEYCLOAK_REALM`, `LT_KEYCLOAK_CLIENT_ID` | required with `keycloak` | `tokens.keycloak.url`, `tokens.keycloak.realm`, `tokens.keycloak.clientId` | The server root, the realm, and the client the test users log in through |
| `LT_KEYCLOAK_PASSWORD_FILE` | required with `keycloak` | The mounted key of `tokens.keycloak.passwordSecret` | The file with the password of the test users |
| `LT_KEYCLOAK_CLIENT_SECRET_FILE`, `LT_KEYCLOAK_CA_FILE` | empty | The mounted keys of `tokens.keycloak.clientSecret` and `tokens.keycloak.caConfigMap` | The file with the client secret, and the file with the CA certificates in PEM |
| `LT_KEYCLOAK_INSECURE` | `false` | `tokens.keycloak.insecureSkipVerify` | `true` skips the check of Keycloak's certificate |
| `LT_METRICS_GATEWAY`, `LT_METRICS_SERVICE` | empty | The two headless Services, with `metrics.enabled: true` | `host:port` of the gateway pods' statistics and of the service pods' metrics; with both empty, nothing is sampled |
| `LT_METRICS_CLUSTER`, `LT_METRICS_DOMAIN` | empty | `metrics.rlsCluster`, `gateway.domain` | The gateway's cluster of the rate limit service, and the domain whose checks count |
| `LT_METRICS_INTERVAL`, `LT_METRICS_REQUIRED` | `5s`, `false` | `metrics.intervalSeconds`, `metrics.required` | How often the first generator samples, and whether a step without statistics fails |
| `HEAP` | `-Xms1g -Xmx1g -XX:MaxMetaspaceSize=256m`, the default of JMeter | `load.heap` | The memory options of JMeter's JVM |

## Running the plan in your own JMeter

The plan runs in JMeter 5.6.3, the version of the image, with `pick.groovy` in the directory of the plan. Give it
the properties file and the properties of the run:

```bash
jmeter -n -t ratelimit-load.jmx -q loadtest.properties \
  -Jhost=<gateway-host> -Jport=8080 -Jtokens=tokens.csv \
  -Jsteps=10:600,50:600 -JhotSteps=0:1200 -Jthreads=200 -Jduration=1260 \
  -l results.jtl
```

| Property | Meaning |
| --- | --- |
| `host`, `port`, `scheme`, `prefix` | The gateway and the path prefix |
| `tokens` | The CSV that `loadkit tokens` writes: `user,hot,token`, hot users first |
| `groups`, `mix` | The groups, comma-separated, and the shares of READ, LIST, and SEARCH |
| `steps`, `hotSteps` | `rate:seconds` pairs, comma-separated, for the other users and for the hot users |
| `threads`, `hotThreads` | The threads of each thread group |
| `start` | The moment the steps start, in milliseconds since the epoch; unset, the start of the run |
| `duration` | A bound on the run in seconds, past the end of the steps |
| `connectTimeout`, `responseTimeout` | How long a request waits to connect and for its response, in milliseconds: 5000 and 10000 by default. A request past either has no response and counts as an error |

`loadkit tokens` writes the tokens file, from Keycloak or self-signed, and with `-watch` keeps renewing it while the
plan runs. The plan reads the file again within five seconds of a change. `loadkit verify -jtl results.jtl -steps ...
-windows 50/60,1000/3600,5000/86400` checks the results the same way a generator does; give it `-start` when the
steps did not start with the first request. `-max-lag`, one second by default, bounds the generator lag;
`-max-error-ratio` and `-max-undecided-ratio`, 0 by default, bound the shares of errors and undecided responses. Run
either command with `-h` for its flags.
