package controllers

import (
	"context"
	"net/http"
	"pull-api-v2/config"
	"pull-api-v2/middleware"
	"pull-api-v2/models"
	"pull-api-v2/services"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// =============================================
// PLATFORM AUTHENTICATION - SECURE VERSION
// Features:
// - Access Token (15 min) + Refresh Token (2 days)
// - HTTP-only Secure Cookies
// - Refresh Token Rotation
// - Session tracking in pull_staff_sessions
// =============================================

// PlatformSecureLoginRequest represents platform login request
type PlatformSecureLoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=6"`
}

// PlatformSecureLoginResponse represents the login response
type PlatformSecureLoginResponse struct {
	Staff     map[string]interface{} `json:"staff"`
	ExpiresIn int64                  `json:"expires_in"` // Access token expiry in seconds
}

// PlatformSecureLogin handles secure platform staff login with cookies
// POST /api/v1/platform/auth/login
func PlatformSecureLogin(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req PlatformSecureLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request format",
			"details": err.Error(),
		})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	clientIP := middleware.GetRealIP(c)
	userAgent := c.Request.UserAgent()

	// Query central database
	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service temporarily unavailable"})
		return
	}

	// Find platform staff
	staff, err := central.QueryOne(ctx, "pull_staff", map[string]interface{}{
		"select": "id,email,password_hash,name,role,is_active,failed_login_attempts,locked_until",
		"where": map[string]interface{}{
			"email": email,
		},
	})

	if err != nil || staff == nil {
		// SECURITY: Record failed login attempt
		if middleware.Security != nil {
			middleware.Security.RecordFailedLogin(clientIP)
		}
		// Audit log (fire-and-forget)
		go logFailedLoginAttempt(central, email, clientIP, userAgent, "user_not_found")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
		return
	}

	staffID := services.GetString(staff, "id")

	// Check if active
	if !services.GetBool(staff, "is_active") {
		go logFailedLoginAttempt(central, email, clientIP, userAgent, "account_disabled")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Account is disabled"})
		return
	}

	// Check if locked
	lockedUntil := services.GetTime(staff, "locked_until")
	if lockedUntil != nil && time.Now().Before(*lockedUntil) {
		go logFailedLoginAttempt(central, email, clientIP, userAgent, "account_locked")
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":        "Account is temporarily locked due to too many failed attempts",
			"locked_until": lockedUntil.Format(time.RFC3339),
			"retry_after":  int64(time.Until(*lockedUntil).Seconds()),
		})
		return
	}

	// Verify password
	passwordHash := services.GetString(staff, "password_hash")
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
		// SECURITY: Record failed login
		if middleware.Security != nil {
			middleware.Security.RecordFailedLogin(clientIP)
		}

		// Increment failed attempts
		go incrementFailedAttempts(central, staffID, staff)
		go logFailedLoginAttempt(central, email, clientIP, userAgent, "invalid_password")

		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
		return
	}

	// SECURITY: Clear failed login attempts on successful auth
	if middleware.Security != nil {
		middleware.Security.ClearFailedLogins(clientIP)
	}

	// Create platform staff model
	platformStaff := &models.PlatformStaff{
		ID:    staffID,
		Email: services.GetString(staff, "email"),
		Name:  services.GetString(staff, "name"),
		Role:  services.GetString(staff, "role"),
	}

	// Generate secure session ID
	sessionID := generateSessionID()

	// Generate token pair
	tokenPair, err := services.GenerateSecureTokenPair(platformStaff, sessionID, clientIP, userAgent)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session"})
		return
	}

	// Create session in database
	_, err = services.CreateSession(ctx, staffID, tokenPair.RefreshToken, clientIP, userAgent)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session"})
		return
	}

	// Set HTTP-only secure cookies
	setAuthCookies(c, tokenPair)

	// Update last login and reset failed attempts (fire-and-forget)
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()

		central.UpdateNoReturn(bgCtx, "pull_staff", map[string]interface{}{
			"failed_login_attempts": 0,
			"locked_until":          nil,
			"last_login_at":         time.Now().Format(time.RFC3339),
			"last_login_ip":         clientIP,
		}, map[string]interface{}{"id": staffID})

		// Audit log
		central.InsertCtx(bgCtx, "pull_staff_audit_log", map[string]interface{}{
			"staff_id":    staffID,
			"action":      "login_success",
			"ip_address":  clientIP,
			"user_agent":  userAgent,
			"metadata":    map[string]interface{}{"session_id": sessionID},
			"created_at":  time.Now().Format(time.RFC3339),
		})
	}()

	c.JSON(http.StatusOK, PlatformSecureLoginResponse{
		Staff: map[string]interface{}{
			"id":    staffID,
			"email": platformStaff.Email,
			"name":  platformStaff.Name,
			"role":  platformStaff.Role,
		},
		ExpiresIn: tokenPair.ExpiresIn,
	})
}

// PlatformRefreshToken refreshes the access token using refresh token cookie
// POST /api/v1/platform/auth/refresh
func PlatformRefreshToken(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Get refresh token from cookie
	refreshToken, err := c.Cookie(services.RefreshTokenCookie)
	if err != nil || refreshToken == "" {
		clearAuthCookies(c)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "No refresh token"})
		return
	}

	clientIP := middleware.GetRealIP(c)
	userAgent := c.Request.UserAgent()

	// Refresh session (with rotation)
	newTokenPair, err := services.RefreshSession(ctx, refreshToken, clientIP, userAgent)
	if err != nil {
		clearAuthCookies(c)
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":  "Session expired or invalid",
			"detail": err.Error(),
		})
		return
	}

	// Set new cookies
	setAuthCookies(c, newTokenPair)

	// Update session activity (fire-and-forget)
	claims, _ := services.ValidateSessionToken(newTokenPair.AccessToken)
	if claims != nil {
		go services.UpdateSessionActivity(context.Background(), claims.SessionID)
	}

	c.JSON(http.StatusOK, gin.H{
		"expires_in": newTokenPair.ExpiresIn,
		"message":    "Token refreshed successfully",
	})
}

// PlatformLogout logs out the current session
// POST /api/v1/platform/auth/logout
func PlatformLogout(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// Get session info from context (set by middleware)
	sessionID := c.GetString("session_id")
	staffID := c.GetString("staff_id")

	if sessionID != "" && staffID != "" {
		// Revoke session
		services.RevokeSession(ctx, sessionID, staffID)

		// Audit log (fire-and-forget)
		go func() {
			central := services.DB.Central()
			if central != nil {
				bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer bgCancel()
				central.InsertCtx(bgCtx, "pull_staff_audit_log", map[string]interface{}{
					"staff_id":   staffID,
					"action":     "logout",
					"ip_address": middleware.GetRealIP(c),
					"user_agent": c.Request.UserAgent(),
					"metadata":   map[string]interface{}{"session_id": sessionID},
					"created_at": time.Now().Format(time.RFC3339),
				})
			}
		}()
	}

	// Clear cookies
	clearAuthCookies(c)

	c.JSON(http.StatusOK, gin.H{"message": "Logged out successfully"})
}

// PlatformLogoutAll logs out all sessions for the current user
// POST /api/v1/platform/auth/logout-all
func PlatformLogoutAll(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	staffID := c.GetString("staff_id")
	if staffID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid session"})
		return
	}

	// Revoke all sessions
	err := services.RevokeAllSessions(ctx, staffID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to logout all sessions"})
		return
	}

	// Audit log (fire-and-forget)
	go func() {
		central := services.DB.Central()
		if central != nil {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer bgCancel()
			central.InsertCtx(bgCtx, "pull_staff_audit_log", map[string]interface{}{
				"staff_id":   staffID,
				"action":     "logout_all_sessions",
				"ip_address": middleware.GetRealIP(c),
				"user_agent": c.Request.UserAgent(),
				"created_at": time.Now().Format(time.RFC3339),
			})
		}
	}()

	// Clear cookies
	clearAuthCookies(c)

	c.JSON(http.StatusOK, gin.H{"message": "All sessions logged out"})
}

// PlatformGetSessions returns all active sessions for the current user
// GET /api/v1/platform/auth/sessions
func PlatformGetSessions(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	staffID := c.GetString("staff_id")
	if staffID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid session"})
		return
	}

	currentSessionID := c.GetString("session_id")

	sessions, err := services.GetActiveSessions(ctx, staffID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get sessions"})
		return
	}

	// Mark current session
	for _, session := range sessions {
		if services.GetString(session, "id") == currentSessionID {
			session["is_current"] = true
		} else {
			session["is_current"] = false
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"sessions": sessions,
		"count":    len(sessions),
	})
}

// PlatformRevokeSession revokes a specific session
// DELETE /api/v1/platform/auth/sessions/:session_id
func PlatformRevokeSession(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	staffID := c.GetString("staff_id")
	// La ruta registra `:session_id` (main.go), pero esto leía `:id` y salía
	// siempre vacío → 400 "Invalid request" pasara lo que pasara. Leemos los
	// dos nombres, como hace el resto del repo con eventId/event_id.
	targetSessionID := c.Param("session_id")
	if targetSessionID == "" {
		targetSessionID = c.Param("id")
	}

	if staffID == "" || targetSessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// Don't allow revoking current session through this endpoint
	currentSessionID := c.GetString("session_id")
	if targetSessionID == currentSessionID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Use logout endpoint for current session"})
		return
	}

	err := services.RevokeSession(ctx, targetSessionID, staffID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke session"})
		return
	}

	// Audit log (fire-and-forget)
	go func() {
		central := services.DB.Central()
		if central != nil {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer bgCancel()
			central.InsertCtx(bgCtx, "pull_staff_audit_log", map[string]interface{}{
				"staff_id":   staffID,
				"action":     "revoke_session",
				"ip_address": middleware.GetRealIP(c),
				"metadata":   map[string]interface{}{"revoked_session_id": targetSessionID},
				"created_at": time.Now().Format(time.RFC3339),
			})
		}
	}()

	c.JSON(http.StatusOK, gin.H{"message": "Session revoked"})
}

// PlatformVerifySession verifies the current session is valid
// GET /api/v1/platform/auth/verify
func PlatformVerifySession(c *gin.Context) {
	// If we reached here, the middleware already validated the token
	staffID := c.GetString("staff_id")
	sessionID := c.GetString("session_id")
	role := c.GetString("role")
	email := c.GetString("email")

	c.JSON(http.StatusOK, gin.H{
		"valid": true,
		"staff": gin.H{
			"id":    staffID,
			"email": email,
			"role":  role,
		},
		"session_id": sessionID,
	})
}

// PlatformGetCurrentUser returns the current authenticated user's info
// GET /api/v1/platform/auth/me
func PlatformGetCurrentUser(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	staffID := c.GetString("staff_id")
	if staffID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid session"})
		return
	}

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service temporarily unavailable"})
		return
	}

	// Get full staff info
	staff, err := central.QueryOne(ctx, "pull_staff", map[string]interface{}{
		"select": "id,email,name,role,is_active,last_login_at,created_at",
		"where": map[string]interface{}{
			"id": staffID,
		},
	})

	if err != nil || staff == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"staff": map[string]interface{}{
			"id":            services.GetString(staff, "id"),
			"email":         services.GetString(staff, "email"),
			"name":          services.GetString(staff, "name"),
			"role":          services.GetString(staff, "role"),
			"is_active":     services.GetBool(staff, "is_active"),
			"last_login_at": staff["last_login_at"],
			"created_at":    staff["created_at"],
		},
		"session_id": c.GetString("session_id"),
	})
}

// =============================================
// COOKIE HELPERS
// =============================================

// setAuthCookies sets HTTP-only secure cookies for authentication
func setAuthCookies(c *gin.Context, tokenPair *services.TokenPair) {
	isSecure := config.IsProduction()
	domain := config.App.CookieDomain
	sameSite := http.SameSiteLaxMode

	if config.App.CookieSameSite == "Strict" {
		sameSite = http.SameSiteStrictMode
	} else if config.App.CookieSameSite == "None" {
		sameSite = http.SameSiteNoneMode
	}

	// Access Token Cookie (short-lived: 15 min)
	c.SetSameSite(sameSite)
	c.SetCookie(
		services.AccessTokenCookie,
		tokenPair.AccessToken,
		int(services.AccessTokenExpiry.Seconds()),
		"/",
		domain,
		isSecure,
		true, // HttpOnly
	)

	// Refresh Token Cookie (long-lived: 2 days)
	c.SetCookie(
		services.RefreshTokenCookie,
		tokenPair.RefreshToken,
		int(services.RefreshTokenExpiry.Seconds()),
		"/api/v1/platform/auth", // Only sent to auth endpoints
		domain,
		isSecure,
		true, // HttpOnly
	)
}

// clearAuthCookies removes authentication cookies
func clearAuthCookies(c *gin.Context) {
	domain := config.App.CookieDomain
	isSecure := config.IsProduction()

	// Clear access token
	c.SetCookie(
		services.AccessTokenCookie,
		"",
		-1,
		"/",
		domain,
		isSecure,
		true,
	)

	// Clear refresh token
	c.SetCookie(
		services.RefreshTokenCookie,
		"",
		-1,
		"/api/v1/platform/auth",
		domain,
		isSecure,
		true,
	)
}

// =============================================
// HELPER FUNCTIONS
// =============================================

// generateSessionID creates a unique session identifier
func generateSessionID() string {
	return uuid.New().String()
}

// incrementFailedAttempts increments failed login attempts and locks if needed
func incrementFailedAttempts(central *services.SupabaseClient, staffID string, staff map[string]interface{}) {
	bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bgCancel()

	attempts := services.GetInt(staff, "failed_login_attempts") + 1
	updateData := map[string]interface{}{
		"failed_login_attempts": attempts,
	}

	// Lock after 5 failed attempts for 15 minutes
	if attempts >= 5 {
		updateData["locked_until"] = time.Now().Add(15 * time.Minute).Format(time.RFC3339)
	}

	central.UpdateNoReturn(bgCtx, "pull_staff", updateData, map[string]interface{}{"id": staffID})
}

// logFailedLoginAttempt logs failed login attempts for security audit
func logFailedLoginAttempt(central *services.SupabaseClient, email, ip, userAgent, reason string) {
	bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bgCancel()

	central.InsertCtx(bgCtx, "pull_staff_audit_log", map[string]interface{}{
		"action":     "login_failed",
		"ip_address": ip,
		"user_agent": userAgent,
		"metadata": map[string]interface{}{
			"email":  email,
			"reason": reason,
		},
		"created_at": time.Now().Format(time.RFC3339),
	})
}
