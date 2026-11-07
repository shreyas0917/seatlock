// Command report turns k6 summary exports (--summary-export) into the
// numbers RESULTS.md reports. It is plain Go so the load tests need no extra
// tools (no jq, no Python).
//
//	go run ./loadtest/report -duration 10s run.json          one run: table + run.result.json
//	go run ./loadtest/report -median a.result.json b.result.json c.result.json
//	                                                          median (min-max) of several runs
//
// Durations in the k6 export are milliseconds. RPS is responses per second
// over the scenario's ACTIVE duration (-duration), not the whole k6 process
// (which also includes setup), so it is not diluted.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Endpoints and statuses, in report order (must match loadtest/lib.js).
var (
	endpoints = []string{"seats", "hold", "confirm", "cancel"}
	statuses  = []string{"200", "201", "400", "402", "404", "409", "410", "422", "500", "502", "503", "504", "0"}
	counters  = []string{"holds_created", "seat_conflicts", "bookings_ok", "confirm_402", "confirm_410", "confirm_504",
		"lock_unavailable_503", "shed_503", "unexpected_status", "status_500", "confirm_retries", "dropped_iterations", "iterations"}
)

// Endpoint is one endpoint's numbers in one run.
type Endpoint struct {
	Count    int            `json:"count"`
	RPS      float64        `json:"rps"`
	P50      float64        `json:"p50_ms"`
	P95      float64        `json:"p95_ms"`
	P99      float64        `json:"p99_ms"`
	Statuses map[string]int `json:"statuses"`
}

// Result is the compact summary of one run (written as <run>.result.json).
type Result struct {
	DurationS   float64              `json:"duration_s"`
	Responses   int                  `json:"responses"`
	RPS         float64              `json:"rps"`
	Endpoints   map[string]*Endpoint `json:"endpoints"`
	Counters    map[string]int       `json:"counters"`
	HoldToBook  map[string]float64   `json:"hold_to_booked_ms,omitempty"` // p50/p95/p99
	Unexpected  float64              `json:"unexpected_pct"`
	Shed        float64              `json:"shed_pct"`
	ShedPlusUnx float64              `json:"shed_plus_unexpected_pct"`
}

type metric map[string]any

func (m metric) num(key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

// Summarize builds a Result from a k6 summary export.
func Summarize(r io.Reader, duration time.Duration) (Result, error) {
	var export struct {
		Metrics map[string]metric `json:"metrics"`
	}
	if err := json.NewDecoder(r).Decode(&export); err != nil {
		return Result{}, fmt.Errorf("decode k6 summary: %w", err)
	}
	get := func(name string) metric {
		if m, ok := export.Metrics[name]; ok {
			return m
		}
		return metric{}
	}
	res := Result{DurationS: duration.Seconds(), Endpoints: map[string]*Endpoint{}, Counters: map[string]int{}}
	for _, e := range endpoints {
		ep := &Endpoint{Statuses: map[string]int{}}
		for _, s := range statuses {
			if n := int(get(fmt.Sprintf("responses{endpoint:%s,status:%s}", e, s)).num("count")); n > 0 {
				ep.Statuses[s] = n
				ep.Count += n
			}
		}
		if ep.Count == 0 {
			continue
		}
		d := get(fmt.Sprintf("http_req_duration{endpoint:%s}", e))
		ep.P50, ep.P95, ep.P99 = d.num("med"), d.num("p(95)"), d.num("p(99)")
		ep.RPS = float64(ep.Count) / duration.Seconds()
		res.Endpoints[e] = ep
		res.Responses += ep.Count
	}
	res.RPS = float64(res.Responses) / duration.Seconds()
	for _, c := range counters {
		res.Counters[c] = int(get(c).num("count"))
	}
	if h := get("hold_to_booked"); h.num("count") > 0 {
		res.HoldToBook = map[string]float64{"p50": h.num("med"), "p95": h.num("p(95)"), "p99": h.num("p(99)")}
	}
	if res.Responses > 0 {
		res.Unexpected = 100 * float64(res.Counters["unexpected_status"]) / float64(res.Responses)
		res.Shed = 100 * float64(res.Counters["shed_503"]) / float64(res.Responses)
		res.ShedPlusUnx = res.Unexpected + res.Shed
	}
	return res, nil
}

// Print writes the human-readable table of one run.
func Print(w io.Writer, r Result) {
	fmt.Fprintf(w, "duration %.0fs  responses %d  RPS %.1f  unexpected %.3f%%  shed %.3f%%\n",
		r.DurationS, r.Responses, r.RPS, r.Unexpected, r.Shed)
	fmt.Fprintf(w, "%-8s %7s %8s %8s %8s %8s  statuses\n", "endpoint", "count", "rps", "p50 ms", "p95 ms", "p99 ms")
	for _, e := range endpoints {
		ep, ok := r.Endpoints[e]
		if !ok {
			continue
		}
		var parts []string
		for _, s := range statuses {
			if n := ep.Statuses[s]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s:%d", s, n))
			}
		}
		fmt.Fprintf(w, "%-8s %7d %8.1f %8.1f %8.1f %8.1f  %s\n", e, ep.Count, ep.RPS, ep.P50, ep.P95, ep.P99, strings.Join(parts, " "))
	}
	var parts []string
	for _, c := range counters {
		parts = append(parts, fmt.Sprintf("%s=%d", c, r.Counters[c]))
	}
	fmt.Fprintln(w, strings.Join(parts, " "))
	if r.HoldToBook != nil {
		fmt.Fprintf(w, "hold->booked ms: p50 %.1f  p95 %.1f  p99 %.1f\n", r.HoldToBook["p50"], r.HoldToBook["p95"], r.HoldToBook["p99"])
	}
}

// Stat is the median and range of one number across runs.
type Stat struct{ Median, Min, Max float64 }

func (s Stat) String() string { return fmt.Sprintf("%.1f (%.1f-%.1f)", s.Median, s.Min, s.Max) }

// Of returns the median, min and max of values (the middle one for 3 runs).
func Of(values []float64) Stat {
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	n := len(v)
	if n == 0 {
		return Stat{}
	}
	med := v[n/2]
	if n%2 == 0 {
		med = (v[n/2-1] + v[n/2]) / 2
	}
	return Stat{Median: med, Min: v[0], Max: v[n-1]}
}

// PrintMedian writes the median (min-max) of the key numbers across runs.
func PrintMedian(w io.Writer, runs []Result) {
	pick := func(f func(Result) float64) Stat {
		var v []float64
		for _, r := range runs {
			v = append(v, f(r))
		}
		return Of(v)
	}
	fmt.Fprintf(w, "runs %d — median (min-max)\n", len(runs))
	fmt.Fprintf(w, "RPS %s  unexpected%% %s  shed%% %s\n",
		pick(func(r Result) float64 { return r.RPS }),
		pick(func(r Result) float64 { return r.Unexpected }),
		pick(func(r Result) float64 { return r.Shed }))
	for _, e := range endpoints {
		has := false
		for _, r := range runs {
			if _, ok := r.Endpoints[e]; ok {
				has = true
			}
		}
		if !has {
			continue
		}
		ep := func(f func(*Endpoint) float64) Stat {
			return pick(func(r Result) float64 {
				if x, ok := r.Endpoints[e]; ok {
					return f(x)
				}
				return 0
			})
		}
		fmt.Fprintf(w, "%-8s rps %s  p50 %s  p95 %s  p99 %s\n", e,
			ep(func(x *Endpoint) float64 { return x.RPS }), ep(func(x *Endpoint) float64 { return x.P50 }),
			ep(func(x *Endpoint) float64 { return x.P95 }), ep(func(x *Endpoint) float64 { return x.P99 }))
	}
	for _, c := range counters {
		fmt.Fprintf(w, "%s %s  ", c, pick(func(r Result) float64 { return float64(r.Counters[c]) }))
	}
	fmt.Fprintln(w)
	if runs[0].HoldToBook != nil {
		fmt.Fprintf(w, "hold->booked ms: p50 %s  p95 %s  p99 %s\n",
			pick(func(r Result) float64 { return r.HoldToBook["p50"] }),
			pick(func(r Result) float64 { return r.HoldToBook["p95"] }),
			pick(func(r Result) float64 { return r.HoldToBook["p99"] }))
	}
}

func main() {
	duration := flag.Duration("duration", 0, "active duration of the scenario (for RPS), e.g. 10s")
	median := flag.Bool("median", false, "aggregate several .result.json files")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: report -duration 10s summary.json | report -median a.result.json b.result.json ...")
		os.Exit(2)
	}
	if *median {
		var runs []Result
		for _, path := range flag.Args() {
			b, err := os.ReadFile(path)
			if err != nil {
				fail(err)
			}
			var r Result
			if err := json.Unmarshal(b, &r); err != nil {
				fail(fmt.Errorf("%s: %w", path, err))
			}
			runs = append(runs, r)
		}
		PrintMedian(os.Stdout, runs)
		return
	}
	if *duration <= 0 {
		fail(fmt.Errorf("-duration is required"))
	}
	path := flag.Arg(0)
	f, err := os.Open(path)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	res, err := Summarize(f, *duration)
	if err != nil {
		fail(err)
	}
	Print(os.Stdout, res)
	out, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(strings.TrimSuffix(path, ".json")+".result.json", append(out, '\n'), 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "report:", err)
	os.Exit(1)
}
