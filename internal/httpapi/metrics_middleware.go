// metrics_middleware.go records RED metrics (Rate, Errors, Duration) for
// every HTTP request: seatlock_http_requests_total, _duration_seconds and
// _in_flight_requests (definitions in internal/metrics).
//
// Where it fits: router.go installs it right after RequestID, so it wraps
// the whole request, including routing.
//
// CARDINALITY: the "route" label is the chi route PATTERN, read AFTER chi
// routed the request: DELETE /v1/holds/123 is recorded as
// route="/v1/holds/{holdID}". Using the raw path would create one time series
// per hold id. Requests that match no route are recorded as "unmatched" (so
// a scanner trying random URLs cannot create series either), and methods
// outside the standard set become "OTHER".
package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/shreyas0917/seatlock/internal/metrics"
)

// RouterOption configures optional router features.
type RouterOption func(*routerOptions)

type routerOptions struct {
	metrics *metrics.Metrics
}

// WithMetrics enables HTTP metrics recorded into m.
func WithMetrics(m *metrics.Metrics) RouterOption {
	return func(o *routerOptions) { o.metrics = m }
}

// knownMethods bounds the "method" label (clients can send any string).
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true,
	http.MethodDelete: true, http.MethodHead: true, http.MethodOptions: true,
}

// requestMetrics returns the middleware.
func requestMetrics(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			m.HTTPInFlight.Inc()
			defer m.HTTPInFlight.Dec()

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			route := "unmatched"
			if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
				route = rctx.RoutePattern()
			}
			method := r.Method
			if !knownMethods[method] {
				method = "OTHER"
			}
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK // the handler wrote nothing: net/http sends 200
			}
			m.HTTPRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
			m.HTTPDuration.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
		})
	}
}
