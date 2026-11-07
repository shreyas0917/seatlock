package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// A timeline column: a name and a PromQL query (instant value per step).
type column struct {
	name  string
	query string
	scale float64 // multiply the value (e.g. seconds -> ms)
}

// The columns of a chaos timeline: what users saw, every step.
var timelineColumns = []column{
	{"rps", `sum(rate(seatlock_http_requests_total[15s]))`, 1},
	{"5xx/s", `sum(rate(seatlock_http_requests_total{status=~"5.."}[15s])) or vector(0)`, 1},
	{"500/s", `sum(rate(seatlock_http_requests_total{status="500"}[15s])) or vector(0)`, 1},
	{"lock_unavail/s", `sum(rate(seatlock_holds_total{outcome="lock_unavailable"}[15s])) or vector(0)`, 1},
	{"busy/s", `sum(rate(seatlock_http_requests_total{status="503"}[15s])) - (sum(rate(seatlock_holds_total{outcome="lock_unavailable"}[15s])) or vector(0)) or vector(0)`, 1},
	{"p95 hold ms", `histogram_quantile(0.95, sum by (le) (rate(seatlock_http_request_duration_seconds_bucket{route="/v1/holds"}[15s])))`, 1000},
	{"p95 confirm ms", `histogram_quantile(0.95, sum by (le) (rate(seatlock_http_request_duration_seconds_bucket{route="/v1/bookings"}[15s])))`, 1000},
	{"pending pay", `max(seatlock_payments_pending_oldest_age_seconds) > 0 or vector(0)`, 1},
	{"outbox pending", `sum(seatlock_outbox_rows{status="PENDING"}) or vector(0)`, 1},
}

// queryRange returns timestamp -> value for one PromQL query.
func queryRange(client *http.Client, base, query string, from, to time.Time, step time.Duration) (map[int64]float64, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(from.Unix(), 10))
	q.Set("end", strconv.FormatInt(to.Unix(), 10))
	q.Set("step", strconv.Itoa(int(step.Seconds())))
	resp, err := client.Get(base + "/api/v1/query_range?" + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Values [][2]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", body.Error)
	}
	out := map[int64]float64{}
	for _, series := range body.Data.Result {
		for _, v := range series.Values {
			ts, _ := v[0].(float64)
			s, _ := v[1].(string)
			f, err := strconv.ParseFloat(s, 64)
			if err != nil || math.IsNaN(f) {
				continue
			}
			out[int64(ts)] += f
		}
	}
	return out, nil
}

// Timeline prints one row per step between from and to, with offsets from
// `zero` (the start of the load) and the events (offset -> label).
func Timeline(w io.Writer, prom string, zero, from, to time.Time, step time.Duration, events map[int64]string) error {
	client := &http.Client{Timeout: 20 * time.Second}
	data := make([]map[int64]float64, len(timelineColumns))
	for i, c := range timelineColumns {
		m, err := queryRange(client, prom, c.query, from, to, step)
		if err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		data[i] = m
	}
	fmt.Fprintf(w, "%7s", "t")
	for _, c := range timelineColumns {
		fmt.Fprintf(w, " %14s", c.name)
	}
	fmt.Fprintln(w, "  event")
	var evs []int64
	for k := range events {
		evs = append(evs, k)
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i] < evs[j] })
	for ts := from.Unix(); ts <= to.Unix(); ts += int64(step.Seconds()) {
		fmt.Fprintf(w, "%6ds", ts-zero.Unix())
		for i, c := range timelineColumns {
			if v, ok := data[i][ts]; ok {
				fmt.Fprintf(w, " %14.1f", v*c.scale)
			} else {
				fmt.Fprintf(w, " %14s", "-")
			}
		}
		// events that happened during this step
		label := ""
		for _, e := range evs {
			if e >= ts && e < ts+int64(step.Seconds()) {
				label += " <- " + events[e]
			}
		}
		fmt.Fprintln(w, label)
	}
	return nil
}
