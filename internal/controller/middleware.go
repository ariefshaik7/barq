package controller

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"barq/internal/apierr"
	"barq/internal/auth"
)

// Middleware is a handler wrapper.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware in order. The first middleware is the
// outermost.
func Chain(handler http.Handler, middleware ...Middleware) http.Handler {
	for index := len(middleware) - 1; index >= 0; index-- {
		handler = middleware[index](handler)
	}
	return handler
}

// requestIDMiddleware attaches a request identifier to every request.
//
// The identifier is echoed in the response header so that a user can
// quote it, and it is placed in the context so that every log line
// emitted while handling the request can carry it.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identifier := r.Header.Get("X-Request-Id")
		if identifier == "" {
			identifier = newRequestID()
		}
		w.Header().Set("X-Request-Id", identifier)
		ctx := context.WithValue(r.Context(), requestIDKey, identifier)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestIDKey is the context key for the request identifier.
var requestIDKey = contextKey("barq.request_id")

// requestIDFrom returns the request identifier of a request.
func requestIDFrom(r *http.Request) string {
	value, _ := r.Context().Value(requestIDKey).(string)
	return value
}

// newRequestID produces a random request identifier.
func newRequestID() string {
	return randomToken()[:16]
}

// securityHeadersMiddleware sets the response headers that a
// browser-facing application needs.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "same-origin")
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		// The embedded UI is entirely self contained. A strict policy
		// removes any chance of it loading an external resource.
		header.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// rateLimiter is a simple per-client token bucket.
//
// The bucket is deliberately small and the window short. The purpose
// is to blunt a runaway client, not to meter legitimate traffic.
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   float64
	last     time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// newRateLimiter creates a limiter that allows the given number of
// requests per second per client.
func newRateLimiter(perSecond float64) *rateLimiter {
	return &rateLimiter{
		buckets:  make(map[string]*bucket),
		capacity: perSecond * 2,
		refill:   perSecond,
		last:     time.Now(),
	}
}

// allow reports whether a client may proceed.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.last).Seconds()
	l.last = now

	entry, ok := l.buckets[key]
	if !ok {
		entry = &bucket{tokens: l.capacity, seen: now}
		l.buckets[key] = entry
	}
	entry.tokens += elapsed * l.refill
	if entry.tokens > l.capacity {
		entry.tokens = l.capacity
	}
	entry.seen = now

	// Expire idle buckets so that the map cannot grow without bound.
	if len(l.buckets) > 4096 {
		for id, candidate := range l.buckets {
			if now.Sub(candidate.seen) > 10*time.Minute {
				delete(l.buckets, id)
			}
		}
	}

	if entry.tokens < 1 {
		return false
	}
	entry.tokens--
	return true
}

// rateLimitMiddleware applies the limiter to the API routes.
func rateLimitMiddleware(limiter *rateLimiter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.allow(clientKey(r)) {
				writeAPIError(w, apierr.New(http.StatusTooManyRequests,
					apierr.CodeRateLimited, "too many requests"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientKey derives the rate limiting key of a request.
func clientKey(r *http.Request) string {
	// The user identifier is preferred when there is one, so that a
	// shared NAT does not punish unrelated users.
	if user, ok := r.Context().Value(userContextKey).(*auth.User); ok && user != nil {
		return "user:" + user.ID
	}
	address := r.RemoteAddr
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		address = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	if index := strings.LastIndex(address, ":"); index > 0 {
		address = address[:index]
	}
	return "ip:" + address
}

// pagination holds a validated page request.
type pagination struct {
	Page    int
	PerPage int
}

// maxPageSize caps how many records one page may return.
const maxPageSize = 100

// parsePagination reads and validates the page parameters.
//
// An out of range value is clamped rather than rejected, so that a
// client that asks for a hundred records per page does not get an
// error when the server caps it lower.
func parsePagination(r *http.Request) pagination {
	page := 1
	perPage := 20
	if raw := r.URL.Query().Get("page"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			page = value
		}
	}
	if raw := r.URL.Query().Get("per_page"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			perPage = value
			if perPage > maxPageSize {
				perPage = maxPageSize
			}
		}
	}
	return pagination{Page: page, PerPage: perPage}
}

// slice applies the pagination to a slice.
func slice[T any](values []T, page pagination) []T {
	start := (page.Page - 1) * page.PerPage
	if start >= len(values) {
		return nil
	}
	end := start + page.PerPage
	if end > len(values) {
		end = len(values)
	}
	return values[start:end]
}
