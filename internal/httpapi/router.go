// router.go wires URLs to handlers and adds middleware.
//
// Where it fits: cmd/api/main.go calls NewRouter and gives the result to the
// http.Server. Every request passes through the middleware below, in order,
// before it reaches a handler in handlers.go.
package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	httpSwagger "github.com/swaggo/http-swagger/v2"
)

// NewRouter builds the HTTP handler for the whole API.
func NewRouter(svc BookingService, db Pinger, logger *slog.Logger) http.Handler {
	h := &handlers{svc: svc, db: db, logger: logger}

	r := chi.NewRouter()

	// Middleware wraps every request. Order matters: RequestID runs first so
	// the logger and the error handler can include the ID.
	r.Use(middleware.RequestID)  // gives each request a unique ID
	r.Use(requestLogger(logger)) // one JSON log line per request
	r.Use(middleware.Recoverer)  // a panic in a handler becomes a 500, not a crashed server

	r.Get("/healthz", h.healthz)
	r.Get("/readyz", h.readyz)

	r.Route("/v1", func(r chi.Router) {
		r.Get("/shows", h.listShows)
		r.Get("/shows/{showID}/seats", h.listSeats)
		r.Post("/bookings", h.createBooking)
		r.Get("/bookings/{bookingID}", h.getBooking)
	})

	// Swagger UI. The spec itself is registered by importing the generated
	// docs package in cmd/api/main.go.
	r.Get("/swagger/*", httpSwagger.Handler(httpSwagger.URL("/swagger/doc.json")))

	return r
}

// requestLogger logs method, path, status, size, duration and request ID for
// every request, and returns the request ID to the client in the X-Request-ID
// header so a user can quote it when reporting a problem.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			requestID := middleware.GetReqID(r.Context())
			w.Header().Set("X-Request-ID", requestID)

			// The wrapper remembers the status code and bytes written,
			// which plain http.ResponseWriter does not expose.
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			logger.Info("http request",
				"request_id", requestID,
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}
