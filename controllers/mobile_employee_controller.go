package controllers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// =============================================
// MOBILE EMPLOYEE CRUD
// The app's EmpleadoNuevo/EmpleadoEditar screens hit these paths:
//   POST   /employees/create
//   PUT    /employees/:employeeId
//   DELETE /employees/:employeeId
// The create form only collects first/last name, email and DPI — it does
// NOT send a password or role, and expects a generated password back. These
// handlers match that shape (the v2 /employees CRUD requires both).
// =============================================

func genTempPassword() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	// URL-safe, no padding — readable to dictate to staff.
	return "Pull" + base64.RawURLEncoding.EncodeToString(b)
}

// roleIDByName resolves a role name to its id in the venue DB.
func roleIDByName(ctx context.Context, venueDB *services.SupabaseClient, name string) string {
	r, _ := venueDB.QueryOne(ctx, "roles", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"name": name},
	})
	return services.GetString(r, "id")
}

// MobileCreateEmployee creates a staff member with an auto-generated
// password. Body: {firstName, lastName, email, dpi, role?}. role defaults to
// "doorman" (the day-of-event use case); pass "staff"/"manager"/etc to override.
func MobileCreateEmployee(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}
	if role := c.GetString("role"); role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Solo un admin o manager puede crear empleados"})
		return
	}

	var req struct {
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
		Email     string `json:"email"`
		DPI       string `json:"dpi"`
		Role      string `json:"role"`
		Password  string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Datos inválidos"})
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.FirstName == "" || req.LastName == "" || req.Email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nombre, apellido y email son obligatorios"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	// Reject duplicate email.
	if existing, _ := venueDB.QueryOne(ctx, "organization_workers", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"email": req.Email, "deleted_at": "is.null"},
	}); existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Ya existe un empleado con ese email"})
		return
	}

	roleName := req.Role
	if roleName == "" {
		roleName = "doorman"
	}
	roleID := roleIDByName(ctx, venueDB, roleName)
	if roleID == "" {
		roleID = roleIDByName(ctx, venueDB, "staff")
	}
	if roleID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No se encontró el rol solicitado"})
		return
	}

	password := req.Password
	if password == "" {
		password = genTempPassword()
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "No se pudo procesar la contraseña"})
		return
	}

	insert := map[string]interface{}{
		"email":         req.Email,
		"first_name":    req.FirstName,
		"last_name":     req.LastName,
		"password_hash": string(hash),
		"role_id":       roleID,
		"is_active":     true,
	}
	if req.DPI != "" {
		insert["dpi"] = req.DPI
	}
	employee, err := venueDB.InsertCtx(ctx, "organization_workers", insert)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "No se pudo crear el empleado", "details": err.Error()})
		return
	}
	delete(employee, "password_hash")
	employee["role"] = roleName

	c.JSON(http.StatusCreated, gin.H{
		"message":           "Empleado creado",
		"data":              employee,
		"generatedPassword": password,
	})
}
