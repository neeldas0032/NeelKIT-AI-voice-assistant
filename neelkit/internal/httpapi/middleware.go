package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/auth"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/id"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/observability"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// chain applies middleware so that the first listed runs outermost. Written the
// other way round, a panic in the logging middleware would escape the recover.
func chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// statusWriter records the status code and byte count for logging and metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which the
// streaming and WebSocket paths need for deadlines and hijacking.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recoverer converts a panic into a 500 instead of killing the process.
//
// A panic in one handler must not take down a server that is in the middle of
// transcribing someone else's audio.
func recoverer() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					observability.Logger(r.Context()).Error("panic in handler",
						"panic", rec, "path", r.URL.Path)
					writeError(w, r, http.StatusInternalServerError,
						"internal", "panic", "something went wrong", nil)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestID assigns or adopts a correlation id and echoes it in the response.
func requestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get("X-Request-Id")
			// An id supplied by a client is a convenience for tracing their own
			// call, but it lands in logs, so its length and character set are
			// bounded rather than trusted.
			if len(rid) == 0 || len(rid) > 64 || !printableASCII(rid) {
				rid = id.New(id.PrefixRequest)
			}
			w.Header().Set("X-Request-Id", rid)
			next.ServeHTTP(w, r.WithContext(observability.WithRequestID(r.Context(), rid)))
		})
	}
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// logging records one line per request with its correlation id.
func logging() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)

			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			observability.Logger(r.Context()).Info("http",
				"method", r.Method, "path", r.URL.Path, "status", sw.status,
				"bytes", sw.bytes, "duration_ms", time.Since(start).Milliseconds())
		})
	}
}

// instrument records request metrics for one route.
//
// The route label is supplied at registration rather than read from
// r.Pattern. ServeMux sets Pattern on the request it passes *into* the handler,
// not on the one an outer middleware is holding, so reading it from the chain
// labelled every request "unmatched" — which looked like working metrics right
// up until the moment anyone tried to use them.
func instrument(m *observability.Metrics, route string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m == nil {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			// Labelled by pattern, never by path: a path label would create a
			// new time series per conversation id and blow the cardinality out
			// within a day of ordinary use.
			m.ObserveHTTP(route, r.Method, statusClass(sw.status), time.Since(start))
		})
	}
}

func statusClass(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// cors applies an origin allowlist.
//
// There is no wildcard mode. This API carries a bearer token and can run
// programs; "Access-Control-Allow-Origin: *" on it would let any page a user
// visits drive their machine.
func cors(allowed []string) Middleware {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		set[strings.TrimRight(strings.TrimSpace(o), "/")] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimRight(r.Header.Get("Origin"), "/")
			if origin != "" && set[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
				w.Header().Set("Access-Control-Max-Age", "600")
				// Responses differ by Origin, so a shared cache must not serve
				// one origin's response to another.
				w.Header().Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type userKey struct{}

// UserID reads the authenticated user from a request context.
func UserID(ctx context.Context) string {
	v, _ := ctx.Value(userKey{}).(string)
	return v
}

// authenticate enforces bearer authentication.
func authenticate(store *auth.Store, audit func(ctx context.Context, action, outcome, detail string)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(header, "Bearer ")
			if !ok {
				token, ok = strings.CutPrefix(header, "bearer ")
			}
			if !ok || strings.TrimSpace(token) == "" {
				if audit != nil {
					audit(r.Context(), "auth.fail", "denied", "missing bearer token")
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="neelkit"`)
				writeError(w, r, http.StatusUnauthorized, "authentication", "missing_token",
					"an Authorization: Bearer token is required", nil)
				return
			}

			uid, err := store.Authenticate(r.Context(), strings.TrimSpace(token))
			if err != nil {
				if audit != nil {
					audit(r.Context(), "auth.fail", "denied", clientIP(r))
				}
				writeError(w, r, http.StatusUnauthorized, "authentication", "invalid_token",
					"that API key is not valid", nil)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, uid)))
		})
	}
}

// timeout bounds a request. WebSocket upgrades are exempt: a long-lived stream
// is the point, and a deadline would cut it off mid-sentence.
func timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isWebSocketUpgrade(r) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// bodyLimit caps request bodies so a single upload cannot exhaust memory.
func bodyLimit(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && !isWebSocketUpgrade(r) {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rateLimiter is a per-key token bucket.
//
// Written by hand rather than pulled from golang.org/x/time: it is twenty lines,
// and the project's dependency list is short enough to read in full, which is
// itself a security property.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute, burst int) *rateLimiter {
	return &rateLimiter{
		buckets: map[string]*bucket{},
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
	}
}

// allow reports whether a request may proceed, and how long to wait if not.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
		return false, wait
	}
	b.tokens--
	return true, 0
}

// rateLimit applies a bucket keyed by authenticated user, falling back to the
// client address for unauthenticated routes.
func rateLimit(l *rateLimiter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := UserID(r.Context())
			if key == "" {
				key = clientIP(r)
			}
			ok, wait := l.allow(key)
			if !ok {
				w.Header().Set("Retry-After", strconvSeconds(wait))
				writeError(w, r, http.StatusTooManyRequests, "rate_limited", "too_many_requests",
					"you are sending requests faster than the configured limit", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func strconvSeconds(d time.Duration) string {
	s := int(d.Seconds() + 0.5)
	if s < 1 {
		s = 1
	}
	return itoa(s)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// clientIP extracts the peer address. X-Forwarded-For is deliberately ignored:
// it is trivially spoofed, and trusting it would let one client evade rate
// limiting by inventing a header.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// errBodyTooLarge identifies the sentinel http.MaxBytesReader returns, so the
// handler can answer 413 instead of a generic 400.
var errBodyTooLarge = errors.New("http: request body too large")
