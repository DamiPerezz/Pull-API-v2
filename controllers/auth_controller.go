package controllers

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"pull-api-v2/middleware"
	"pull-api-v2/models"
	"pull-api-v2/services"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// =============================================
// STAFF AUTHENTICATION
// =============================================

// LoginStaffRequest represents the login request body.
// VenueID is optional — when blank we fall back to the first active venue
// (the mobile app doesn't ship a venue picker yet in the demo build).
type LoginStaffRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=6"`
	VenueID  string `json:"venue_id"`
}

// LoginStaffResponse represents the login response
type LoginStaffResponse struct {
	Token string                 `json:"token"`
	Staff map[string]interface{} `json:"staff"`
	Venue map[string]interface{} `json:"venue"`
}

// LoginStaff handles staff login
// POST /api/v1/auth/login-staff
// OPTIMIZED: Parallel role + venue fetch after password validation
func LoginStaff(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var req LoginStaffRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.SafeError(c, http.StatusBadRequest, "Invalid request format", err)
		return
	}

	// Get client IP for security tracking
	clientIP := middleware.GetRealIP(c)

	// Resolve venue_id: explicit > first active venue (demo fallback).
	if req.VenueID == "" {
		if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id",
			"where":  map[string]interface{}{"is_active": true, "deleted_at": "is.null"},
			"limit":  1,
		}); v != nil {
			req.VenueID = services.GetString(v, "id")
		}
	}
	if req.VenueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id is required"})
		return
	}

	// Get venue database
	venueDB := services.DB.ForVenue(req.VenueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Find staff by email
	staff, err := venueDB.QueryOne(ctx, "organization_workers", map[string]interface{}{
		"select": "id,email,password_hash,first_name,last_name,role_id,is_active,failed_login_attempts,locked_until",
		"where": map[string]interface{}{
			"email":      strings.ToLower(req.Email),
			"deleted_at": "is.null",
		},
	})

	if err != nil || staff == nil {
		// SECURITY: Record failed login attempt
		if middleware.Security != nil {
			middleware.Security.RecordFailedLogin(clientIP)
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
		return
	}

	// Check if active
	if !services.GetBool(staff, "is_active") {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Account is disabled"})
		return
	}

	// SECURITY: Per-account lockout — IP-based limits alone are evadable.
	staffID := services.GetString(staff, "id")
	if lockedUntil := services.GetTime(staff, "locked_until"); lockedUntil != nil && lockedUntil.After(time.Now()) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Account temporarily locked. Try again later."})
		return
	}

	// Verify password
	passwordHash := services.GetString(staff, "password_hash")
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
		// SECURITY: Record failed login attempt (IP + per-account counter)
		if middleware.Security != nil {
			middleware.Security.RecordFailedLogin(clientIP)
		}
		attempts := services.GetInt(staff, "failed_login_attempts") + 1
		update := map[string]interface{}{"failed_login_attempts": attempts}
		if attempts >= 5 {
			update["locked_until"] = time.Now().Add(15 * time.Minute).Format(time.RFC3339)
			update["failed_login_attempts"] = 0
		}
		venueDB.UpdateNoReturn(ctx, "organization_workers", update, map[string]interface{}{"id": staffID})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
		return
	}

	// SECURITY: Clear failed login attempts on successful auth
	if middleware.Security != nil {
		middleware.Security.ClearFailedLogins(clientIP)
	}
	if services.GetInt(staff, "failed_login_attempts") > 0 {
		venueDB.UpdateNoReturn(ctx, "organization_workers", map[string]interface{}{
			"failed_login_attempts": 0, "locked_until": nil,
		}, map[string]interface{}{"id": staffID})
	}

	roleID := services.GetString(staff, "role_id")

	// OPTIMIZATION: Parallel role + venue fetch
	var role map[string]interface{}
	var venue *models.Venue
	var venueErr error
	var wg sync.WaitGroup

	wg.Add(2)

	// Get role
	go func() {
		defer wg.Done()
		role, _ = venueDB.QueryOne(ctx, "roles", map[string]interface{}{
			"select": "name",
			"where":  map[string]interface{}{"id": roleID},
		})
	}()

	// Get venue info from central
	go func() {
		defer wg.Done()
		venue, venueErr = services.DB.GetVenue(ctx, req.VenueID)
	}()

	wg.Wait()

	if venueErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get venue info"})
		return
	}

	roleName := "staff"
	if role != nil {
		roleName = services.GetString(role, "name")
	}

	// Generate token. We use the full GenerateStaffToken so the JWT carries
	// organization_id / email / name — the PullMobileApp-GL decodes the JWT
	// and reads those fields directly to populate organization_id_real /
	// employee_id_real in its auth store.
	staffModel := &models.Staff{
		ID:             staffID,
		Email:          services.GetString(staff, "email"),
		Name:           services.GetString(staff, "first_name") + " " + services.GetString(staff, "last_name"),
		VenueID:        req.VenueID,
		OrganizationID: venue.OrganizationID,
		Role:           roleName,
	}
	token, err := services.GenerateStaffToken(staffModel)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	// Update last login (fire-and-forget)
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()
		venueDB.UpdateNoReturn(bgCtx, "organization_workers", map[string]interface{}{
			"last_login_at": time.Now().Format(time.RFC3339),
		}, map[string]interface{}{
			"id": staffID,
		})
	}()

	// Sync legacy flags from features for backwards compatibility
	venue.SyncLegacyFlags()

	firstName := services.GetString(staff, "first_name")
	lastName := services.GetString(staff, "last_name")
	fullName := firstName
	if lastName != "" {
		if fullName != "" {
			fullName += " "
		}
		fullName += lastName
	}

	// The PullMobileApp-GL expects `employee` (legacy v1 shape) with
	// venue_id_real / organization_id_real / employee_id_real fields. We
	// keep `staff` populated too so v2-aware clients keep working.
	employee := map[string]interface{}{
		"id":                   staffID,
		"email":                services.GetString(staff, "email"),
		"first_name":           firstName,
		"last_name":            lastName,
		"name":                 fullName,
		"role":                 roleName,
		"venue_id":             req.VenueID,
		"venue_id_real":        req.VenueID,
		"organization_id":      venue.OrganizationID,
		"organization_id_real": venue.OrganizationID,
		"employee_id_real":     staffID,
	}

	c.JSON(http.StatusOK, gin.H{
		"token":    token,
		"employee": employee,
		"staff":    employee,
		"venue":    venue.GetMobileAppConfig(),
	})
}

// VerifyToken verifies a staff token and returns venue info with feature flags
// GET /api/v1/auth/verify
func VerifyToken(c *gin.Context) {
	// Token already validated by middleware
	staffID := c.GetString("staff_id")
	venueID := c.GetString("venue_id")
	orgID := c.GetString("organization_id")
	email := c.GetString("email")
	name := c.GetString("name")
	role := c.GetString("role")

	if staffID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// Get venue info with feature flags (important for app flow)
	venue, err := services.DB.GetVenue(ctx, venueID)
	if err != nil || venue == nil {
		// Token is valid but venue not found - still return valid but with warning
		c.JSON(http.StatusOK, gin.H{
			"valid": true,
			"type":  "jwt",
			"claims": gin.H{
				"employee_id":     staffID,
				"venue_id":        venueID,
				"organization_id": orgID,
				"email":           email,
				"name":            name,
				"role":            role,
			},
			"staff": gin.H{
				"id":       staffID,
				"venue_id": venueID,
				"role":     role,
			},
		})
		return
	}

	// Sync legacy flags from features for backwards compatibility
	venue.SyncLegacyFlags()

	// PullMobileApp-GL's verifyToken handler reads `response.data.type === 'jwt'`
	// and pulls everything from `.claims`. We include venue_name/slug/currency
	// + use_vip_list_flow so the app can rehydrate without an extra fetch.
	c.JSON(http.StatusOK, gin.H{
		"valid": true,
		"type":  "jwt",
		"claims": gin.H{
			"employee_id":       staffID,
			"venue_id":          venueID,
			"organization_id":   orgID,
			"email":             email,
			"name":              name,
			"role":              role,
			"venue_name":        venue.Name,
			"venue_slug":        venue.Slug,
			"venue_currency":    venue.Currency,
			"use_vip_list_flow": venue.UseVipListFlow,
		},
		"staff": gin.H{
			"id":              staffID,
			"venue_id":        venueID,
			"organization_id": orgID,
			"email":           email,
			"name":            name,
			"role":            role,
		},
		// Use the new GetMobileAppConfig for complete venue config
		"venue": venue.GetMobileAppConfig(),
	})
}

// RefreshToken refreshes a staff token
// POST /api/v1/auth/refresh
func RefreshToken(c *gin.Context) {
	staffID := c.GetString("staff_id")
	venueID := c.GetString("venue_id")
	role := c.GetString("role")

	if staffID == "" || venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// Generate new token
	newToken, err := services.GenerateStaffTokenSimple(staffID, venueID, role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token": newToken,
	})
}

// =============================================
// USER AUTHENTICATION (Email code-based)
// =============================================

// RequestCodeRequest represents the code request body
type RequestCodeRequest struct {
	Email string `json:"email" binding:"required,email"`
	// Optional: the anonymous checkout WebApp doesn't know the venue_id, so we
	// fall back to the first active venue (single-venue deployment).
	VenueID string `json:"venue_id"`
}

// RequestCode sends a verification code to user's email
// POST /api/v1/user-auth/request-code
func RequestCode(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var req RequestCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))

	// Fall back to the first active venue when the WebApp didn't send one.
	if req.VenueID == "" {
		if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		}); v != nil {
			req.VenueID = services.GetString(v, "id")
		}
	}

	// Get venue database
	venueDB := services.DB.ForVenue(req.VenueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Find or create user
	user, err := venueDB.QueryOne(ctx, "public_users", map[string]interface{}{
		"select": "id,email,name",
		"where": map[string]interface{}{
			"email": email,
		},
	})

	var userID string
	if err != nil || user == nil {
		// Create new user
		newUser, err := venueDB.InsertCtx(ctx, "public_users", map[string]interface{}{
			"email":      email,
			"created_at": time.Now().Format(time.RFC3339),
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user"})
			return
		}
		userID = services.GetString(newUser, "id")
	} else {
		userID = services.GetString(user, "id")
	}

	// Generate 6-digit code
	code, err := generateVerificationCode()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate code"})
		return
	}

	// Invalidate previous codes (fire-and-forget)
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()
		venueDB.UpdateNoReturn(bgCtx, "verification_codes", map[string]interface{}{
			"used": true,
		}, map[string]interface{}{
			"user_id": userID,
			"used":    false,
		})
	}()

	// Store code (expires in 10 minutes)
	expiresAt := time.Now().Add(10 * time.Minute)
	_, err = venueDB.InsertCtx(ctx, "verification_codes", map[string]interface{}{
		"user_id":    userID,
		"code":       code,
		"expires_at": expiresAt.Format(time.RFC3339),
		"used":       false,
		"attempts":   0,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store code"})
		return
	}

	// SECURITY: Get venue name for email (do NOT log the verification code!)
	venueName := "Pull Events"
	if venue, err := services.DB.GetVenue(ctx, req.VenueID); err == nil && venue != nil {
		venueName = venue.Name
	}

	// Send verification code via email (background task)
	go func(toEmail, verificationCode, venue string) {
		emailCtx, emailCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer emailCancel()

		if services.Email != nil {
			if err := services.Email.SendVerificationCode(emailCtx, toEmail, verificationCode, venue); err != nil {
				// Log error but don't expose to user (email might still be delivered)
				log.Printf("[Auth] Failed to send verification email to %s: %v", toEmail, err)
			}
		}
	}(email, code, venueName)

	c.JSON(http.StatusOK, gin.H{
		"message":    "Verification code sent",
		"expires_in": 600,
	})
}

// VerifyCodeRequest represents the verify code request body
type VerifyCodeRequest struct {
	Email   string `json:"email" binding:"required,email"`
	Code    string `json:"code" binding:"required,len=6"`
	VenueID string `json:"venue_id"`
}

// VerifyCode verifies the code and returns a token
// POST /api/v1/user-auth/verify-code
// OPTIMIZED: Parallel user + verification code fetch
func VerifyCode(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var req VerifyCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))

	// Fall back to the first active venue when the WebApp didn't send one.
	if req.VenueID == "" {
		if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		}); v != nil {
			req.VenueID = services.GetString(v, "id")
		}
	}

	// Get venue database
	venueDB := services.DB.ForVenue(req.VenueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Find user first (needed for verification lookup)
	user, err := venueDB.QueryOne(ctx, "public_users", map[string]interface{}{
		"select": "id,email,name,surname",
		"where": map[string]interface{}{
			"email": email,
		},
	})
	if err != nil || user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid email"})
		return
	}

	userID := services.GetString(user, "id")

	// Find valid verification code
	verification, err := venueDB.QueryOne(ctx, "verification_codes", map[string]interface{}{
		"select": "id,code,expires_at,attempts",
		"where": map[string]interface{}{
			"user_id": userID,
			"used":    false,
		},
		"order": "created_at.desc",
		"limit": 1,
	})

	if err != nil || verification == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "No valid code found"})
		return
	}

	// Check attempts
	attempts := services.GetInt(verification, "attempts")
	if attempts >= 5 {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many attempts. Request a new code."})
		return
	}

	verificationID := services.GetString(verification, "id")

	// Increment attempts (fire-and-forget)
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()
		venueDB.UpdateNoReturn(bgCtx, "verification_codes", map[string]interface{}{
			"attempts": attempts + 1,
		}, map[string]interface{}{
			"id": verificationID,
		})
	}()

	// Check expiration
	expiresAt := services.GetTime(verification, "expires_at")
	if expiresAt == nil || time.Now().After(*expiresAt) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Code expired"})
		return
	}

	// Verify code
	storedCode := services.GetString(verification, "code")
	if storedCode != req.Code {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid code"})
		return
	}

	// Mark code as used (fire-and-forget)
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()
		venueDB.UpdateNoReturn(bgCtx, "verification_codes", map[string]interface{}{
			"used":    true,
			"used_at": time.Now().Format(time.RFC3339),
		}, map[string]interface{}{
			"id": verificationID,
		})
	}()

	// Generate user token
	token, err := services.GenerateUserTokenSimple(userID, email)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	// La web NO guarda el token (política anti-XSS): espera una cookie
	// HttpOnly y comprueba `success` en el body. El middleware ya lee la
	// cookie "auth_token" como fallback (extractToken).
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("auth_token", token, 7*24*3600, "/", "", true, true)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"token":   token,
		"user": gin.H{
			"id":      userID,
			"email":   email,
			"name":    services.GetString(user, "name"),
			"surname": services.GetString(user, "surname"),
		},
	})
}

// UserLogout clears the HttpOnly auth cookie.
// POST /api/v1/user-auth/logout
func UserLogout(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("auth_token", "", -1, "/", "", true, true)
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// GetUserProfile returns the user's profile
// GET /api/v1/user-auth/profile
func GetUserProfile(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// venue_id opcional: la web (single-venue) no lo envía.
	venueID := c.Query("venue_id")
	if venueID == "" {
		if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		}); v != nil {
			venueID = services.GetString(v, "id")
		}
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Get user
	user, err := venueDB.QueryOne(ctx, "public_users", map[string]interface{}{
		"select": "id,email,name,surname,phone,phone_prefix,birth_date,gender,profile_image,tier,total_spent,average_spend",
		"where": map[string]interface{}{
			"id": userID,
		},
	})
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// La web exige `success` en el body (refreshProfile del AuthContext lo
	// comprueba al montar la app — sin él, descarta la sesión válida y
	// rebota al login).
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"user":    user,
	})
}

// UpdateProfileRequest represents the update profile request
type UpdateProfileRequest struct {
	Name        string `json:"name"`
	Surname     string `json:"surname"`
	Phone       string `json:"phone"`
	PhonePrefix string `json:"phone_prefix"`
	BirthDate   string `json:"birth_date"`
	Gender      string `json:"gender"`
}

// UpdateUserProfile updates the user's profile
// PUT /api/v1/user-auth/profile
func UpdateUserProfile(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	var req UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	// venue_id opcional: la web (single-venue) no lo envía.
	venueID := c.Query("venue_id")
	if venueID == "" {
		if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		}); v != nil {
			venueID = services.GetString(v, "id")
		}
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Build update data
	updateData := make(map[string]interface{})
	if req.Name != "" {
		updateData["name"] = req.Name
	}
	if req.Surname != "" {
		updateData["surname"] = req.Surname
	}
	if req.Phone != "" {
		updateData["phone"] = req.Phone
	}
	if req.PhonePrefix != "" {
		updateData["phone_prefix"] = req.PhonePrefix
	}
	if req.BirthDate != "" {
		updateData["birth_date"] = req.BirthDate
	}
	if req.Gender != "" {
		updateData["gender"] = req.Gender
	}

	if len(updateData) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	updateData["updated_at"] = time.Now().Format(time.RFC3339)

	// Update user
	result, err := venueDB.UpdateCtx(ctx, "public_users", updateData, map[string]interface{}{
		"id": userID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update profile"})
		return
	}

	if len(result) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Profile updated successfully",
		"user":    result[0],
	})
}

// =============================================
// PLATFORM AUTHENTICATION
// =============================================

// PlatformLoginRequest represents platform login request
type PlatformLoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=6"`
}

// PlatformLogin handles platform staff login
// POST /api/v1/platform/login
func PlatformLogin(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var req PlatformLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	// Query central database
	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Central database not available"})
		return
	}

	// Find platform staff
	staff, err := central.QueryOne(ctx, "pull_staff", map[string]interface{}{
		"select": "id,email,password_hash,name,role,is_active,failed_login_attempts,locked_until",
		"where": map[string]interface{}{
			"email": strings.ToLower(req.Email),
		},
	})

	if err != nil || staff == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
		return
	}

	// Check if active
	if !services.GetBool(staff, "is_active") {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Account is disabled"})
		return
	}

	// Check if locked
	lockedUntil := services.GetTime(staff, "locked_until")
	if lockedUntil != nil && time.Now().Before(*lockedUntil) {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":        "Account is temporarily locked",
			"locked_until": lockedUntil.Format(time.RFC3339),
		})
		return
	}

	staffID := services.GetString(staff, "id")

	// Verify password
	passwordHash := services.GetString(staff, "password_hash")
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
		// Increment failed attempts (fire-and-forget)
		go func() {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer bgCancel()

			attempts := services.GetInt(staff, "failed_login_attempts") + 1
			updateData := map[string]interface{}{
				"failed_login_attempts": attempts,
			}

			// Lock after 5 failed attempts
			if attempts >= 5 {
				updateData["locked_until"] = time.Now().Add(15 * time.Minute).Format(time.RFC3339)
			}

			central.UpdateNoReturn(bgCtx, "pull_staff", updateData, map[string]interface{}{"id": staffID})
		}()

		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
		return
	}

	// Generate token
	role := services.GetString(staff, "role")
	token, err := services.GeneratePlatformTokenSimple(staffID, role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	// Reset failed attempts and update last login (fire-and-forget)
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()

		central.UpdateNoReturn(bgCtx, "pull_staff", map[string]interface{}{
			"failed_login_attempts": 0,
			"locked_until":          nil,
			"last_login_at":         time.Now().Format(time.RFC3339),
			"last_login_ip":         c.ClientIP(),
		}, map[string]interface{}{"id": staffID})

		// Log audit
		central.InsertCtx(bgCtx, "pull_staff_audit_log", map[string]interface{}{
			"staff_id":   staffID,
			"action":     "login",
			"ip_address": c.ClientIP(),
			"user_agent": c.Request.UserAgent(),
		})
	}()

	c.JSON(http.StatusOK, gin.H{
		"token": token,
		"staff": gin.H{
			"id":    staffID,
			"email": services.GetString(staff, "email"),
			"name":  services.GetString(staff, "name"),
			"role":  role,
		},
	})
}

// =============================================
// HELPERS
// =============================================

// generateVerificationCode generates a random 6-digit code
func generateVerificationCode() (string, error) {
	max := big.NewInt(900000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	code := n.Int64() + 100000
	return fmt.Sprintf("%06d", code), nil
}

// HashPassword hashes a password using bcrypt
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

// CheckPassword checks if a password matches a hash
func CheckPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}
