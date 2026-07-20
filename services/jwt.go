package services

import (
	"fmt"
	"pull-api-v2/config"
	"pull-api-v2/models"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// =============================================
// JWT SERVICE
// Handles token generation and validation
// =============================================

const (
	// 72h so a doorman who logged in the day before an event doesn't get
	// logged out mid-queue (24h expiry + logout-on-401 bit us in audit A1).
	StaffTokenExpiry    = 72 * time.Hour
	UserTokenExpiry     = 7 * 24 * time.Hour
	PlatformTokenExpiry = 8 * time.Hour
)

// JWTClaims represents the JWT claims structure
type JWTClaims struct {
	jwt.RegisteredClaims
	UserID         string `json:"user_id,omitempty"`
	EmployeeID     string `json:"employee_id,omitempty"` // alias of user_id for mobile compat
	Email          string `json:"email,omitempty"`
	Name           string `json:"name,omitempty"`
	VenueID        string `json:"venue_id,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	Role           string `json:"role,omitempty"`
	Type           string `json:"type"` // venue_staff, user, platform_staff
}

// GenerateStaffToken generates a JWT for venue staff
func GenerateStaffToken(staff *models.Staff) (string, error) {
	claims := JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(StaffTokenExpiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "pull-api-v2",
		},
		UserID:         staff.ID,
		EmployeeID:     staff.ID,
		Email:          staff.Email,
		Name:           staff.Name,
		VenueID:        staff.VenueID,
		OrganizationID: staff.OrganizationID,
		Role:           staff.Role,
		Type:           "venue_staff",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(config.App.JWTSecret))
}

// GenerateUserToken generates a JWT for public users
func GenerateUserToken(user *models.User) (string, error) {
	claims := JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(UserTokenExpiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "pull-api-v2",
		},
		UserID: user.ID,
		Email:  user.Email,
		Name:   user.Name,
		Type:   "user",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(config.App.JWTSecret))
}

// GeneratePlatformToken generates a JWT for platform staff
func GeneratePlatformToken(staff *models.PlatformStaff) (string, error) {
	claims := JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(PlatformTokenExpiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "pull-api-v2",
		},
		UserID: staff.ID,
		Email:  staff.Email,
		Name:   staff.Name,
		Role:   staff.Role,
		Type:   "platform_staff",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(config.App.JWTSecret))
}

// ValidateToken validates a JWT and returns the claims
func ValidateToken(tokenString string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(config.App.JWTSecret), nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*JWTClaims); ok && token.Valid {
		return claims, nil
	}

	return nil, fmt.Errorf("invalid token")
}

// RefreshToken generates a new token from existing claims
func RefreshToken(claims *JWTClaims) (string, error) {
	var expiry time.Duration
	switch claims.Type {
	case "venue_staff":
		expiry = StaffTokenExpiry
	case "user":
		expiry = UserTokenExpiry
	case "platform_staff":
		expiry = PlatformTokenExpiry
	default:
		expiry = StaffTokenExpiry
	}

	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(expiry))
	claims.IssuedAt = jwt.NewNumericDate(time.Now())

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(config.App.JWTSecret))
}

// =============================================
// CONVENIENCE FUNCTIONS (for controllers)
// =============================================

// GenerateStaffTokenSimple generates a JWT for venue staff with individual params
func GenerateStaffTokenSimple(staffID, venueID, role string) (string, error) {
	staff := &models.Staff{
		ID:      staffID,
		VenueID: venueID,
		Role:    role,
	}
	return GenerateStaffToken(staff)
}

// GenerateUserTokenSimple generates a JWT for public users with individual params
func GenerateUserTokenSimple(userID, email string) (string, error) {
	user := &models.User{
		ID:    userID,
		Email: email,
	}
	return GenerateUserToken(user)
}

// GeneratePlatformTokenSimple generates a JWT for platform staff with individual params
func GeneratePlatformTokenSimple(staffID, role string) (string, error) {
	staff := &models.PlatformStaff{
		ID:   staffID,
		Role: role,
	}
	return GeneratePlatformToken(staff)
}
