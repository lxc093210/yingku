package api

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// ---- request logging ----
//
// One line per request on the standard logger (stderr): method, path,
// status and duration. Companion software (Nan) talks to this server
// unattended, so the log is the only way to see which protocol paths it
// actually hits and which ones 404. The api_key query value is redacted
// — keys must never land in logs; token headers are not logged at all.

// statusRecorder captures the response status for the log line while
// passing everything else through untouched.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (rec *statusRecorder) WriteHeader(code int) {
	if !rec.wrote {
		rec.status = code
		rec.wrote = true
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	return rec.ResponseWriter.Write(b)
}

// Flush passes through so streamed responses keep working.
func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		if !rec.wrote {
			rec.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// redactedURI returns path + query with any api_key value masked. The
// raw query is edited in place (not decoded/re-encoded) so every other
// parameter survives byte-for-byte and the mask stays a literal ***.
func redactedURI(r *http.Request) string {
	raw := r.URL.RawQuery
	if raw == "" {
		return r.URL.Path
	}
	parts := strings.Split(raw, "&")
	masked := false
	for i, p := range parts {
		k, _, _ := strings.Cut(p, "=")
		if strings.EqualFold(k, "api_key") {
			parts[i] = k + "=***"
			masked = true
		}
	}
	if !masked {
		return r.URL.RequestURI()
	}
	return r.URL.Path + "?" + strings.Join(parts, "&")
}

// logMiddleware wraps the whole handler chain (auth + mux, static UI
// included) so rejected and unrouted requests are visible too.
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)", r.Method, redactedURI(r), rec.status,
			time.Since(start).Round(time.Millisecond))
	})
}
