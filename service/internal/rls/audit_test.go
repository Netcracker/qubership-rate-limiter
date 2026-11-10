package rls

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	envoyratelimit "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	engine "github.com/netcracker/qubership-rate-limiter/engine"
	"github.com/netcracker/qubership-rate-limiter/engine/compile"
	"github.com/netcracker/qubership-rate-limiter/engine/model"
	"github.com/netcracker/qubership-rate-limiter/engine/store/memory"
	"github.com/netcracker/qubership-rate-limiter/internal/metrics"
	"github.com/netcracker/qubership-rate-limiter/service/internal/store"
)

func auditLines(log *recorder, message string) []string {
	var lines []string
	for line := range strings.SplitSeq(log.output(), "\n") {
		if strings.HasPrefix(line, message+" ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestRefusalAudit_groupsByDomainRuleSubjectAndAxes(t *testing.T) {
	var audit refusalAudit
	log := &recorder{}
	values := map[string]string{"tenant": "a"}
	base := auditKey{domain: "public", rule: "b/r", sub: "alice", axesFingerprint: fingerprintAxes(values)}
	first := auditSample{path: "/first", method: "GET", requestID: "first-id", window: "gcra:60", axisValues: values}
	audit.record(auditBatch{base: first})
	otherDomain, otherRule, otherSub, otherAxes := base, base, base, base
	otherDomain.domain, otherRule.rule, otherSub.sub = "private", "b/s", "bob"
	later := auditSample{path: "/later", method: "POST", requestID: "later-id", window: "fixedwindow:3600",
		costExceedsCapacity: true, axisValues: values}
	otherSample := later
	otherSample.axisValues = map[string]string{"tenant": "b"}
	otherAxes.axesFingerprint = fingerprintAxes(otherSample.axisValues)
	audit.record(auditBatch{base: later, otherDomain: later, otherRule: later, otherSub: later, otherAxes: otherSample})

	audit.flush(log)

	lines := auditLines(log, refusalAuditMessage)
	require.Len(t, lines, 5)
	for _, line := range lines {
		if strings.Contains(line, "refusals=2 ") {
			assert.Contains(t, line, `domain=public rule=b/r window=fixedwindow:3600 sub=alice `+
				`cause=cost_exceeds_capacity costExceedsCapacity=1 axes={"tenant":"a"} path=/first method=GET`)
			assert.Contains(t, line, "requestId=first-id")
			return
		}
	}
	t.Errorf("audit lines = %v, want one line for both refusals of the same key", lines)
}

func TestRefusalAudit_limitsKeysAndReportsOverflowOnce(t *testing.T) {
	var audit refusalAudit
	log := &recorder{}
	dropped := valueOf(metrics.AuditDropped)
	before := dropped()
	for i := range maxAuditKeys {
		audit.record(auditBatch{{sub: strconv.Itoa(i)}: {}})
	}
	for _, sub := range []string{"extra-a", "extra-b", "extra-a", "0"} {
		audit.record(auditBatch{{sub: sub}: {}})
	}
	audit.flush(log)

	assert.Len(t, auditLines(log, refusalAuditMessage), maxAuditKeys)
	assert.Contains(t, log.output(), `sub=0 axes={} path="" method="" refusals=2`)
	assert.Equal(t, []string{refusalOverflowMessage + " keysDropped=2 countCapped=false"},
		auditLines(log, refusalOverflowMessage))
	assert.Equal(t, float64(3), dropped()-before)

	next := &recorder{}
	audit.record(auditBatch{{sub: "extra-a"}: {}})
	audit.flush(next)
	assert.Len(t, auditLines(next, refusalAuditMessage), 1)
	assert.Empty(t, auditLines(next, refusalOverflowMessage))
}

func TestRefusalAudit_reportsALowerBoundWhenOverflowCountFills(t *testing.T) {
	var audit refusalAudit
	log := &recorder{}
	for i := range 2*maxAuditKeys + 2 {
		audit.record(auditBatch{{sub: strconv.Itoa(i)}: {}})
	}
	audit.flush(log)

	assert.Equal(t, []string{fmt.Sprintf("%s keysDropped=%d countCapped=true", refusalOverflowMessage, maxAuditKeys+1)},
		auditLines(log, refusalOverflowMessage))
}

func TestRefusalAudit_reportsOverflowAtTheSerializedAxesBudget(t *testing.T) {
	var audit refusalAudit
	log := &recorder{}
	values := make(map[string]string)
	for i := range 8 {
		values["axis"+strconv.Itoa(i)] = strings.Repeat("v", maxLoggedValueLength)
	}
	axes := auditAxes(values)
	fit := maxAuditAxesBytes / len(axes.values)
	require.Less(t, fit+1, maxAuditKeys)
	dropped := valueOf(metrics.AuditDropped)
	before := dropped()
	key := auditKey{axesFingerprint: fingerprintAxes(values)}
	for i := range fit + 1 {
		key.sub = strconv.Itoa(i)
		audit.record(auditBatch{key: {axisValues: values}})
	}
	audit.record(auditBatch{key: {axisValues: values}})
	audit.flush(log)

	assert.Len(t, auditLines(log, refusalAuditMessage), fit)
	assert.Equal(t, []string{refusalOverflowMessage + " keysDropped=1 countCapped=false"},
		auditLines(log, refusalOverflowMessage))
	assert.Equal(t, float64(2), dropped()-before)
}

func TestRefusalAudit_omitsOversizedAxesButRetainsTheirFingerprint(t *testing.T) {
	values := make(map[string]string)
	for i := range 40 {
		values["axis"+strconv.Itoa(i)] = strings.Repeat("v", maxLoggedValueLength)
	}
	var audit refusalAudit
	log := &recorder{}
	for _, value := range []string{"first", "second"} {
		values["axis0"] = value
		audit.record(auditBatch{{axesFingerprint: fingerprintAxes(values)}: {axisValues: values}})
	}
	audit.flush(log)
	lines := auditLines(log, refusalAuditMessage)
	require.Len(t, lines, 2)
	for _, line := range lines {
		assert.Contains(t, line, "axesFingerprint=")
		assert.Contains(t, line, `axesOmitted=true axes={}`)
		assert.Contains(t, line, "refusals=1 ")
	}
}

func TestAuditAxes_stripsUnicodeControlCharacters(t *testing.T) {
	axes := auditAxes(map[string]string{"path": "/a\u0085b\u009bc\nd"})
	assert.Equal(t, loggedAxes{values: `{"path":"/abcd"}`, changed: true}, axes)
}

func TestAuditAxes_appliesTheRecordBudgetAfterJSONEscaping(t *testing.T) {
	values := make(map[string]string)
	for i := range 8 {
		values["axis"+strconv.Itoa(i)] = strings.Repeat("&", maxLoggedValueLength)
	}
	assert.Equal(t, loggedAxes{values: "{}", omitted: true}, auditAxes(values))
}

func TestRefusalAudit_writesTheIntervalWhileTheServiceIsRunning(t *testing.T) {
	var audit refusalAudit
	log := &recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	audit.record(auditBatch{{sub: "alice"}: {}})
	go func() {
		defer close(done)
		audit.run(ctx, log, 10*time.Millisecond)
	}()

	require.Eventually(t, func() bool {
		return len(auditLines(log, refusalAuditMessage)) == 1
	}, time.Second, 5*time.Millisecond)
}

func TestShouldRateLimit_auditsEnforcedRefusalsOncePerCheck(t *testing.T) {
	p := perClientPerHourPolicy(1)
	shadow := p.Blocks[0].Rules[0]
	shadow.Name, shadow.Behavior = "shadow", model.BehaviorShadow
	p.Blocks[0].Rules = append(p.Blocks[0].Rules, shadow)
	server, log := newServerOver(ruleSetWith(t, p))
	token := tokenWithSub("Alice")
	first := map[string]string{
		"token": token, "path": "/first?secret=hidden", "method": "GET", "request_id": "first-id",
	}
	later := map[string]string{"token": token, "path": "/later", "method": "POST", "request_id": "later-id"}
	const domain = "gateway.public"
	require.Equal(t, envoyratelimit.RateLimitResponse_OK,
		shouldRateLimit(t, server, request(domain, first)).GetOverallCode())
	require.Equal(t, envoyratelimit.RateLimitResponse_OVER_LIMIT,
		shouldRateLimit(t, server, request(domain, first, later)).GetOverallCode())
	server.audit.flush(log)

	lines := auditLines(log, refusalAuditMessage)
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], `rule=b/each window=gcra:3600 sub=alice axes={"sub":"alice"}`)
	assert.Contains(t, lines[0], `path=/first?[redacted] method=GET refusals=1`)
	assert.Contains(t, lines[0], "requestId=first-id")
	assert.NotContains(t, lines[0], token)
	assert.NotContains(t, lines[0], "hidden")
}

func TestShouldRateLimit_truncatesLongAxisValuesAndKeepsBucketsSeparate(t *testing.T) {
	p := domainWidePolicy(1, time.Hour)
	p.Blocks[0].Rules[0].Counters = []string{model.KeyPath}
	server, log := newServerOver(ruleSetWith(t, p))
	prefix := "/" + strings.Repeat("a", 120<<10)
	for _, suffix := range []string{"one", "two"} {
		req := request("gateway.public", map[string]string{"path": prefix + suffix, "request_id": suffix})
		require.Equal(t, envoyratelimit.RateLimitResponse_OK, shouldRateLimit(t, server, req).GetOverallCode())
		require.Equal(t, envoyratelimit.RateLimitResponse_OVER_LIMIT, shouldRateLimit(t, server, req).GetOverallCode())
	}
	server.audit.flush(log)

	lines := auditLines(log, refusalAuditMessage)
	require.Len(t, lines, 2)
	wantAxes := `axes={"path":"/` + strings.Repeat("a", maxLoggedValueLength-1) + `[truncated]"}`
	for _, line := range lines {
		assert.Contains(t, line, wantAxes)
		assert.Contains(t, line, "axesFingerprint=")
		assert.Contains(t, line, "refusals=1 ")
		assert.Less(t, len(line), 2048)
	}
}

func TestShouldRateLimit_auditsRefusalsWithoutACounterVerdict(t *testing.T) {
	const domain = "gateway.public"
	p := &model.Policy{Domain: domain, Blocks: []model.Block{{
		Name: "b", Rules: rulesFillingTheBucketBudget(model.KeySub),
	}}}
	snap, problems := compile.Compile(testNamespace, domain, p)
	require.Empty(t, problems)
	snap.Blocks = append(snap.Blocks, snap.Blocks[0])
	server, log := newServerOver(store.NewRuleSet(map[string]store.Domain{
		domain: {Engine: engine.New(snap, memory.New()), Snapshot: snap},
	}))
	invalidCost := request(domain, map[string]string{"sub": "alice"})
	invalidCost.HitsAddend = maxHitsAddend + 1
	for _, req := range []*envoyratelimit.RateLimitRequest{
		checkOfClients(maxDescriptorsPerCheck + 1), invalidCost, request(domain, map[string]string{"sub": "alice"}),
	} {
		require.Equal(t, envoyratelimit.RateLimitResponse_OVER_LIMIT, shouldRateLimit(t, server, req).GetOverallCode())
	}
	server.audit.flush(log)

	lines := auditLines(log, refusalAuditMessage)
	require.Len(t, lines, 3)
	for _, fields := range []string{
		"sub=client-0 cause=too_many_descriptors", "sub=alice cause=invalid_cost", "sub=alice cause=too_many_buckets",
	} {
		assert.Contains(t, strings.Join(lines, "\n"), fields+" axes={}")
	}
}

func TestShouldRateLimit_auditsACostThatExceedsWindowCapacity(t *testing.T) {
	server, log := newServerOver(ruleSetWith(t, domainWidePolicy(1, time.Hour)))
	req := request("gateway.public")
	req.HitsAddend = 2
	require.Equal(t, envoyratelimit.RateLimitResponse_OVER_LIMIT, shouldRateLimit(t, server, req).GetOverallCode())
	server.audit.flush(log)

	lines := auditLines(log, refusalAuditMessage)
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "rule=b/all window=gcra:3600 cause=cost_exceeds_capacity costExceedsCapacity=1 axes={}")
	assert.NotContains(t, lines[0], "sub=")
}
