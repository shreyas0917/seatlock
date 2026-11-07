package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The internal server serves /metrics AND the pprof endpoints (the public
// router must serve neither: see TestMetrics_NotOnThePublicRouter and TestPprof_NotOnThePublicRouter in httpapi).
func TestNewServer_ServesMetricsAndPprof(t *testing.T) {
	m := NewAPI()
	m.Holds.WithLabelValues("created").Inc()
	handler := NewServer(":0", m).Handler

	for _, path := range []string{"/metrics", "/debug/pprof/", "/debug/pprof/goroutine?debug=1", "/debug/pprof/heap", "/debug/pprof/cmdline"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, rec.Code, path)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), `seatlock_holds_total{outcome="created"} 1`))
}
