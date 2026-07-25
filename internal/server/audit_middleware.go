package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/audit"
	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/openai"
)

const maxAuditErrorBody = 64 << 10

type auditResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	errorBody   []byte
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status, w.wroteHeader = status, true
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(payload []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.status >= 400 && len(w.errorBody) < maxAuditErrorBody {
		remaining := maxAuditErrorBody - len(w.errorBody)
		w.errorBody = append(w.errorBody, payload[:min(len(payload), remaining)]...)
	}
	return w.ResponseWriter.Write(payload)
}

func (w *auditResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *auditResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *auditResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *auditResponseWriter) errorCode() string {
	var payload map[string]any
	if json.Unmarshal(w.errorBody, &payload) == nil {
		if code := openai.String(payload, "code", ""); code != "" {
			return code
		}
		if detail, ok := payload["error"].(map[string]any); ok {
			if code := openai.String(detail, "code", ""); code != "" {
				return code
			}
			if kind := openai.String(detail, "type", ""); kind != "" {
				return kind
			}
		}
	}
	return strconv.Itoa(w.statusCode())
}

func (s *Server) auditRequests(next http.Handler) http.Handler {
	if s == nil || s.audits == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol := ""
		if r.Method == http.MethodPost {
			switch r.URL.Path {
			case "/v1/chat/completions":
				protocol = "chat"
			case "/v1/responses":
				protocol = "responses"
			case "/v1/messages":
				protocol = "messages"
			}
		}
		if protocol == "" {
			next.ServeHTTP(w, r)
			return
		}

		started := time.Now()
		capture := audit.NewCapture(protocol, grok.NewID(), started)
		recorder := &auditResponseWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r.WithContext(audit.WithCapture(r.Context(), capture)))
		if recorder.statusCode() >= 400 {
			capture.SetError(recorder.errorCode())
		}
		s.audits.Enqueue(capture.Finish(recorder.statusCode(), time.Now()))
	})
}

func captureAuditRequest(ctx context.Context, body map[string]any) {
	streaming, _ := body["stream"].(bool)
	audit.CaptureFromContext(ctx).SetRequest(openai.String(body, "model", ""), streaming)
}
