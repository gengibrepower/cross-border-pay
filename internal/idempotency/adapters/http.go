package adapters

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gengibrepower/cross-border-pay/internal/idempotency/app"
	"github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

const maxBodyBytes = 1 << 20

// storedHeaders are the headers a replay repeats; X-Request-Id is always the current one.
var storedHeaders = []string{"Content-Type", "Location"}

type Keys interface {
	Begin(ctx context.Context, req *domain.Request) (app.Acquired, error)
	Complete(ctx context.Context, held *domain.Held, resp *domain.Response) error
	Fail(ctx context.Context, held *domain.Held) (released bool, err error)
}

// MerchantFunc returns the authenticated merchant; cmd/api wires it to the merchants
// module so that idempotency does not import it.
type MerchantFunc func(ctx context.Context) (domain.MerchantID, bool)

type heldKey struct{}

// HeldFrom returns the key the request runs under. ok is false only on a route that
// Require does not wrap.
func HeldFrom(ctx context.Context) (held *domain.Held, ok bool) {
	held, ok = ctx.Value(heldKey{}).(*domain.Held)
	return held, ok
}

// Require makes a route idempotent by its Idempotency-Key header (section 4.2). It goes
// inside the authentication, since keys belong to a merchant.
func Require(keys Keys, merchant MerchantFunc, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m, ok := merchant(r.Context())
			if !ok {
				httpapi.WriteInternalError(w, r, logger, errors.New("idempotency: route is not behind RequireAPIKey"))
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					writeProblem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "the body is larger than 1 MiB")
					return
				}
				writeProblem(w, r, http.StatusBadRequest, "invalid_request", "the body could not be read")
				return
			}
			req, err := domain.NewRequest(m, r.Header.Get("Idempotency-Key"), domain.HashRequest(r.Method, r.URL.Path, body))
			if err != nil {
				writeProblem(w, r, http.StatusBadRequest, "idempotency_key_required", "send an Idempotency-Key header of 1 to 255 characters")
				return
			}

			acquired, err := keys.Begin(r.Context(), &req)
			switch {
			case errors.Is(err, app.ErrInProgress):
				writeInProgress(w, r)
				return
			case errors.Is(err, app.ErrKeyReused):
				writeProblem(w, r, http.StatusUnprocessableEntity, "idempotency_key_reused", "this Idempotency-Key was used with a different request")
				return
			case err != nil:
				httpapi.WriteInternalError(w, r, logger, err)
				return
			case acquired.Replay != nil:
				replay(w, acquired.Replay)
				return
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			buf := &bufferedResponse{header: http.Header{}}
			next.ServeHTTP(buf, r.WithContext(context.WithValue(r.Context(), heldKey{}, acquired.Held)))

			// The work is done: a client that gave up must not keep it from being recorded.
			ctx := context.WithoutCancel(r.Context())
			if buf.status >= http.StatusInternalServerError {
				if _, err := keys.Fail(ctx, acquired.Held); err != nil {
					logger.ErrorContext(ctx, "idempotency key not failed", "error", err, "request_id", httpapi.RequestID(ctx))
				}
				buf.flush(w)
				return
			}
			// Stored before it is sent: once the client sees the answer, a retry replays it.
			err = keys.Complete(ctx, acquired.Held, buf.response())
			switch {
			case errors.Is(err, app.ErrLeaseLost):
				// A retry resumed the key while this request ran; that retry answers.
				writeInProgress(w, r)
			case err != nil:
				httpapi.WriteInternalError(w, r, logger, err)
			default:
				buf.flush(w)
			}
		})
	}
}

func replay(w http.ResponseWriter, resp *domain.Response) {
	for _, h := range storedHeaders {
		if v, ok := resp.Headers[h]; ok {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Idempotent-Replayed", "true")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// bufferedResponse holds the handler's answer until the key has recorded it.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails; the signature is io.Writer's.
}

func (b *bufferedResponse) response() *domain.Response {
	headers := map[string]string{}
	for _, h := range storedHeaders {
		if v := b.header.Get(h); v != "" {
			headers[h] = v
		}
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return &domain.Response{Status: b.status, Headers: headers, Body: b.body.Bytes()}
}

func (b *bufferedResponse) flush(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}

func writeInProgress(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusConflict, "idempotency_key_in_progress", "a request with this Idempotency-Key is still in progress; retry later")
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	httpapi.WriteProblem(w, r, httpapi.NewProblem(status, code, detail))
}
