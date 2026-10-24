package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type requestIDKey struct{}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Handler wraps the routes with, outermost first: request ID, access log, panic
// recovery, and problem responses for unmatched routes.
func Handler(mux *http.ServeMux, logger *slog.Logger) http.Handler {
	return withRequestID(withAccessLog(logger, withRecover(logger, withProblems(mux))))
}

// withRequestID always generates the ID instead of trusting a client header, so the
// ID in our logs is ours.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := rand.Text()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b) //nolint:wrapcheck // a transparent wrapper returns the inner error as is.
}

// Unwrap lets http.ResponseController reach the original writer (Flush, deadlines).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func withAccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		logger.InfoContext(r.Context(), "request",
			"request_id", RequestID(r.Context()), "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

func withRecover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// net/http uses this panic to abort a response on purpose.
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			WriteInternalError(w, r, logger, fmt.Errorf("panic: %v", v))
		}()
		next.ServeHTTP(w, r)
	})
}

// discard captures what ServeMux would answer for an unmatched route, so it can be
// answered as a problem instead of plain text.
type discard struct {
	header http.Header
	status int
}

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(code int)        { d.status = code }

func withProblems(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}

		d := &discard{header: http.Header{}}
		h.ServeHTTP(d, r)
		switch d.status {
		case http.StatusMethodNotAllowed:
			w.Header().Set("Allow", d.header.Get("Allow"))
			WriteProblem(w, r, NewProblem(d.status, "method_not_allowed", r.Method+" is not allowed on "+r.URL.Path))
		case http.StatusNotFound:
			WriteProblem(w, r, NewProblem(d.status, "not_found", "no route for "+r.URL.Path))
		default:
			mux.ServeHTTP(w, r)
		}
	})
}
