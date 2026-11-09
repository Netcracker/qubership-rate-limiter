# Security audit

The rate limiter writes security audit events at info level in the service log. The events record enforced rate limit
refusals and management operations on counters.

## Configuration

Use `LOG_LEVEL: info` or `LOG_LEVEL: debug` on the service chart to collect these events. Route messages that start with
`rate limit exceeded`, `rate limit audit overflow`, or `management mutation` to the security audit destination. These
prefixes follow the platform log envelope. The examples show the message portion unless a field comes from the envelope.

## Example events

### Rate limit exceeded

```text
rate limit exceeded domain=gateway.public rule=api/per-user window=gcra:60 sub=alice axes={"sub":"alice"} path=/api/v1/orders method=GET refusals=412 first=2026-11-08T10:00:00.123Z last=2026-11-08T10:00:09.871Z requestId=5f0c491a
```

Each service replica writes one record per 10-second interval for each combination of `domain`, `rule` (`block/rule`),
resolved `sub`, and bucket `axes`. A check counts once per combination. A check refused by multiple enforcing rules
can appear in several records. Shadow verdicts stay in the decision metrics.

| Field | Meaning |
| --- | --- |
| `window` | Algorithm and period in seconds of the strictest bucket for the first refusal. If a cost exceeds capacity, this names the window of the first such refusal. |
| `sub` | Subject resolved from the token or a direct descriptor under the domain's key mapping. Omitted when the check has no single resolved subject. |
| `axes` | JSON object with the values used to address the refusing bucket. A bucket with no axes has `{}`. |
| `path`, `method`, `requestId` | Values from the first refusal of the interval. The path's query string is redacted. |
| `refusals`, `first`, `last` | Number of checks refused for the key and UTC times of the first and last refusal. |
| `cause`, `costExceedsCapacity` | For a cost that exceeds the window's capacity, `cause=cost_exceeds_capacity` and the count of such checks within `refusals`. |

Hard refusals carry `cause=too_many_descriptors`, `cause=too_many_buckets`, or `cause=invalid_cost` and use the cause in
place of the rule in the aggregation key. These records omit `rule` and `window` and carry `axes={}`. Request-wide
violations use identity and request fields from the first descriptor. The service does not log the bearer token.

### Rate limit audit overflow

```text
rate limit audit overflow keysDropped=24 countCapped=false
```

A replica buffers up to 1,000 audit keys and 1 MiB of serialized axes in an interval. The overflow line reports how
many distinct keys it omitted. `countCapped=true` indicates a lower bound of 1,001 keys. The counter
`ratelimit_audit_dropped_total` counts omitted contributions, including repeat refusals for an omitted key. On graceful
shutdown, each replica writes the pending interval after draining its in-flight checks.

### Management mutation

```text
[request_id=ac2d9341] management mutation subject=system:serviceaccount:core:operator idempotencyKey=reset-49 domain=gateway.public endpoint=counters ruleId=api/per-user axes={"sub":"alice"} dryRun=false outcome=reset count=1
```

The management API records the authenticated caller, idempotency key, target domain and rule, JSON axis selection,
preview flag, and result of a single-counter mutation. An accepted bulk reset records the command and its selection.
The platform log envelope supplies `request_id` in both cases. See [Management API](management-api.md).

```text
[request_id=b7033cb8] management mutation accepted subject=system:serviceaccount:core:operator idempotencyKey=preview-50 domain=gateway.public endpoint=counter-resets command=preview-domain selection=domain dryRun=true selector=domain-wide
```
