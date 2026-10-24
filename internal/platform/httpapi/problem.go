package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Problem is an RFC 9457 problem details body (decision 32). Code is the stable,
// machine-readable identifier clients branch on; Type is derived from it.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Code      string `json:"code"`
	RequestID string `json:"request_id,omitempty"`
}

func NewProblem(status int, code, detail string) *Problem {
	return &Problem{
		Type:   "/problems/" + code,
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Code:   code,
	}
}

func WriteProblem(w http.ResponseWriter, r *http.Request, p *Problem) {
	p.Instance = r.URL.Path
	p.RequestID = RequestID(r.Context())
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteInternalError logs err with the request ID and answers 500 without detail, so
// internal messages never reach the client.
func WriteInternalError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	logger.ErrorContext(r.Context(), "internal error",
		"error", err, "request_id", RequestID(r.Context()), "method", r.Method, "path", r.URL.Path)
	WriteProblem(w, r, NewProblem(http.StatusInternalServerError, "internal_error", ""))
}
