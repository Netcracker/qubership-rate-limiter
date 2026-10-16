package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// step is one load step: a target rate held for a duration.
type step struct {
	rate     float64 // requests per second
	duration time.Duration
}

// window is one fixed window of the per-user rule.
type window struct {
	requests int64
	period   time.Duration
}

func (w window) String() string { return fmt.Sprintf("%d/%s", w.requests, w.period) }

type verifyConfig struct {
	steps []step
	// start is the moment the schedule began, in milliseconds since the
	// epoch; 0 takes the start of the earliest sample.
	start             int64
	warmup            time.Duration
	windows           []window
	skew              time.Duration
	maxErrorRatio     float64
	maxUndecidedRatio float64
	minRateRatio      float64
	maxP99            time.Duration
	maxLag            time.Duration
}

// verifyCommand reads one or more JMeter CSV results files of one run,
// writes summary.json and summary.md into -out, prints the Markdown summary,
// and returns errChecksFailed when a check fails. It reads the files twice,
// once for the start of the run unless -start gives it, and keeps statistics
// rather than samples, so its memory grows with the users and groups whose
// limits it checks, not with the length of the run.
func verifyCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var jtls stringList
	fs.Var(&jtls, "jtl", "JMeter CSV results file; repeat the flag for the files of several generators")
	stepsFlag := fs.String("steps", "", "load steps as rate:duration pairs, comma-separated, such as 20:2m,50:2m; "+
		"a rate counts requests per second over all the files")
	start := fs.Int64("start", 0, "moment the schedule began, in milliseconds since the epoch; "+
		"0 takes the start of the earliest sample")
	warmup := fs.Duration("warmup", 0, "start of each step left out of its statistics")
	windowsFlag := fs.String("windows", "", "fixed windows of the per-user rule as requests/period, comma-separated, "+
		"such as 50/1m,1000/1h,5000/24h; empty skips the limit check")
	skew := fs.Duration("skew", 100*time.Millisecond,
		"largest clock difference between the generator and the counter store")
	maxErrorRatio := fs.Float64("max-error-ratio", 0,
		"largest share of samples answered with neither 200 nor 429")
	maxUndecidedRatio := fs.Float64("max-undecided-ratio", 0,
		"largest share of samples the gateway let through without a decision")
	minRateRatio := fs.Float64("min-rate-ratio", 0,
		"smallest achieved rate of a step as a share of its target; 0 skips the check")
	maxP99 := fs.Duration("max-p99", 0, "largest 99th percentile of a step's response time; 0 skips the check")
	maxLag := fs.Duration("max-lag", time.Second, "largest 99th percentile of a step's lag behind the schedule; "+
		"a larger lag means the generator ran out of threads or CPU, and the step measured the generator")
	out := fs.String("out", ".", "directory to write summary.json and summary.md into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case len(jtls) == 0:
		return errors.New("no -jtl file given")
	case *skew < 0:
		return fmt.Errorf("-skew is %v; want 0 or more", *skew)
	case *start < 0:
		return fmt.Errorf("-start is %d; want milliseconds since the epoch", *start)
	}
	steps, err := parseSteps(*stepsFlag)
	if err != nil {
		return err
	}
	for i, s := range steps {
		if *warmup >= s.duration {
			return fmt.Errorf("-warmup %v leaves nothing of step %d, which lasts %v", *warmup, i+1, s.duration)
		}
	}
	windows, err := parseWindows(*windowsFlag)
	if err != nil {
		return err
	}
	cfg := verifyConfig{
		steps: steps, start: *start, warmup: *warmup, windows: windows, skew: *skew,
		maxErrorRatio: *maxErrorRatio, maxUndecidedRatio: *maxUndecidedRatio,
		minRateRatio: *minRateRatio, maxP99: *maxP99, maxLag: *maxLag,
	}

	readAll := func(fn func(sample)) error {
		for _, path := range jtls {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			err = readJTL(f, fn)
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		return nil
	}
	if cfg.start == 0 {
		if err := readAll(func(s sample) {
			if cfg.start == 0 || s.start < cfg.start {
				cfg.start = s.start
			}
		}); err != nil {
			return err
		}
	}
	acc := newAccumulator(cfg)
	if err := readAll(acc.add); err != nil {
		return err
	}
	rep := acc.report()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	js, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "summary.json"), append(js, '\n'), 0o644); err != nil {
		return err
	}
	md := rep.markdown()
	if err := os.WriteFile(filepath.Join(*out, "summary.md"), []byte(md), 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprint(stdout, md)
	if !rep.Passed {
		return fmt.Errorf("%w: %s", errChecksFailed, strings.Join(rep.Failures, "; "))
	}
	return nil
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// parseSteps parses rate:duration pairs; a duration is a Go duration or a
// number of seconds.
func parseSteps(v string) ([]step, error) {
	if v == "" {
		return nil, errors.New("-steps is empty")
	}
	var steps []step
	for part := range strings.SplitSeq(v, ",") {
		rate, dur, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			return nil, fmt.Errorf("step %q is not rate:duration", part)
		}
		r, err := strconv.ParseFloat(rate, 64)
		if err != nil || r < 0 {
			return nil, fmt.Errorf("step %q: rate %q is not a non-negative number", part, rate)
		}
		d, err := parseDuration(dur)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("step %q: duration %q is not a positive duration", part, dur)
		}
		steps = append(steps, step{rate: r, duration: d})
	}
	return steps, nil
}

// parseWindows parses requests/period pairs; a period is a Go duration or a
// number of seconds, and a whole number of milliseconds.
func parseWindows(v string) ([]window, error) {
	if v == "" {
		return nil, nil
	}
	var windows []window
	for part := range strings.SplitSeq(v, ",") {
		req, per, ok := strings.Cut(strings.TrimSpace(part), "/")
		if !ok {
			return nil, fmt.Errorf("window %q is not requests/period", part)
		}
		n, err := strconv.ParseInt(req, 10, 64)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("window %q: requests %q is not a positive integer", part, req)
		}
		d, err := parseDuration(per)
		if err != nil || d < time.Millisecond || d%time.Millisecond != 0 {
			return nil, fmt.Errorf("window %q: period %q is not a whole number of milliseconds", part, per)
		}
		windows = append(windows, window{requests: n, period: d})
	}
	return windows, nil
}

func parseDuration(v string) (time.Duration, error) {
	if s, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Duration(s) * time.Second, nil
	}
	return time.ParseDuration(v)
}

// report is the outcome of a run, the content of summary.json.
type report struct {
	Start      time.Time      `json:"start"`
	Steps      []stepReport   `json:"steps"`
	Total      counts         `json:"total"`
	Windows    []windowReport `json:"windows,omitempty"`
	Violations []violation    `json:"violations,omitempty"`
	// ViolationCount counts every violation; Violations lists the first
	// maxListedViolations of them.
	ViolationCount int      `json:"violationCount"`
	Failures       []string `json:"failures,omitempty"`
	Passed         bool     `json:"passed"`
}

// counts tallies samples by outcome. Errors counts the samples answered with
// neither 200 nor 429, admitted ones included.
type counts struct {
	Samples   int64 `json:"samples"`
	Admitted  int64 `json:"admitted"`
	Refused   int64 `json:"refused"`
	Undecided int64 `json:"undecided"`
	Failed    int64 `json:"failed"`
	Errors    int64 `json:"errors"`
}

func (c *counts) add(s sample) {
	c.Samples++
	switch s.outcome {
	case admitted:
		c.Admitted++
	case refused:
		c.Refused++
	case undecided:
		c.Undecided++
	case failed:
		c.Failed++
	}
	if s.code != "200" && s.code != "429" {
		c.Errors++
	}
}

// stepReport describes the measured part of one step: the step without its
// first -warmup.
type stepReport struct {
	Step        int     `json:"step"`
	TargetRPS   float64 `json:"targetRps"`
	Seconds     float64 `json:"seconds"`
	AchievedRPS float64 `json:"achievedRps"`
	counts
	P50Millis int64 `json:"p50Ms"`
	P90Millis int64 `json:"p90Ms"`
	P95Millis int64 `json:"p95Ms"`
	P99Millis int64 `json:"p99Ms"`
	MaxMillis int64 `json:"maxMs"`
	// LagP99Millis and LagMaxMillis describe how late the generator sent
	// the requests of the step against its schedule.
	LagP99Millis int64 `json:"lagP99Ms"`
	LagMaxMillis int64 `json:"lagMaxMs"`
}

// windowReport describes how the samples filled one window. An instance is
// one fixed window of one user in one group; an admission counts toward an
// instance only where the whole interval in which it can have been decided
// lies inside that instance.
type windowReport struct {
	Window string `json:"window"`
	// Instances counts the instances with at least one such admission.
	Instances int `json:"instances"`
	// Full counts the instances whose such admissions reached the window's
	// requests.
	Full int `json:"full"`
	// MaxAdmitted is the largest number of such admissions in one instance.
	MaxAdmitted int64 `json:"maxAdmitted"`
}

// violation is a sample sequence the rate limit cannot have produced.
type violation struct {
	Kind   string    `json:"kind"`
	User   string    `json:"user"`
	Group  string    `json:"group"`
	Window string    `json:"window,omitempty"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail"`
}

const maxListedViolations = 50

// accumulator builds the report of a run one sample at a time.
type accumulator struct {
	cfg    verifyConfig
	steps  []stepAcc
	bounds [][2]int64 // the measured part of each step, in milliseconds since the epoch
	total  counts
	limits limitInput
}

type stepAcc struct {
	counts
	elapsed, lag histogram
}

func newAccumulator(cfg verifyConfig) *accumulator {
	a := &accumulator{cfg: cfg, steps: make([]stepAcc, len(cfg.steps)), bounds: make([][2]int64, len(cfg.steps))}
	offset := cfg.start
	for i, s := range cfg.steps {
		a.bounds[i] = [2]int64{offset + cfg.warmup.Milliseconds(), offset + s.duration.Milliseconds()}
		offset += s.duration.Milliseconds()
	}
	if len(cfg.windows) > 0 {
		a.limits = limitInput{}
	}
	return a
}

func (a *accumulator) add(s sample) {
	a.total.add(s)
	for i, b := range a.bounds {
		if s.start >= b[0] && s.start < b[1] {
			a.steps[i].add(s)
			a.steps[i].elapsed.add(s.elapsed)
			a.steps[i].lag.add(s.lag)
			break
		}
	}
	if a.limits != nil {
		a.limits.add(s)
	}
}

// buildReport returns the report of samples, the way verifyCommand builds it
// from its files.
func buildReport(samples []sample, cfg verifyConfig) report {
	if cfg.start == 0 {
		for _, s := range samples {
			if cfg.start == 0 || s.start < cfg.start {
				cfg.start = s.start
			}
		}
	}
	a := newAccumulator(cfg)
	for _, s := range samples {
		a.add(s)
	}
	return a.report()
}

func (a *accumulator) report() report {
	cfg := a.cfg
	var rep report
	if a.total.Samples == 0 {
		rep.Failures = append(rep.Failures, "the results hold no sample of the background or hot users")
		return rep
	}
	rep.Start = time.UnixMilli(cfg.start).UTC()
	rep.Total = a.total
	for i, s := range cfg.steps {
		acc := a.steps[i]
		seconds := float64(a.bounds[i][1]-a.bounds[i][0]) / 1000
		sr := stepReport{
			Step: i + 1, TargetRPS: s.rate, Seconds: seconds,
			AchievedRPS: float64(acc.Samples) / seconds, counts: acc.counts,
			P50Millis: acc.elapsed.quantile(0.50), P90Millis: acc.elapsed.quantile(0.90),
			P95Millis: acc.elapsed.quantile(0.95), P99Millis: acc.elapsed.quantile(0.99),
			MaxMillis:    acc.elapsed.quantile(1),
			LagP99Millis: acc.lag.quantile(0.99), LagMaxMillis: acc.lag.quantile(1),
		}
		rep.Steps = append(rep.Steps, sr)
		if cfg.minRateRatio > 0 && sr.AchievedRPS < cfg.minRateRatio*sr.TargetRPS {
			rep.Failures = append(rep.Failures, fmt.Sprintf("step %d achieved %.1f requests per second of the %.1f targeted",
				sr.Step, sr.AchievedRPS, sr.TargetRPS))
		}
		if cfg.maxLag > 0 && sr.Samples > 0 && time.Duration(sr.LagP99Millis)*time.Millisecond > cfg.maxLag {
			rep.Failures = append(rep.Failures, fmt.Sprintf("step %d fell %d ms behind its schedule at p99, above %v: "+
				"give the generator more threads or add generators", sr.Step, sr.LagP99Millis, cfg.maxLag))
		}
		if cfg.maxP99 > 0 && sr.Samples > 0 && time.Duration(sr.P99Millis)*time.Millisecond > cfg.maxP99 {
			rep.Failures = append(rep.Failures, fmt.Sprintf("step %d has a p99 of %d ms, above %v",
				sr.Step, sr.P99Millis, cfg.maxP99))
		}
	}
	if r := ratio(rep.Total.Errors, rep.Total.Samples); r > cfg.maxErrorRatio {
		rep.Failures = append(rep.Failures, fmt.Sprintf("%d of %d samples were answered with neither 200 nor 429",
			rep.Total.Errors, rep.Total.Samples))
	}
	if r := ratio(rep.Total.Undecided, rep.Total.Samples); r > cfg.maxUndecidedRatio {
		rep.Failures = append(rep.Failures, fmt.Sprintf("%d of %d samples passed the gateway without a rate limit decision",
			rep.Total.Undecided, rep.Total.Samples))
	}

	if a.limits != nil {
		rep.Windows, rep.Violations, rep.ViolationCount = checkLimits(a.limits, cfg.windows, cfg.skew)
		if rep.ViolationCount > 0 {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%d sample sequences break the limits", rep.ViolationCount))
		}
	}
	rep.Passed = len(rep.Failures) == 0
	return rep
}

func ratio(n, of int64) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}

// histogram counts values in whole milliseconds, the resolution JMeter
// records response times in: the response times or the lags of one step.
type histogram struct {
	counts []int64
	total  int64
}

func (h *histogram) add(ms int64) {
	ms = max(ms, 0)
	if int(ms) >= len(h.counts) {
		grown := make([]int64, ms+1)
		copy(grown, h.counts)
		h.counts = grown
	}
	h.counts[ms]++
	h.total++
}

// quantile returns the nearest-rank q-quantile, or 0 for an empty histogram.
func (h *histogram) quantile(q float64) int64 {
	if h.total == 0 {
		return 0
	}
	rank := max(int64(math.Ceil(q*float64(h.total))), 1)
	var seen int64
	for ms, n := range h.counts {
		seen += n
		if seen >= rank {
			return int64(ms)
		}
	}
	return int64(len(h.counts) - 1)
}

// limitKey names the counters of one user in one group.
type limitKey struct{ user, group string }

// event is what the limit check keeps of one sample: when the service can
// have decided it, and its outcome.
type event struct {
	start, end int64 // milliseconds since the epoch
	outcome    outcome
}

// limitInput holds the events of every user and group, the input of
// checkLimits.
type limitInput map[limitKey][]event

// add keeps s unless it is undecided: a response the service did not decide
// charged no counter.
func (in limitInput) add(s sample) {
	if s.outcome == undecided {
		return
	}
	k := limitKey{s.user, s.group}
	in[k] = append(in[k], event{start: s.start, end: s.end(), outcome: s.outcome})
}

// checkLimits checks the events of each user and group against fixed
// windows that start at multiples of their period since the epoch, the way
// the counter store aligns them. It returns a report per window, the first
// maxListedViolations violations, and the count of all of them.
//
// The service decides a request at some moment between the start and the end
// of its sample, read on the counter store's clock, which may differ from the
// generator's by up to skew. Each check therefore holds for every moment the
// decision can have taken, and reports only what no such moment explains:
//
//   - over-admission: an instance holds more admissions than the window's
//     requests, counting only the admissions that cannot have been decided
//     in a neighboring instance;
//   - false refusal: a 429 arrives while no window can have been full, even
//     counting every admission, and every sample without a response, that
//     can have been decided in the same instance before it.
func checkLimits(in limitInput, windows []window, skew time.Duration) ([]windowReport, []violation, int) {
	keys := make([]limitKey, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b limitKey) int {
		return cmp.Or(strings.Compare(a.user, b.user), strings.Compare(a.group, b.group))
	})

	skewMs := skew.Milliseconds()
	reports := make([]windowReport, len(windows))
	for i, w := range windows {
		reports[i].Window = w.String()
	}
	var listed []violation
	count := 0
	report := func(v violation) {
		count++
		if len(listed) < maxListedViolations {
			listed = append(listed, v)
		}
	}

	for _, k := range keys {
		var starts, ends []int64 // admissions and samples without a response
		var refusals []event
		for _, e := range in[k] {
			switch e.outcome {
			case admitted, failed:
				starts = append(starts, e.start)
				ends = append(ends, e.end)
			case refused:
				refusals = append(refusals, e)
			}
		}
		slices.Sort(starts)
		slices.Sort(ends)

		for wi, w := range windows {
			p := w.period.Milliseconds()
			certain := map[int64]int64{}
			for _, e := range in[k] {
				if e.outcome != admitted {
					continue
				}
				lo, hi := floorDiv(e.start-skewMs, p), floorDiv(e.end+skewMs, p)
				if lo == hi {
					certain[lo]++
				}
			}
			instances := make([]int64, 0, len(certain))
			for inst := range certain {
				instances = append(instances, inst)
			}
			slices.Sort(instances)
			for _, inst := range instances {
				n := certain[inst]
				reports[wi].Instances++
				reports[wi].MaxAdmitted = max(reports[wi].MaxAdmitted, n)
				if n >= w.requests {
					reports[wi].Full++
				}
				if n > w.requests {
					report(violation{
						Kind: "over-admission", User: k.user, Group: k.group, Window: w.String(),
						At:     time.UnixMilli(inst * p).UTC(),
						Detail: fmt.Sprintf("%d admissions in a window of %d requests", n, w.requests),
					})
				}
			}
		}

		for _, r := range refusals {
			lo, hi := r.start-skewMs, r.end+skewMs
			justified := false
			for _, w := range windows {
				p := w.period.Milliseconds()
				for inst := floorDiv(lo, p); inst <= floorDiv(hi, p) && !justified; inst++ {
					ws, we := inst*p, (inst+1)*p
					// An admission can precede the refusal inside this
					// instance if it can start before min(we, hi), the
					// refusal's latest decision here, and can end inside
					// the instance. A sample that ends before ws-skew also
					// starts before min(we, hi)+skew, because hi >= ws, so
					// the difference of the two counts below is the number
					// of such admissions.
					before, _ := slices.BinarySearch(starts, min(we, hi)+skewMs)
					gone, _ := slices.BinarySearch(ends, ws-skewMs)
					justified = int64(before-gone) >= w.requests
				}
				if justified {
					break
				}
			}
			if !justified {
				report(violation{
					Kind: "false refusal", User: k.user, Group: k.group,
					At:     time.UnixMilli(r.start).UTC(),
					Detail: "a 429 while every window held fewer admissions than its requests",
				})
			}
		}
	}
	return reports, listed, count
}

// floorDiv returns a divided by b rounded toward negative infinity, for b > 0.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}

func (r report) markdown() string {
	var b strings.Builder
	verdict := "PASSED"
	if !r.Passed {
		verdict = "FAILED"
	}
	fmt.Fprintf(&b, "# Load test summary: %s\n\n", verdict)
	fmt.Fprintf(&b, "Started at %s UTC.\n\n", r.Start.Format("2006-01-02 15:04:05"))
	b.WriteString("| Step | Target rps | Achieved rps | Samples | Admitted | Refused | Undecided | Errors " +
		"| p50 ms | p90 ms | p95 ms | p99 ms | max ms | lag p99 ms |\n")
	b.WriteString("| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "| %d | %.1f | %.1f | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d |\n",
			s.Step, s.TargetRPS, s.AchievedRPS, s.Samples, s.Admitted, s.Refused, s.Undecided, s.Errors,
			s.P50Millis, s.P90Millis, s.P95Millis, s.P99Millis, s.MaxMillis, s.LagP99Millis)
	}
	fmt.Fprintf(&b, "\nWhole run: %d samples, %d admitted, %d refused, %d undecided, %d without a response, %d errors.\n",
		r.Total.Samples, r.Total.Admitted, r.Total.Refused, r.Total.Undecided, r.Total.Failed, r.Total.Errors)
	if len(r.Windows) > 0 {
		b.WriteString("\n| Window | Instances | Reached the limit | Most admissions in one instance |\n")
		b.WriteString("| --- | ---: | ---: | ---: |\n")
		for _, w := range r.Windows {
			fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", w.Window, w.Instances, w.Full, w.MaxAdmitted)
		}
		fmt.Fprintf(&b, "\nLimit violations: %d.\n", r.ViolationCount)
		for _, v := range r.Violations {
			fmt.Fprintf(&b, "- %s: user %s, group %s, %s at %s: %s\n",
				v.Kind, v.User, v.Group, windowOrAny(v.Window), v.At.Format(time.RFC3339Nano), v.Detail)
		}
	}
	if len(r.Failures) > 0 {
		b.WriteString("\nFailed checks:\n\n")
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	return b.String()
}

// windowOrAny returns w, or "any window" for a violation no single window
// names.
func windowOrAny(w string) string {
	if w == "" {
		return "any window"
	}
	return w
}
