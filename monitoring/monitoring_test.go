// Package monitoring holds no Go code: only Prometheus, alert and Grafana
// configuration. This test validates the parts promtool cannot: the Grafana
// dashboard JSON parses, and every panel and every query uses the
// PROVISIONED Prometheus datasource (uid "prometheus"). A panel pointing at
// a datasource that does not exist would silently show "No data" in the
// screenshot.
package monitoring

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type datasourceRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type panel struct {
	ID         int            `json:"id"`
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Datasource *datasourceRef `json:"datasource"`
	Targets    []struct {
		Datasource *datasourceRef `json:"datasource"`
		Expr       string         `json:"expr"`
	} `json:"targets"`
}

func TestDashboardUsesTheProvisionedDatasource(t *testing.T) {
	provisioned, err := os.ReadFile("grafana/provisioning/datasources/prometheus.yml")
	require.NoError(t, err)
	require.Contains(t, string(provisioned), "uid: prometheus", "the datasource uid the dashboard relies on")

	raw, err := os.ReadFile("grafana/dashboards/seatlock-reliability.json")
	require.NoError(t, err)
	var dash struct {
		UID    string  `json:"uid"`
		Title  string  `json:"title"`
		Panels []panel `json:"panels"`
	}
	require.NoError(t, json.Unmarshal(raw, &dash), "the dashboard JSON must parse")
	assert.Equal(t, "seatlock-reliability", dash.UID)

	ids := map[int]bool{}
	queries := 0
	for _, p := range dash.Panels {
		assert.False(t, ids[p.ID], "duplicate panel id %d", p.ID)
		ids[p.ID] = true
		if p.Type == "row" {
			continue
		}
		require.NotNil(t, p.Datasource, "panel %q has no datasource", p.Title)
		assert.Equal(t, "prometheus", p.Datasource.UID, "panel %q", p.Title)
		require.NotEmpty(t, p.Targets, "panel %q has no query", p.Title)
		for _, target := range p.Targets {
			require.NotNil(t, target.Datasource, "a query of panel %q has no datasource", p.Title)
			assert.Equal(t, "prometheus", target.Datasource.UID, "panel %q", p.Title)
			assert.NotEmpty(t, strings.TrimSpace(target.Expr), "panel %q has an empty query", p.Title)
			// Our metrics never carry a label called job for worker jobs.
			assert.NotContains(t, target.Expr, "exported_job", "panel %q", p.Title)
			queries++
		}
	}
	assert.GreaterOrEqual(t, queries, 25)
}

// Every invariant gauge must be wired to DoubleBookingDetected, which
// aggregates over ALL checks (no check name filter).
func TestDoubleBookingAlertCoversEveryInvariant(t *testing.T) {
	rules, err := os.ReadFile("alerts.yml")
	require.NoError(t, err)
	assert.Contains(t, string(rules), "expr: max by (check) (seatlock_invariant_violations) > 0")
}
