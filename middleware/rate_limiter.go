package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// =============================================
// RATE LIMITER MIDDLEWARE
// In-memory rate limiting for API protection
// OPTIMIZED: Object pooling + factory pattern
// =============================================

type rateLimiter struct {
	requests map[string]*clientRequests
	mu       sync.RWMutex
	limit    int
	window   time.Duration
}

type clientRequests struct {
	count     int
	resetTime time.Time
}

// Pool for clientRequests to reduce allocations
var clientRequestsPool = sync.Pool{
	New: func() interface{} {
		return &clientRequests{}
	},
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		requests: make(map[string]*clientRequests),
		limit:    limit,
		window:   window,
	}
	return rl
}

// Start a single cleanup goroutine for all rate limiters
func init() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			authLimiter.cleanup()
			generalLimiter.cleanup()
			paymentLimiter.cleanup()
			createLimiter.cleanup()
		}
	}()
}

func (rl *rateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	for key, client := range rl.requests {
		if now.After(client.resetTime) {
			delete(rl.requests, key)
			// Return to pool
			client.count = 0
			clientRequestsPool.Put(client)
		}
	}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	client, exists := rl.requests[key]

	if !exists || now.After(client.resetTime) {
		// Get from pool instead of allocating
		newClient := clientRequestsPool.Get().(*clientRequests)
		newClient.count = 1
		newClient.resetTime = now.Add(rl.window)
		rl.requests[key] = newClient
		return true
	}

	if client.count >= rl.limit {
		return false
	}

	client.count++
	return true
}

// Global rate limiters
// Límites por IP REAL (GetRealIP resuelve la IP del comprador vía el proxy,
// no la de salida de Cloudflare — ver security.go). Calibrados para un
// evento con miles de personas: generosos para no estrangular a compradores
// legítimos (incl. NAT compartido de wifi del local o de la operadora), y el
// anti-carding real vive en pay_guard (por-orden y por-tarjeta), NO aquí.
var (
	authLimiter    = newRateLimiter(30, time.Minute)  // 30 auth/min por IP
	generalLimiter = newRateLimiter(300, time.Minute) // 300 req/min por IP (browsing)
	paymentLimiter = newRateLimiter(40, time.Minute)  // 40 pagos/min por IP (NAT compartido)
	createLimiter  = newRateLimiter(80, time.Minute)  // 80 creates/min por IP
)

// createRateLimitHandler creates a rate limit middleware (factory pattern)
func createRateLimitHandler(limiter *rateLimiter, errMsg string) func() gin.HandlerFunc {
	// Pre-create the handler to avoid allocation on each call
	handler := func(c *gin.Context) {
		if !limiter.allow(GetRealIP(c)) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": errMsg})
			c.Abort()
			return
		}
		c.Next()
	}
	// Return a function that returns the same handler
	return func() gin.HandlerFunc {
		return handler
	}
}

// Rate limit middleware functions using factory
var (
	// RateLimitAuth limits authentication attempts
	RateLimitAuth = createRateLimitHandler(authLimiter, "Too many authentication attempts. Please try again later.")
	// RateLimitGeneral limits general API requests
	RateLimitGeneral = createRateLimitHandler(generalLimiter, "Too many requests. Please slow down.")
	// RateLimitPayment limits payment-related requests
	RateLimitPayment = createRateLimitHandler(paymentLimiter, "Too many payment attempts. Please wait a moment.")
	// RateLimitCreate limits resource creation
	RateLimitCreate = createRateLimitHandler(createLimiter, "Too many requests. Please try again later.")
)
