package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A trimmed k6 --summary-export (k6 0.54 format).
const sample = `{"root_group":{},"metrics":{
 "responses{endpoint:hold,status:201}":{"count":100,"rate":9},
 "responses{endpoint:hold,status:409}":{"count":4890,"rate":400},
 "responses{endpoint:hold,status:503}":{"count":10,"rate":1},
 "responses{endpoint:confirm,status:201}":{"count":100,"rate":9},
 "responses{endpoint:seats,status:500}":{"count":0,"rate":0},
 "http_req_duration{endpoint:hold}":{"med":12.5,"p(95)":80,"p(99)":140,"count":5000},
 "http_req_duration{endpoint:confirm}":{"med":110,"p(95)":160,"p(99)":190,"count":100},
 "hold_to_booked":{"med":130,"p(95)":200,"p(99)":240,"count":100},
 "bookings_ok":{"count":100,"rate":9},
 "unexpected_status":{"count":10,"rate":1}
}}`

func TestSummarize(t *testing.T) {
	r, err := Summarize(strings.NewReader(sample), 10*time.Second)
	require.NoError(t, err)

	assert.Equal(t, 5100, r.Responses)
	assert.InDelta(t, 510.0, r.RPS, 0.001)
	require.Contains(t, r.Endpoints, "hold")
	assert.Equal(t, 5000, r.Endpoints["hold"].Count)
	assert.Equal(t, map[string]int{"201": 100, "409": 4890, "503": 10}, r.Endpoints["hold"].Statuses)
	assert.Equal(t, 80.0, r.Endpoints["hold"].P95)
	assert.NotContains(t, r.Endpoints, "seats", "an endpoint with no responses is left out")
	assert.Equal(t, 100, r.Counters["bookings_ok"])
	assert.Equal(t, 0, r.Counters["shed_503"], "a metric missing from the export is 0")
	assert.InDelta(t, 100*10/5100.0, r.Unexpected, 1e-9)
	assert.Equal(t, 240.0, r.HoldToBook["p99"])

	var buf bytes.Buffer
	Print(&buf, r)
	assert.Contains(t, buf.String(), "201:100 409:4890 503:10")
}

func TestOf_MedianMinMax(t *testing.T) {
	assert.Equal(t, Stat{Median: 20, Min: 10, Max: 90}, Of([]float64{90, 10, 20}))
	assert.Equal(t, Stat{Median: 15, Min: 10, Max: 20}, Of([]float64{20, 10}))
	assert.Equal(t, Stat{}, Of(nil))
}
