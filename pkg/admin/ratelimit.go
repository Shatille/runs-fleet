package admin

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

var rateLimitLog = logging.WithComponent(logging.LogTypeAdmin, "ratelimit")

type ipBucket struct {
	tokens    atomic.Int64
	lastSeen  atomic.Int64
	resetTime atomic.Int64
}

// RateLimiter provides per-IP token bucket rate limiting for HTTP handlers.
type RateLimiter struct {
	limit    int
	window   time.Duration
	buckets  sync.Map
	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewRateLimiter creates a rate limiter with the given requests-per-minute limit.
// It starts a background goroutine to clean up expired entries every 5 minutes.
func NewRateLimiter(requestsPerMinute int) *RateLimiter {
	if requestsPerMinute <= 0 {
		requestsPerMinute = 60
	}

	rl := &RateLimiter{
		limit:  requestsPerMinute,
		window: time.Minute,
		stopCh: make(chan struct{}),
	}

	go rl.cleanup()
	return rl
}

// Stop terminates the background cleanup goroutine.
func (rl *RateLimiter) Stop() {
	rl.stopOnce.Do(func() {
		close(rl.stopCh)
	})
}

// Wrap returns an http.Handler that enforces rate limiting before calling next.
func (rl *RateLimiter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := extractClientIP(r)
		if !rl.allow(ip) {
			retryAfter := rl.retryAfterSeconds(ip)
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(ErrorResponse{
				Error: "Rate limit exceeded",
			})
			rateLimitLog.Warn("rate limit exceeded",
				slog.String(logging.KeyRemoteAddr, ip),
				slog.String("path", r.URL.Path))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) allow(ip string) bool {
	now := time.Now().UnixMilli()

	val, loaded := rl.buckets.LoadOrStore(ip, &ipBucket{})
	bucket, ok := val.(*ipBucket)
	if !ok {
		rateLimitLog.Error("unexpected type in rate limit bucket map", slog.Any("value", val))
		rl.buckets.Delete(ip)
		return true
	}

	if !loaded {
		bucket.tokens.Store(int64(rl.limit - 1))
		bucket.lastSeen.Store(now)
		bucket.resetTime.Store(now + rl.window.Milliseconds())
		return true
	}

	resetTime := bucket.resetTime.Load()
	if now >= resetTime {
		bucket.tokens.Store(int64(rl.limit - 1))
		bucket.lastSeen.Store(now)
		bucket.resetTime.Store(now + rl.window.Milliseconds())
		return true
	}

	bucket.lastSeen.Store(now)
	remaining := bucket.tokens.Add(-1)
	return remaining >= 0
}

func (rl *RateLimiter) retryAfterSeconds(ip string) int {
	val, loaded := rl.buckets.Load(ip)
	if !loaded {
		return 1
	}
	bucket, ok := val.(*ipBucket)
	if !ok {
		return 1
	}
	resetTime := bucket.resetTime.Load()
	now := time.Now().UnixMilli()
	remaining := resetTime - now
	if remaining <= 0 {
		return 1
	}
	seconds := int(remaining/1000) + 1
	return seconds
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-2 * rl.window).UnixMilli()
			rl.buckets.Range(func(key, value any) bool {
				bucket, ok := value.(*ipBucket)
				if !ok {
					rl.buckets.Delete(key)
					return true
				}
				if bucket.lastSeen.Load() < cutoff {
					rl.buckets.Delete(key)
				}
				return true
			})
		}
	}
}

func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if parts := strings.SplitN(xff, ",", 2); len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
