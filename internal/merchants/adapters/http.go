package adapters

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

type Authenticator interface {
	Authenticate(ctx context.Context, plaintext string) (domain.Principal, error)
}

type principalKey struct{}

// PrincipalFrom returns the merchant the request was authenticated as. ok is false
// only on a route that RequireAPIKey does not wrap.
func PrincipalFrom(ctx context.Context) (p domain.Principal, ok bool) {
	p, ok = ctx.Value(principalKey{}).(domain.Principal)
	return p, ok
}

// RequireAPIKey accepts "Authorization: Bearer sk_..." and answers 401 otherwise.
func RequireAPIKey(auth Authenticator, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				unauthorized(w, r, "send the API key as: Authorization: Bearer sk_...")
				return
			}
			principal, err := auth.Authenticate(r.Context(), key)
			if errors.Is(err, app.ErrUnauthorized) {
				unauthorized(w, r, "the API key is invalid or revoked")
				return
			}
			if err != nil {
				httpapi.WriteInternalError(w, r, logger, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
		})
	}
}

func unauthorized(w http.ResponseWriter, r *http.Request, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="cross-border-pay"`)
	httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnauthorized, "unauthorized", detail))
}
