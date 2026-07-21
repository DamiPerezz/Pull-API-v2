package middleware

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"pull-api-v2/config"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// =============================================
// SECURITY MIDDLEWARE
// Comprehensive security hardening for the API
// =============================================

// SecurityConfig holds security configuration
type SecurityConfig struct {
	// Request limits
	MaxBodySize    int64
	MaxHeaderSize  int
	RequestTimeout time.Duration

	// Trusted proxies for accurate IP detection
	TrustedProxies []string

	// IP blocking
	blockedIPs   map[string]time.Time
	blockedIPsMu sync.RWMutex

	// Suspicious activity tracking
	suspiciousIPs   map[string]*suspiciousActivity
	suspiciousIPsMu sync.RWMutex
}

type suspiciousActivity struct {
	failedLogins   int
	lastAttempt    time.Time
	blockedUntil   time.Time
}

// Global security config
var Security *SecurityConfig

// InitSecurity initializes security configuration
func InitSecurity() {
	Security = &SecurityConfig{
		MaxBodySize:    10 * 1024 * 1024, // 10MB max body
		MaxHeaderSize:  8192,              // 8KB max header
		RequestTimeout: 30 * time.Second,
		TrustedProxies: []string{
			"127.0.0.1",
			"10.0.0.0/8",
			"172.16.0.0/12",
			"192.168.0.0/16",
		},
		blockedIPs:    make(map[string]time.Time),
		suspiciousIPs: make(map[string]*suspiciousActivity),
	}

	// Start cleanup goroutine
	go Security.cleanup()
}

// cleanup periodically removes expired blocks
func (s *SecurityConfig) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		now := time.Now()

		s.blockedIPsMu.Lock()
		for ip, expiry := range s.blockedIPs {
			if now.After(expiry) {
				delete(s.blockedIPs, ip)
			}
		}
		s.blockedIPsMu.Unlock()

		s.suspiciousIPsMu.Lock()
		for ip, activity := range s.suspiciousIPs {
			if now.After(activity.blockedUntil) && now.Sub(activity.lastAttempt) > time.Hour {
				delete(s.suspiciousIPs, ip)
			}
		}
		s.suspiciousIPsMu.Unlock()
	}
}

// =============================================
// IP DETECTION (Secure)
// =============================================

// GetRealIP extracts the real client IP, only trusting headers from trusted proxies
func GetRealIP(c *gin.Context) string {
	// IP real del comprador reenviada por NUESTRO proxy de Cloudflare Pages,
	// que la prueba con un secreto compartido (un ataque directo a fly.dev no
	// lo conoce). SIN esto, Fly-Client-IP es la IP de SALIDA de Cloudflare —
	// la misma para todos los compradores — y colapsa TODOS los cubos de rate
	// limit: al abrir la venta, cientos de personas compartirían el límite de
	// "5 pagos/min" → 429 masivo. Ver auditoría escalabilidad 2026-07-21 #1.
	if secret := os.Getenv("PROXY_SHARED_SECRET"); secret != "" {
		if auth := c.GetHeader("X-Pull-Proxy-Auth"); auth != "" &&
			hmac.Equal([]byte(auth), []byte(secret)) {
			if cip := c.GetHeader("X-Pull-Client-IP"); net.ParseIP(cip) != nil {
				return cip
			}
		}
	}

	// Fly.io sets Fly-Client-IP itself and it cannot be forged by the
	// client — prefer it over X-Forwarded-For, which callers can spoof to
	// rotate rate-limit buckets (see AUDITORIA-2026-07-19 hallazgo A2).
	if fip := c.GetHeader("Fly-Client-IP"); fip != "" {
		if net.ParseIP(fip) != nil {
			return fip
		}
	}

	clientIP := c.ClientIP()

	// Only trust forwarded headers if request comes from trusted proxy
	if Security != nil && isTrustedProxy(clientIP) {
		// Check X-Forwarded-For first
		if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
			// Get the first (original client) IP
			ips := strings.Split(xff, ",")
			if len(ips) > 0 {
				realIP := strings.TrimSpace(ips[0])
				if net.ParseIP(realIP) != nil {
					return realIP
				}
			}
		}

		// Check X-Real-IP
		if xri := c.GetHeader("X-Real-IP"); xri != "" {
			if net.ParseIP(xri) != nil {
				return xri
			}
		}
	}

	return clientIP
}

// isTrustedProxy checks if IP is in trusted proxy list
func isTrustedProxy(ip string) bool {
	if Security == nil {
		return false
	}

	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false
	}

	for _, trusted := range Security.TrustedProxies {
		if strings.Contains(trusted, "/") {
			// CIDR range
			_, network, err := net.ParseCIDR(trusted)
			if err == nil && network.Contains(parsedIP) {
				return true
			}
		} else {
			// Single IP
			if ip == trusted {
				return true
			}
		}
	}

	return false
}

// =============================================
// IP BLOCKING
// =============================================

// IsIPBlocked checks if an IP is currently blocked
func (s *SecurityConfig) IsIPBlocked(ip string) bool {
	s.blockedIPsMu.RLock()
	defer s.blockedIPsMu.RUnlock()

	if expiry, exists := s.blockedIPs[ip]; exists {
		return time.Now().Before(expiry)
	}
	return false
}

// BlockIP blocks an IP for a duration
func (s *SecurityConfig) BlockIP(ip string, duration time.Duration) {
	s.blockedIPsMu.Lock()
	defer s.blockedIPsMu.Unlock()

	s.blockedIPs[ip] = time.Now().Add(duration)
}

// RecordFailedLogin records a failed login attempt
// In development: more lenient thresholds
func (s *SecurityConfig) RecordFailedLogin(ip string) {
	// Skip for localhost in development
	if !config.IsProduction() && (ip == "127.0.0.1" || ip == "::1") {
		return
	}

	s.suspiciousIPsMu.Lock()
	defer s.suspiciousIPsMu.Unlock()

	activity, exists := s.suspiciousIPs[ip]
	if !exists {
		activity = &suspiciousActivity{}
		s.suspiciousIPs[ip] = activity
	}

	activity.failedLogins++
	activity.lastAttempt = time.Now()

	// Progressive blocking thresholds differ by environment
	threshold := 5  // Production: block after 5 failures
	if !config.IsProduction() {
		threshold = 20 // Development: more lenient, block after 20 failures
	}

	if activity.failedLogins >= threshold {
		duration := time.Duration(activity.failedLogins-threshold+1) * 5 * time.Minute
		maxDuration := time.Hour
		if !config.IsProduction() {
			maxDuration = 10 * time.Minute // Shorter max block in development
		}
		if duration > maxDuration {
			duration = maxDuration
		}
		activity.blockedUntil = time.Now().Add(duration)
		s.blockedIPs[ip] = activity.blockedUntil
	}
}

// ClearFailedLogins clears failed login count for an IP (on successful login)
func (s *SecurityConfig) ClearFailedLogins(ip string) {
	s.suspiciousIPsMu.Lock()
	defer s.suspiciousIPsMu.Unlock()

	delete(s.suspiciousIPs, ip)
}

// =============================================
// SECURITY MIDDLEWARE HANDLERS
// =============================================

// SecureHeaders adds comprehensive security headers
func SecureHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Prevent MIME type sniffing
		c.Header("X-Content-Type-Options", "nosniff")

		// Prevent clickjacking
		c.Header("X-Frame-Options", "DENY")

		// XSS protection (legacy browsers)
		c.Header("X-XSS-Protection", "1; mode=block")

		// Control referrer information
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")

		// Content Security Policy
		c.Header("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")

		// Permissions Policy (restrict browser features)
		c.Header("Permissions-Policy", "geolocation=(), microphone=(), camera=()")

		// Cache control for sensitive data
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Header("Cache-Control", "no-store, no-cache, must-revalidate, private")
			c.Header("Pragma", "no-cache")
		}

		// HSTS in production
		if config.IsProduction() {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		}

		c.Next()
	}
}

// RequestID adds a unique request ID for tracing
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check if request ID already exists (from load balancer)
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			// Generate new request ID
			bytes := make([]byte, 16)
			rand.Read(bytes)
			requestID = hex.EncodeToString(bytes)
		}

		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)

		c.Next()
	}
}

// IPBlockCheck checks if the client IP is blocked
// In development: more lenient, shorter blocks
// OPTIMIZED: GetRealIP called only once
func IPBlockCheck() gin.HandlerFunc {
	return func(c *gin.Context) {
		if Security == nil {
			c.Next()
			return
		}

		// Get IP once (avoid duplicate call)
		ip := GetRealIP(c)

		// Skip IP blocking in development for localhost
		if !config.IsProduction() {
			if ip == "127.0.0.1" || ip == "::1" || strings.HasPrefix(ip, "192.168.") {
				c.Next()
				return
			}
		}

		if Security.IsIPBlocked(ip) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "Too many failed attempts. Please try again later.",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// BodySizeLimit limits request body size
func BodySizeLimit(maxSize int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > maxSize {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": "Request body too large",
			})
			c.Abort()
			return
		}

		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSize)
		c.Next()
	}
}

// =============================================
// INPUT SANITIZATION (Enhanced Security)
// =============================================

// Dangerous patterns to detect (pre-compiled for performance)
// SECURITY: Enhanced patterns to catch more SQL injection and XSS bypass attempts
var (
	// SQL Injection patterns - expanded to catch more variants and bypass attempts
	sqlInjectionPattern = regexp.MustCompile(`(?i)(` +
		`union\s+select|` +
		`insert\s+into|` +
		`delete\s+from|` +
		`drop\s+(table|database|index)|` +
		`update\s+\w+\s+set|` +
		`alter\s+table|` +
		`create\s+(table|database|index)|` +
		`truncate\s+table|` +
		`exec(ute)?\s*\(|` +
		`xp_\w+|` +                             // SQL Server extended procedures
		`sp_\w+|` +                             // Stored procedures
		`sleep\s*\(|` +                         // Time-based SQL injection
		`benchmark\s*\(|` +                     // MySQL benchmark
		`waitfor\s+delay|` +                    // SQL Server time delay
		`load_file\s*\(|` +                     // MySQL file read
		`into\s+(out|dump)file|` +              // MySQL file write
		`information_schema|` +                 // Schema enumeration
		`;\s*-{2}|` +                           // SQL comment after semicolon
		`/\*.*\*/|` +                           // SQL block comments (bypass attempts)
		`'\s*or\s+'?[0-9]=|` +                  // Classic OR injection
		`'\s*and\s+'?[0-9]=|` +                 // Classic AND injection
		`\b(and|or)\s+[0-9]+\s*[=<>]|` +        // Numeric comparison injection
		`having\s+[0-9]+\s*[=<>]|` +            // HAVING clause injection
		`group\s+by\s+.+\s+having` +            // GROUP BY with HAVING
		`)`)

	// XSS patterns - expanded to catch more variants and event handlers
	xssPattern = regexp.MustCompile(`(?i)(` +
		`<\s*script|` +                         // Script tags (with optional whitespace)
		`javascript\s*:|` +                     // JavaScript protocol
		`vbscript\s*:|` +                       // VBScript protocol
		`on(click|load|error|mouse\w+|key\w+|focus|blur|change|submit|reset|select|abort|drag\w*|drop|contextmenu|input|invalid|scroll|wheel|copy|cut|paste|beforeunload|unload|hashchange|message|online|offline|popstate|storage|beforeprint|afterprint)\s*=|` + // Event handlers
		`<\s*iframe|` +
		`<\s*object|` +
		`<\s*embed|` +
		`<\s*applet|` +
		`<\s*form|` +
		`<\s*input|` +
		`<\s*button|` +
		`<\s*body|` +
		`<\s*img[^>]+onerror|` +                // img with onerror
		`<\s*svg[^>]*onload|` +                 // SVG with onload
		`<\s*link[^>]+rel\s*=|` +               // Potential stylesheet injection
		`<\s*meta[^>]+http-equiv|` +            // Meta refresh
		`expression\s*\(|` +                    // CSS expression
		`url\s*\(\s*["']?\s*javascript|` +      // CSS url() with javascript
		`@import|` +                            // CSS import
		`\beval\s*\(|` +                        // eval()
		`\bFunction\s*\(|` +                    // Function constructor
		`\bsetTimeout\s*\(|` +                  // setTimeout with string
		`\bsetInterval\s*\(|` +                 // setInterval with string
		`document\s*\.\s*(cookie|write|location|domain)|` + // DOM manipulation
		`window\s*\.\s*(location|open)|` +      // Window manipulation
		`\.innerHTML\s*=|` +                    // innerHTML assignment
		`\.outerHTML\s*=|` +                    // outerHTML assignment
		`fromCharCode|` +                       // String encoding bypass
		`&#\d+;|` +                             // HTML decimal encoding
		`&#x[0-9a-f]+;` +                       // HTML hex encoding
		`)`)

	pathTraversalPattern = regexp.MustCompile(`\.\.[/\\]|%2e%2e[/\\%]|%252e`)
	htmlTagPattern       = regexp.MustCompile(`<[^>]*>`)
	controlCharPattern   = regexp.MustCompile(`[\x00-\x1f\x7f]`)

	// SQL comment patterns for normalization
	sqlCommentPattern = regexp.MustCompile(`(/\*[^*]*\*+(?:[^/*][^*]*\*+)*/|--[^\n]*|#[^\n]*)`)
)

// normalizeForValidation normalizes input to prevent bypass attempts
// Decodes common encoding schemes and removes SQL comments
func normalizeForValidation(s string) string {
	// URL decode (catches %27 for ', %3B for ;, etc.)
	decoded, err := url.QueryUnescape(s)
	if err != nil {
		decoded = s
	}

	// Double URL decode (catches %2527 → %27 → ')
	if doubleDecoded, err := url.QueryUnescape(decoded); err == nil {
		decoded = doubleDecoded
	}

	// Remove SQL comments that could be used to bypass detection
	decoded = sqlCommentPattern.ReplaceAllString(decoded, " ")

	// Remove null bytes (used to terminate strings early)
	decoded = strings.ReplaceAll(decoded, "\x00", "")
	decoded = strings.ReplaceAll(decoded, "%00", "")

	// Normalize whitespace (multiple spaces → single space)
	decoded = strings.Join(strings.Fields(decoded), " ")

	return decoded
}

// SanitizeInput checks for common attack patterns
// OPTIMIZED: Check raw query string instead of parsing all parameters
// SECURITY: Enhanced with input normalization to prevent bypass attempts
func SanitizeInput() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check URL path for traversal attacks
		path := c.Request.URL.Path
		normalizedPath := normalizeForValidation(path)
		if pathTraversalPattern.MatchString(normalizedPath) {
			log.Printf("[Security] Path traversal attempt blocked: %s", c.ClientIP())
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			c.Abort()
			return
		}

		// OPTIMIZED: Check raw query string instead of iterating parsed params
		// SECURITY: Normalize before checking to catch encoded attacks
		rawQuery := c.Request.URL.RawQuery
		if rawQuery != "" {
			normalizedQuery := normalizeForValidation(rawQuery)
			if containsDangerousPattern(normalizedQuery) {
				log.Printf("[Security] Dangerous pattern blocked from %s: %s", c.ClientIP(), rawQuery[:min(50, len(rawQuery))])
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
				c.Abort()
				return
			}
		}

		c.Next()
	}
}

// containsDangerousPattern checks if a string contains dangerous patterns
func containsDangerousPattern(s string) bool {
	return sqlInjectionPattern.MatchString(s) || xssPattern.MatchString(s)
}

// min returns the smaller of two integers (helper for Go < 1.21 compatibility)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SanitizeString removes potentially dangerous characters from a string
func SanitizeString(s string) string {
	// Remove null bytes
	s = strings.ReplaceAll(s, "\x00", "")

	// Trim whitespace
	s = strings.TrimSpace(s)

	// Limit length
	if len(s) > 1000 {
		s = s[:1000]
	}

	return s
}

// SanitizeName specifically sanitizes name fields (uses pre-compiled regex)
func SanitizeName(name string) string {
	// Remove HTML tags (uses pre-compiled regex)
	name = htmlTagPattern.ReplaceAllString(name, "")

	// Remove control characters (uses pre-compiled regex)
	name = controlCharPattern.ReplaceAllString(name, "")

	// Trim and limit
	name = strings.TrimSpace(name)
	if len(name) > 100 {
		name = name[:100]
	}

	return name
}

// SanitizeEmail normalizes and validates email
func SanitizeEmail(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))

	// Basic length check
	if len(email) > 254 {
		return ""
	}

	return email
}

// =============================================
// UUID VALIDATION
// =============================================

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsValidUUID checks if a string is a valid UUID
func IsValidUUID(s string) bool {
	return uuidPattern.MatchString(strings.ToLower(s))
}

// ValidateUUIDParam validates a URL parameter as UUID
func ValidateUUIDParam(param string) gin.HandlerFunc {
	return func(c *gin.Context) {
		value := c.Param(param)
		if value != "" && !IsValidUUID(value) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Invalid " + param + " format",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// =============================================
// ERROR HANDLING (Safe)
// =============================================

// SafeError returns a safe error message without leaking implementation details
// SECURITY: In production, internal errors are logged but NOT exposed to clients
// This prevents information leakage that could aid attackers
func SafeError(c *gin.Context, status int, publicMessage string, internalErr error) {
	// Get request context for logging
	requestID, _ := c.Get("request_id")
	if requestID == nil {
		requestID = "unknown"
	}

	// Always log internal errors for debugging (server-side only)
	if internalErr != nil {
		log.Printf("[Error] [%s] %s - Internal: %v (IP: %s, Path: %s)",
			requestID, publicMessage, internalErr, c.ClientIP(), c.Request.URL.Path)
	}

	// Return safe message to client
	response := gin.H{"error": publicMessage}

	// SECURITY: Only include error details in development mode
	// Production must NEVER expose internal error messages
	if !config.IsProduction() && internalErr != nil {
		response["details"] = internalErr.Error()
		response["request_id"] = requestID // Helpful for dev debugging
	} else if config.IsProduction() {
		// In production, include request_id so users can reference it in support
		response["request_id"] = requestID
	}

	c.JSON(status, response)
}

// SafeErrorAbort is like SafeError but also aborts the middleware chain
// Use this in middleware when you need to stop processing
func SafeErrorAbort(c *gin.Context, status int, publicMessage string, internalErr error) {
	SafeError(c, status, publicMessage, internalErr)
	c.Abort()
}

// =============================================
// SECURE CORS
// =============================================

// SecureCORS provides secure CORS handling
// In development: allows all origins for easy testing
// In production: strict origin validation
func SecureCORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		if origin != "" {
			if !config.IsProduction() {
				// DEVELOPMENT: Allow all origins for local testing
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Access-Control-Allow-Credentials", "true")
			} else if config.IsOriginAllowed(origin) {
				// PRODUCTION: Only allow whitelisted origins
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Access-Control-Allow-Credentials", "true")
			} else {
				// PRODUCTION: Reject unknown origins
				c.JSON(http.StatusForbidden, gin.H{"error": "Origin not allowed"})
				c.Abort()
				return
			}
		}

		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-Request-ID")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		c.Header("Access-Control-Max-Age", "86400") // Cache preflight for 24h
		c.Header("Access-Control-Expose-Headers", "X-Request-ID") // Allow frontend to read request ID

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// =============================================
// WEBHOOK SECURITY
// =============================================

// WebhookRateLimit applies stricter rate limiting for webhooks
func WebhookRateLimit() gin.HandlerFunc {
	limiter := newRateLimiter(50, time.Minute) // 50 webhook calls per minute

	return func(c *gin.Context) {
		ip := GetRealIP(c)
		if !limiter.allow(ip) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "Too many requests",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// =============================================
// PANIC RECOVERY (Safe)
// =============================================

// SafeRecovery recovers from panics without leaking stack traces
func SafeRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				requestID, _ := c.Get("request_id")
				_ = requestID // Log with request ID

				// Return generic error
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": "Internal server error",
				})
				c.Abort()
			}
		}()
		c.Next()
	}
}
