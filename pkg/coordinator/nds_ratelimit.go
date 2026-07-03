package coordinator

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type fixedWindowLimiter struct {
	mu      sync.Mutex
	entries map[string]rateLimitEntry
}

type rateLimitEntry struct {
	count int
	start time.Time
}

func newFixedWindowLimiter() *fixedWindowLimiter {
	return &fixedWindowLimiter{entries: make(map[string]rateLimitEntry)}
}

func (l *fixedWindowLimiter) allow(key string, limit int, window time.Duration, now time.Time) bool {
	if key == "" || limit <= 0 || window <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	entry := l.entries[key]
	if entry.start.IsZero() || now.Sub(entry.start) >= window {
		l.entries[key] = rateLimitEntry{count: 1, start: now}
		l.gcLocked(now, window)
		return true
	}
	if entry.count >= limit {
		return false
	}
	entry.count++
	l.entries[key] = entry
	return true
}

func (l *fixedWindowLimiter) gcLocked(now time.Time, window time.Duration) {
	for key, entry := range l.entries {
		if now.Sub(entry.start) >= 2*window {
			delete(l.entries, key)
		}
	}
}

func requestIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
