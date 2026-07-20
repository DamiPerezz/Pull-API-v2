package controllers

import (
	"context"
	"net/http"
	"pull-api-v2/services"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// =============================================
// EMPLOYEE MANAGEMENT
// =============================================

// GetEmployees returns all employees for a venue
// GET /api/v1/employees
// OPTIMIZED: Parallel employees + roles fetch
func GetEmployees(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	role := c.GetString("role")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// Only admin/manager can view employees
	if role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	// Parse filters
	roleFilter := c.Query("role")
	activeFilter := c.Query("active")

	whereClause := map[string]interface{}{
		"deleted_at": "is.null",
	}

	if activeFilter == "true" {
		whereClause["is_active"] = true
	} else if activeFilter == "false" {
		whereClause["is_active"] = false
	}

	// OPTIMIZATION: Parallel fetch employees and roles
	var employees []map[string]interface{}
	var roles []map[string]interface{}
	var empErr error
	var wg sync.WaitGroup

	wg.Add(2)

	// Fetch employees
	go func() {
		defer wg.Done()
		employees, empErr = venueDB.QueryCtx(ctx, "organization_workers", map[string]interface{}{
			"select": "id,email,first_name,last_name,phone,dpi,role_id,is_active,profile_image,last_login_at,created_at",
			"where":  whereClause,
			"order":  "created_at.desc",
		})
	}()

	// Fetch all roles
	go func() {
		defer wg.Done()
		roles, _ = venueDB.QueryCtx(ctx, "roles", map[string]interface{}{
			"select": "id,name",
		})
	}()

	wg.Wait()

	if empErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch employees"})
		return
	}

	// Build role map for O(1) lookup
	roleMap := services.BuildIDNameMap(roles, "id", "name")

	// Filter by role if specified and add role name
	var filteredEmployees []map[string]interface{}
	if roleFilter != "" {
		// Find role ID by name
		var filterRoleID string
		for _, r := range roles {
			if services.GetString(r, "name") == roleFilter {
				filterRoleID = services.GetString(r, "id")
				break
			}
		}

		// Filter employees by role
		filteredEmployees = make([]map[string]interface{}, 0, len(employees))
		for _, emp := range employees {
			roleID := services.GetString(emp, "role_id")
			if roleID == filterRoleID {
				emp["role_name"] = roleMap[roleID]
				filteredEmployees = append(filteredEmployees, emp)
			}
		}
	} else {
		// No filter, add role names to all
		filteredEmployees = employees
		for _, emp := range filteredEmployees {
			roleID := services.GetString(emp, "role_id")
			emp["role_name"] = roleMap[roleID]
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"employees": filteredEmployees,
		"total":     len(filteredEmployees),
	})
}

// CreateEmployeeRequest represents the create employee request
type CreateEmployeeRequest struct {
	Email     string `json:"email" binding:"required,email"`
	FirstName string `json:"first_name" binding:"required"`
	LastName  string `json:"last_name" binding:"required"`
	Phone     string `json:"phone"`
	DPI       string `json:"dpi" binding:"required"`
	Password  string `json:"password" binding:"required,min=6"`
	RoleID    string `json:"role_id" binding:"required,uuid"`
}

// CreateEmployee creates a new employee
// POST /api/v1/employees
// OPTIMIZED: Parallel validation checks
func CreateEmployee(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	role := c.GetString("role")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// Only admin can create employees
	if role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only admin can create employees"})
		return
	}

	var req CreateEmployeeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))

	// OPTIMIZATION: Parallel validation - email, DPI, and role
	var existingEmail, existingDPI, roleRecord map[string]interface{}
	var wg sync.WaitGroup
	var mu sync.Mutex

	wg.Add(3)

	// Check email
	go func() {
		defer wg.Done()
		result, _ := venueDB.QueryOne(ctx, "organization_workers", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"email":      email,
				"deleted_at": "is.null",
			},
		})
		mu.Lock()
		existingEmail = result
		mu.Unlock()
	}()

	// Check DPI
	go func() {
		defer wg.Done()
		result, _ := venueDB.QueryOne(ctx, "organization_workers", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"dpi":        req.DPI,
				"deleted_at": "is.null",
			},
		})
		mu.Lock()
		existingDPI = result
		mu.Unlock()
	}()

	// Verify role
	go func() {
		defer wg.Done()
		result, _ := venueDB.QueryOne(ctx, "roles", map[string]interface{}{
			"select": "id,name",
			"where":  map[string]interface{}{"id": req.RoleID},
		})
		mu.Lock()
		roleRecord = result
		mu.Unlock()
	}()

	wg.Wait()

	// Check validation results
	if existingEmail != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Email already registered"})
		return
	}
	if existingDPI != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "DPI already registered"})
		return
	}
	if roleRecord == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid role ID"})
		return
	}

	// Hash password
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process password"})
		return
	}

	// Create employee
	employee, err := venueDB.InsertCtx(ctx, "organization_workers", map[string]interface{}{
		"email":         email,
		"first_name":    req.FirstName,
		"last_name":     req.LastName,
		"phone":         req.Phone,
		"dpi":           req.DPI,
		"password_hash": string(passwordHash),
		"role_id":       req.RoleID,
		"is_active":     true,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to create employee",
			"details": err.Error(),
		})
		return
	}

	// Remove password hash from response
	delete(employee, "password_hash")
	employee["role_name"] = services.GetString(roleRecord, "name")

	c.JSON(http.StatusCreated, gin.H{
		"message":  "Employee created successfully",
		"employee": employee,
	})
}

// UpdateEmployeeRequest represents the update employee request. The mobile
// app (EmpleadoEditar) sends camelCase names, so we accept both.
type UpdateEmployeeRequest struct {
	Email        string `json:"email"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	FirstNameAlt string `json:"firstName"`
	LastNameAlt  string `json:"lastName"`
	Phone        string `json:"phone"`
	DPI          string `json:"dpi"`
	RoleID       string `json:"role_id"`
	RoleName     string `json:"role"`
	IsActive     *bool  `json:"is_active"`
	Password     string `json:"password"`
}

// UpdateEmployee updates an employee
// PUT /api/v1/employees/:id
// OPTIMIZED: Parallel validation checks
func UpdateEmployee(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	role := c.GetString("role")
	employeeID := c.Param("id")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// Admin or manager can update employees
	if role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only admin or manager can update employees"})
		return
	}

	var req UpdateEmployeeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	// Coalesce camelCase (mobile) into the snake_case fields.
	if req.FirstName == "" {
		req.FirstName = req.FirstNameAlt
	}
	if req.LastName == "" {
		req.LastName = req.LastNameAlt
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	// Resolve role by name (mobile sends "role", not role_id).
	if req.RoleID == "" && req.RoleName != "" {
		if r, _ := venueDB.QueryOne(ctx, "roles", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"name": req.RoleName},
		}); r != nil {
			req.RoleID = services.GetString(r, "id")
		}
	}

	// Check employee exists
	existing, err := venueDB.QueryOne(ctx, "organization_workers", map[string]interface{}{
		"select": "id",
		"where": map[string]interface{}{
			"id":         employeeID,
			"deleted_at": "is.null",
		},
	})
	if err != nil || existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Employee not found"})
		return
	}

	// Build update data and track what needs validation
	updateData := make(map[string]interface{})
	needsEmailCheck := req.Email != ""
	needsDPICheck := req.DPI != ""
	needsRoleCheck := req.RoleID != ""

	var email string
	if needsEmailCheck {
		email = strings.ToLower(strings.TrimSpace(req.Email))
	}

	// OPTIMIZATION: Parallel validation for email, DPI, and role
	var emailCheck, dpiCheck, roleRecord map[string]interface{}
	var wg sync.WaitGroup
	var mu sync.Mutex

	if needsEmailCheck {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, _ := venueDB.QueryOne(ctx, "organization_workers", map[string]interface{}{
				"select": "id",
				"where": map[string]interface{}{
					"email":      email,
					"id":         "neq." + employeeID,
					"deleted_at": "is.null",
				},
			})
			mu.Lock()
			emailCheck = result
			mu.Unlock()
		}()
	}

	if needsDPICheck {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, _ := venueDB.QueryOne(ctx, "organization_workers", map[string]interface{}{
				"select": "id",
				"where": map[string]interface{}{
					"dpi":        req.DPI,
					"id":         "neq." + employeeID,
					"deleted_at": "is.null",
				},
			})
			mu.Lock()
			dpiCheck = result
			mu.Unlock()
		}()
	}

	if needsRoleCheck {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, _ := venueDB.QueryOne(ctx, "roles", map[string]interface{}{
				"select": "id",
				"where":  map[string]interface{}{"id": req.RoleID},
			})
			mu.Lock()
			roleRecord = result
			mu.Unlock()
		}()
	}

	wg.Wait()

	// Check validation results
	if needsEmailCheck {
		if emailCheck != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Email already registered"})
			return
		}
		updateData["email"] = email
	}

	if needsDPICheck {
		if dpiCheck != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "DPI already registered"})
			return
		}
		updateData["dpi"] = req.DPI
	}

	if needsRoleCheck {
		if roleRecord == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid role ID"})
			return
		}
		updateData["role_id"] = req.RoleID
	}

	// Add other fields
	if req.FirstName != "" {
		updateData["first_name"] = req.FirstName
	}
	if req.LastName != "" {
		updateData["last_name"] = req.LastName
	}
	if req.Phone != "" {
		updateData["phone"] = req.Phone
	}
	if req.IsActive != nil {
		updateData["is_active"] = *req.IsActive
	}
	if req.Password != "" {
		if len(req.Password) < 6 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Password must be at least 6 characters"})
			return
		}
		passwordHash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process password"})
			return
		}
		updateData["password_hash"] = string(passwordHash)
	}

	if len(updateData) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	updateData["updated_at"] = time.Now().Format(time.RFC3339)

	// Update employee
	result, err := venueDB.UpdateCtx(ctx, "organization_workers", updateData, map[string]interface{}{
		"id": employeeID,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to update employee",
			"details": err.Error(),
		})
		return
	}

	if len(result) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Employee not found"})
		return
	}

	// Remove password hash from response
	delete(result[0], "password_hash")

	c.JSON(http.StatusOK, gin.H{
		"message":  "Employee updated successfully",
		"employee": result[0],
	})
}

// DeleteEmployee soft-deletes an employee
// DELETE /api/v1/employees/:id
func DeleteEmployee(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	role := c.GetString("role")
	staffID := c.GetString("staff_id")
	employeeID := c.Param("id")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// Admin or manager can delete employees
	if role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only admin or manager can delete employees"})
		return
	}

	// Cannot delete yourself
	if staffID == employeeID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot delete your own account"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	// Check employee exists
	existing, err := venueDB.QueryOne(ctx, "organization_workers", map[string]interface{}{
		"select": "id",
		"where": map[string]interface{}{
			"id":         employeeID,
			"deleted_at": "is.null",
		},
	})
	if err != nil || existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Employee not found"})
		return
	}

	// Soft delete
	_, err = venueDB.UpdateCtx(ctx, "organization_workers", map[string]interface{}{
		"deleted_at": time.Now().Format(time.RFC3339),
		"is_active":  false,
	}, map[string]interface{}{
		"id": employeeID,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to delete employee",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Employee deleted successfully",
	})
}

// =============================================
// ROLES
// =============================================

// GetRoles returns all roles
// GET /api/v1/roles
func GetRoles(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	roles, err := venueDB.QueryCtx(ctx, "roles", map[string]interface{}{
		"select": "id,name,permissions,created_at",
		"order":  "name.asc",
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch roles"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"roles": roles,
	})
}
