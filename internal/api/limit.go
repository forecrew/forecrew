package api

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/auth"
)

// A public route answers an address this many times per window: enough for
// the people of one office signing in at once, too few to walk through email
// addresses and slugs. forecrewd sees the address of the connection, so
// behind a reverse proxy this is one limit for everybody.
const (
	publicRouteLimit  = 120
	publicRouteWindow = time.Minute
)

// ipLimit counts calls per address in fixed windows. It lives in memory, so a
// restart forgets it.
type ipLimit struct {
	mu    sync.Mutex
	now   func() time.Time
	start time.Time
	calls map[string]int
}

// allow counts a call from ip and says whether it is within the limit.
func (l *ipLimit) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := l.now(); now.Sub(l.start) >= publicRouteWindow {
		l.start = now
		clear(l.calls)
	}
	l.calls[ip]++
	return l.calls[ip] <= publicRouteLimit
}

// limited answers 429 to an address past the limit of public routes.
func (s *Server) limited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.discover.allow(auth.RemoteIP(r)) {
			w.Header().Set("Retry-After", strconv.Itoa(int(publicRouteWindow.Seconds())))
			s.fail(w, r, errors.New("too many requests, try again in a minute"), http.StatusTooManyRequests, "rate_limited")
			return
		}
		next.ServeHTTP(w, r)
	})
}
