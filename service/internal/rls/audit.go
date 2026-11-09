package rls

import (
	"context"
	"encoding/json"
	"hash/maphash"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	envoyratelimit "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"

	engine "github.com/netcracker/qubership-rate-limiter/engine"
	"github.com/netcracker/qubership-rate-limiter/internal/metrics"
)

const (
	auditInterval           = 10 * time.Second
	maxAuditKeys            = 1000
	maxAuditAxesBytes       = 1 << 20
	maxAuditRecordAxesBytes = 8 << 10
	costExceedsCapacity     = "cost_exceeds_capacity"
	refusalAuditMessage     = "rate limit exceeded"
	refusalOverflowMessage  = "rate limit audit overflow"
)

type auditKey struct {
	domain, rule, sub, cause string
	axes                     loggedAxes
}

type auditSample struct {
	path, method, requestID, window string
	costExceedsCapacity             bool
}

type auditBatch map[auditKey]auditSample

// addRequestViolation attributes a request-level violation to the first descriptor.
func (b *auditBatch) addRequestViolation(req *envoyratelimit.RateLimitRequest, eng *engine.Engine, cause string) {
	var c check
	if descriptors := req.GetDescriptors(); len(descriptors) > 0 {
		c = engineRequest(descriptors[0], 0)
	}
	b.addViolation(req.GetDomain(), cause, c, eng)
}

func (b *auditBatch) addDecision(domain string, c check, decision engine.Decision) {
	sample := c.auditSample()
	for _, rule := range decision.Rules {
		if rule.Allowed || rule.Shadow {
			continue
		}
		key := auditKey{domain: domain, rule: metrics.RuleID(rule.Block, rule.Rule), sub: decision.Subject,
			axes: auditAxes(rule.Axes)}
		sample.window = rule.Algorithm + ":" + strconv.FormatInt(rule.PeriodSeconds, 10)
		sample.costExceedsCapacity = rule.CostExceedsCapacity
		b.add(key, sample)
	}
}

func (b *auditBatch) addViolation(domain, cause string, c check, eng *engine.Engine) {
	b.add(auditKey{domain: domain, sub: eng.Subject(c.Request), axes: auditAxes(nil), cause: cause}, c.auditSample())
}

func (b *auditBatch) add(key auditKey, sample auditSample) {
	if *b == nil {
		*b = make(auditBatch)
	}
	if first, exists := (*b)[key]; exists {
		if sample.costExceedsCapacity && !first.costExceedsCapacity {
			first.window = sample.window
			first.costExceedsCapacity = true
			(*b)[key] = first
		}
		return
	}
	(*b)[key] = sample
}

func (c check) auditSample() auditSample {
	return auditSample{path: sanitizePath(c.Path), method: sanitizeValue(c.Method), requestID: sanitizeValue(c.requestID)}
}

type loggedAxes struct {
	values      string
	fingerprint uint64
	omitted     bool
}

func auditAxes(axes map[string]string) loggedAxes {
	if len(axes) == 0 {
		return loggedAxes{values: "{}"}
	}
	logged := make(map[string]string, len(axes))
	changed := false
	for name, value := range axes {
		logged[name] = auditAxisValue(value)
		changed = changed || logged[name] != value
	}
	// A map of string values always marshals. JSON sorts its keys and escapes controls.
	encoded, _ := json.Marshal(logged)
	if len(encoded) > maxAuditRecordAxesBytes {
		return loggedAxes{values: "{}", fingerprint: fingerprintAxes(axes), omitted: true}
	}
	result := loggedAxes{values: string(encoded)}
	if changed {
		result.fingerprint = fingerprintAxes(axes)
	}
	return result
}

func auditAxisValue(value string) string {
	if len(value) <= maxLoggedValueLength {
		return sanitizeValue(value)
	}
	prefix := strings.ToValidUTF8(value[:maxLoggedValueLength], "")
	return sanitizeValue(prefix) + valueTruncated
}

var axisFingerprintSeed = maphash.MakeSeed()

func fingerprintAxes(axes map[string]string) uint64 {
	var fingerprint uint64
	// Include each name with its value. XOR makes the result independent of map iteration order.
	for name, value := range axes {
		fingerprint ^= maphash.Comparable(axisFingerprintSeed, [2]string{name, value})
	}
	return fingerprint
}

type auditEntry struct {
	auditSample
	first, last      time.Time
	refusals         uint64
	costExceedsCount uint64
}

func (e *auditEntry) add(sample auditSample, now time.Time) {
	if sample.costExceedsCapacity {
		if e.costExceedsCount == 0 {
			e.window = sample.window
		}
		e.costExceedsCount++
	}
	e.last = now
	e.refusals++
}

type refusalAudit struct {
	mu sync.Mutex

	entries   map[auditKey]auditEntry
	axesBytes int
	dropped   map[uint64]struct{}
	capped    bool
	seed      maphash.Seed
}

func (a *refusalAudit) record(batch auditBatch) {
	if len(batch) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for key, sample := range batch {
		entry, found := a.entries[key]
		if !found {
			if len(a.entries) >= maxAuditKeys || a.axesBytes+len(key.axes.values) > maxAuditAxesBytes {
				a.drop(key)
				continue
			}
			if a.entries == nil {
				a.entries = make(map[auditKey]auditEntry)
			}
			a.axesBytes += len(key.axes.values)
			entry = auditEntry{auditSample: sample, first: now}
		}
		entry.add(sample, now)
		a.entries[key] = entry
	}
}

func (a *refusalAudit) drop(key auditKey) {
	metrics.AuditDropped.Inc()
	if a.capped {
		return
	}
	if a.dropped == nil {
		a.dropped = make(map[uint64]struct{})
		a.seed = maphash.MakeSeed()
	}
	hash := maphash.Comparable(a.seed, key)
	if _, seen := a.dropped[hash]; seen {
		return
	}
	if len(a.dropped) == maxAuditKeys {
		a.capped = true
		return
	}
	a.dropped[hash] = struct{}{}
}

func (a *refusalAudit) run(ctx context.Context, log Logger, interval time.Duration) {
	for {
		now := time.Now()
		next := now.Truncate(interval).Add(interval)
		select {
		case <-ctx.Done():
			a.flush(log)
			return
		case <-time.After(next.Sub(now)):
			a.flush(log)
		}
	}
}

func (a *refusalAudit) flush(log Logger) {
	a.mu.Lock()
	entries, dropped, capped := a.entries, len(a.dropped), a.capped
	a.entries, a.dropped = nil, nil
	a.axesBytes, a.capped = 0, false
	a.mu.Unlock()

	ctx := context.Background()
	for key, entry := range entries {
		entry.write(ctx, log, key)
	}
	if dropped != 0 {
		if capped {
			dropped++
		}
		log.InfoC(ctx, "%s keysDropped=%d countCapped=%t", refusalOverflowMessage, dropped, capped)
	}
}

func (e auditEntry) write(ctx context.Context, log Logger, key auditKey) {
	fields := ""
	if key.rule != "" {
		fields += " rule=" + key.rule + " window=" + e.window
	}
	if key.sub != "" {
		fields += " sub=" + auditValue(key.sub)
	}
	if key.cause != "" {
		fields += " cause=" + key.cause
	}
	if e.costExceedsCount > 0 {
		fields += " cause=" + costExceedsCapacity + " costExceedsCapacity=" + strconv.FormatUint(e.costExceedsCount, 10)
	}
	if key.axes.fingerprint != 0 {
		fields += " axesFingerprint=" + strconv.FormatUint(key.axes.fingerprint, 16)
	}
	if key.axes.omitted {
		fields += " axesOmitted=true"
	}
	log.InfoC(ctx, "%s domain=%s%s axes=%s path=%s method=%s refusals=%d first=%s last=%s requestId=%s",
		refusalAuditMessage, key.domain, fields, key.axes.values, auditValue(e.path), auditValue(e.method), e.refusals,
		e.first.UTC().Format(time.RFC3339Nano), e.last.UTC().Format(time.RFC3339Nano), auditValue(e.requestID))
}

func auditValue(value string) string {
	if value == "" || strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '=' || r == '"' || r == '\\'
	}) {
		return strconv.QuoteToASCII(value)
	}
	return value
}
