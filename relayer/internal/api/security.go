package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// securityMiddleware adds conservative HTTP security controls and a small
// in-process abuse guard. It is intentionally dependency-free. In a
// multi-instance deployment, put a real gateway/WAF or Redis-backed limiter in
// front of the API as the distributed rate-limit layer.
func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	limiter := newIPRateLimiter(120, time.Minute)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self' https:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		if !limiter.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many requests")
			return
		}

		// Reject unexpectedly large JSON bodies before handlers parse them.
		if r.Body != nil && r.ContentLength > 1<<20 {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	// Do not trust X-Forwarded-For here. If a reverse proxy is used, it should
	// terminate rate limiting itself or pass a sanitized client address.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

type ipRateLimiter struct {
	mu         sync.Mutex
	limit      int
	window     time.Duration
	entries    map[string]rateEntry
	maxEntries int
}

type rateEntry struct {
	count int
	reset time.Time
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{limit: limit, window: window, entries: make(map[string]rateEntry), maxEntries: 10000}
}

func (l *ipRateLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[ip]
	if e.reset.IsZero() || now.After(e.reset) {
		if len(l.entries) >= l.maxEntries {
			// Cheap bounded cleanup. Expired entries are discarded first; if a
			// burst of unique addresses fills the map, fail closed for the new
			// address rather than allowing unbounded memory growth.
			for key, entry := range l.entries {
				if now.After(entry.reset) {
					delete(l.entries, key)
				}
			}
			if len(l.entries) >= l.maxEntries {
				return false
			}
		}
		l.entries[ip] = rateEntry{count: 1, reset: now.Add(l.window)}
		return true
	}
	if e.count >= l.limit {
		return false
	}
	e.count++
	l.entries[ip] = e
	return true
}
