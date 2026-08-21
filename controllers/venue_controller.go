package controllers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"pull-api-v2/services"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// VenueImagesBucket is the bucket name for venue images
const VenueImagesBucketName = "venue-images"

// Signed URL expiration for venue images (1 hour)
const VenueImageURLExpiration = 1 * time.Hour

// Cache TTLs for public endpoints
const (
	venueListCacheTTL   = 5 * time.Minute
	venueSingleCacheTTL = 5 * time.Minute
	venueEventsCacheTTL = 2 * time.Minute
)

// =============================================
// IMAGE URL HELPERS
// =============================================

// enrichVenueWithSignedImageURL converts a venue's image path to a signed URL
// This allows the frontend to display images from private storage buckets
func enrichVenueWithSignedImageURL(ctx context.Context, venue map[string]interface{}, storage *services.StorageService) {
	imagePath := services.GetString(venue, "image")
	if imagePath == "" {
		return
	}

	// Skip if already a full URL (starts with http)
	if strings.HasPrefix(imagePath, "http") {
		return
	}

	// Generate signed URL for the image
	signedURL, err := storage.GenerateSignedURL(ctx, VenueImagesBucketName, imagePath, VenueImageURLExpiration)
	if err != nil {
		log.Printf("[enrichVenueWithSignedImageURL] Failed to sign image %s: %v", imagePath, err)
		// Keep the original path on error - frontend will handle it
		return
	}

	venue["image"] = signedURL
	venue["image_path"] = imagePath // Keep original path for reference
}

// enrichVenuesWithSignedImageURLs processes multiple venues in parallel
func enrichVenuesWithSignedImageURLs(ctx context.Context, venues []map[string]interface{}) {
	if len(venues) == 0 {
		return
	}

	// Get central storage service
	storage, err := services.GetCentralStorage()
	if err != nil {
		log.Printf("[enrichVenuesWithSignedImageURLs] Storage not available: %v", err)
		return
	}

	// Process venues in parallel with bounded concurrency
	sem := make(chan struct{}, 10) // Max 10 concurrent operations
	var wg sync.WaitGroup

	for i := range venues {
		wg.Add(1)
		go func(venue map[string]interface{}) {
			defer wg.Done()
			sem <- struct{}{}        // Acquire
			defer func() { <-sem }() // Release

			enrichVenueWithSignedImageURL(ctx, venue, storage)
		}(venues[i])
	}

	wg.Wait()
}

// =============================================
// PUBLIC VENUE ENDPOINTS
// =============================================

// GetVenues returns a list of active venues
// GET /api/v1/venues
// OPTIMIZED: Response caching
func GetVenues(c *gin.Context) {
	// Check cache first
	cacheKey := "public:venues:list"
	if cached, ok := services.AppCache.Get(cacheKey); ok {
		if venues, ok := cached.([]map[string]interface{}); ok {
			c.JSON(http.StatusOK, gin.H{
				"venues": venues,
				"count":  len(venues),
				"cached": true,
			})
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Query venues from central database
	venues, err := central.QueryCtx(ctx, "venues", map[string]interface{}{
		"select": "id,name,slug,description,image,location,city,country,currency,primary_payment_gateway",
		"where": map[string]interface{}{
			"is_active":  true,
			"deleted_at": "is.null",
		},
		"order": "name.asc",
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get venues"})
		return
	}

	// Enrich venues with signed image URLs
	enrichVenuesWithSignedImageURLs(ctx, venues)

	// Cache the result (with signed URLs)
	services.AppCache.Set(cacheKey, venues, venueListCacheTTL)

	c.JSON(http.StatusOK, gin.H{
		"venues": venues,
		"count":  len(venues),
	})
}

// GetVenue returns a venue by slug
// GET /api/v1/venues/:slug
// OPTIMIZED: Response caching
func GetVenue(c *gin.Context) {
	slug := c.Param("slug")
	if slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Venue slug is required"})
		return
	}

	// Check cache first
	cacheKey := fmt.Sprintf("public:venue:%s", slug)
	if cached, ok := services.AppCache.Get(cacheKey); ok {
		if venue, ok := cached.(map[string]interface{}); ok {
			c.JSON(http.StatusOK, gin.H{
				"venue":  venue,
				"cached": true,
			})
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Query venue from central database
	venue, err := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "*",
		"where": map[string]interface{}{
			"slug":       slug,
			"is_active":  true,
			"deleted_at": "is.null",
		},
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get venue"})
		return
	}

	if venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}

	// Enrich venue with signed image URL
	storage, err := services.GetCentralStorage()
	if err == nil {
		enrichVenueWithSignedImageURL(ctx, venue, storage)
	}

	// Cache the result (with signed URL)
	services.AppCache.Set(cacheKey, venue, venueSingleCacheTTL)

	c.JSON(http.StatusOK, gin.H{
		"venue": venue,
	})
}

// GetVenueEvents returns events for a venue by slug
// GET /api/v1/venues/:slug/events
// OPTIMIZED: Response caching + parallel queries
func GetVenueEvents(c *gin.Context) {
	slug := c.Param("slug")
	if slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Venue slug is required"})
		return
	}

	// Parse filters for cache key
	dateFilter := c.Query("date")
	upcoming := c.Query("upcoming")

	// Build cache key with filters
	cacheKey := fmt.Sprintf("public:venue:%s:events:date=%s:upcoming=%s", slug, dateFilter, upcoming)
	if cached, ok := services.AppCache.Get(cacheKey); ok {
		if data, ok := cached.(map[string]interface{}); ok {
			data["cached"] = true
			c.JSON(http.StatusOK, data)
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// First, get venue ID from central
	venue, err := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id",
		"where": map[string]interface{}{
			"slug":       slug,
			"is_active":  true,
			"deleted_at": "is.null",
		},
	})

	if err != nil || venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}

	venueID := services.GetString(venue, "id")

	// Get venue database
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	whereClause := map[string]interface{}{
		"status":     services.PublishedEventStatuses,
		"deleted_at": "is.null",
	}

	if dateFilter != "" {
		whereClause["start_datetime"] = "gte." + dateFilter + "T00:00:00Z"
	} else if upcoming == "true" {
		whereClause["start_datetime"] = "gte." + time.Now().Format(time.RFC3339)
	}

	// Query events from venue database
	events, err := venueDB.QueryCtx(ctx, "events", map[string]interface{}{
		"select": services.EventSelectColumns,
		"where":  whereClause,
		"order":  "start_datetime.asc",
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get events"})
		return
	}

	services.EnrichEvents(events)

	result := map[string]interface{}{
		"events":   events,
		"count":    len(events),
		"venue_id": venueID,
	}

	// Cache the result
	services.AppCache.Set(cacheKey, result, venueEventsCacheTTL)

	c.JSON(http.StatusOK, result)
}

// =============================================
// STAFF VENUE ENDPOINTS (Authenticated)
// =============================================

// GetVenueInfo returns detailed venue info for staff
// GET /api/v1/venue/info
// OPTIMIZED: Parallel stats queries
func GetVenueInfo(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// Get venue from central
	venue, err := services.DB.GetVenue(ctx, venueID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}

	// Get additional stats from venue DB
	venueDB := services.DB.ForVenue(venueID)
	if venueDB != nil {
		today := time.Now().Format("2006-01-02")

		// OPTIMIZATION: Parallel stats queries
		var dailyStats map[string]interface{}
		var upcomingEvents []map[string]interface{}
		var wg sync.WaitGroup

		wg.Add(2)

		// Get today's stats
		go func() {
			defer wg.Done()
			dailyStats, _ = venueDB.QueryOne(ctx, "daily_revenue_summary", map[string]interface{}{
				"select": "total_transactions,total_gross_amount,total_net_to_venue,tickets_sold",
				"where": map[string]interface{}{
					"date": today,
				},
			})
		}()

		// Get upcoming events count
		go func() {
			defer wg.Done()
			upcomingEvents, _ = venueDB.QueryCtx(ctx, "events", map[string]interface{}{
				"select": "id",
				"where": map[string]interface{}{
					"event_date":   "gte." + today,
					"is_active":    true,
					"is_published": true,
					"deleted_at":   "is.null",
				},
			})
		}()

		wg.Wait()

		c.JSON(http.StatusOK, gin.H{
			"venue": map[string]interface{}{
				"id":                   venue.ID,
				"name":                 venue.Name,
				"slug":                 venue.Slug,
				"description":          venue.Description,
				"image":                venue.Image,
				"location":             venue.Location,
				"country":              venue.Country,
				"currency":             venue.Currency,
				"timezone":             venue.Timezone,
				"contact_email":        venue.ContactEmail,
				"contact_phone":        venue.ContactPhone,
				"platform_fee_percent": venue.PlatformFeePercent,
				"platform_fee_fixed":   venue.PlatformFeeFixed,
				"use_vip_list_flow":    venue.UseVipListFlow,
				"use_guest_list_flow":  venue.UseGuestListFlow,
				"is_active":            venue.IsActive,
			},
			"stats": map[string]interface{}{
				"today":           dailyStats,
				"upcoming_events": len(upcomingEvents),
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"venue": venue,
	})
}

// UpdateVenueRequest represents the update venue request
type UpdateVenueRequest struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Image        string `json:"image"`
	Location     string `json:"location"`
	ContactEmail string `json:"contact_email"`
	ContactPhone string `json:"contact_phone"`
}

// UpdateVenue updates venue info (staff only)
// PUT /api/v1/venue/update
func UpdateVenue(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	role := c.GetString("role")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// Only admin or manager can update venue
	if role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
		return
	}

	var req UpdateVenueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Build update data
	updateData := make(map[string]interface{})
	if req.Name != "" {
		updateData["name"] = req.Name
	}
	if req.Description != "" {
		updateData["description"] = req.Description
	}
	if req.Image != "" {
		updateData["image"] = req.Image
	}
	if req.Location != "" {
		updateData["location"] = req.Location
	}
	if req.ContactEmail != "" {
		updateData["contact_email"] = req.ContactEmail
	}
	if req.ContactPhone != "" {
		updateData["contact_phone"] = req.ContactPhone
	}

	if len(updateData) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	updateData["updated_at"] = time.Now().Format(time.RFC3339)

	// Update venue in central
	result, err := central.UpdateCtx(ctx, "venues", updateData, map[string]interface{}{
		"id": venueID,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update venue"})
		return
	}

	if len(result) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}

	// Invalidate venue cache
	if slug := services.GetString(result[0], "slug"); slug != "" {
		services.AppCache.Delete(fmt.Sprintf("public:venue:%s", slug))
	}
	services.AppCache.Delete("public:venues:list")

	c.JSON(http.StatusOK, gin.H{
		"message": "Venue updated successfully",
		"venue":   result[0],
	})
}

// =============================================
// PLATFORM VENUE ENDPOINTS (Admin only)
// =============================================

// GetAllVenues returns all venues for platform admin
// GET /api/v1/admin/venues
func GetAllVenues(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Parse query params
	status := c.Query("status")
	plan := c.Query("plan")
	limit := c.DefaultQuery("limit", "50")
	offset := c.DefaultQuery("offset", "0")

	whereClause := map[string]interface{}{
		"deleted_at": "is.null",
	}

	if status == "active" {
		whereClause["is_active"] = true
	} else if status == "inactive" {
		whereClause["is_active"] = false
	}

	// Note: subscription_plan filter disabled - column doesn't exist in central DB
	_ = plan

	// Query venues - columns from central DB schema
	venues, err := central.QueryCtx(ctx, "venues", map[string]interface{}{
		"select": "*",
		"where":  whereClause,
		"order":  "created_at.desc",
		"limit":  limit,
		"offset": offset,
	})

	if err != nil {
		log.Printf("[GetAllVenues] Error querying venues: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get venues", "details": err.Error()})
		return
	}

	// Enrich venues with signed image URLs
	enrichVenuesWithSignedImageURLs(ctx, venues)

	c.JSON(http.StatusOK, gin.H{
		"venues": venues,
		"count":  len(venues),
	})
}

// GetVenueById returns a single venue by ID for platform admin
// GET /api/v1/admin/venues/:id
func GetVenueById(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.Param("id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Venue ID is required"})
		return
	}

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Query venue with all fields
	venue, err := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "*",
		"where": map[string]interface{}{
			"id":         venueID,
			"deleted_at": "is.null",
		},
	})

	if err != nil {
		log.Printf("[GetVenueById] Error querying venue: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get venue", "details": err.Error()})
		return
	}

	if venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}

	// Query database config for this venue
	dbConfig, err := central.QueryOne(ctx, "venue_database_configs", map[string]interface{}{
		"select": "id,venue_id,supabase_url,supabase_service_key_encrypted,supabase_anon_key,is_active",
		"where": map[string]interface{}{
			"venue_id": venueID,
		},
	})

	if err != nil {
		log.Printf("[GetVenueById] Error querying database config: %v", err)
		// Don't fail, just continue without db config
	}

	// If db config exists, decrypt the service key for display
	if dbConfig != nil {
		encryptedKey := services.GetString(dbConfig, "supabase_service_key_encrypted")
		if encryptedKey != "" {
			decryptedKey, err := services.DecryptServiceKey(encryptedKey)
			if err != nil {
				log.Printf("[GetVenueById] Error decrypting service key: %v", err)
				// Show masked key on error
				dbConfig["supabase_service_key"] = "••••••••"
			} else {
				dbConfig["supabase_service_key"] = decryptedKey
			}
		}
		// Remove encrypted field from response
		delete(dbConfig, "supabase_service_key_encrypted")
		venue["database_config"] = dbConfig
	}

	// Enrich venue with signed image URL
	storage, err := services.GetCentralStorage()
	if err == nil {
		enrichVenueWithSignedImageURL(ctx, venue, storage)
	}

	c.JSON(http.StatusOK, gin.H{
		"venue": venue,
	})
}

// UpdateVenueAdmin updates a venue (platform admin only)
// PUT /api/v1/admin/venues/:id
func UpdateVenueAdmin(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Escritura: exige admin (o super_admin, comodín). Antes no miraba el rol
	// ni una vez — un "viewer" podía reescribir el venue, credenciales de BD
	// incluidas.
	if !requirePlatformRole(c, platformRoleAdmin) {
		return
	}

	venueID := c.Param("id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Venue ID is required"})
		return
	}

	var updateData map[string]interface{}
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Extract database_config if present
	var dbConfig map[string]interface{}
	if dc, ok := updateData["database_config"].(map[string]interface{}); ok {
		dbConfig = dc
		delete(updateData, "database_config")
	}

	// Remove fields that shouldn't be updated
	delete(updateData, "id")
	delete(updateData, "created_at")
	delete(updateData, "organization_id")

	// Set updated_at
	updateData["updated_at"] = time.Now().Format(time.RFC3339)

	// Update venue (only filter by ID, deleted venues shouldn't be accessible anyway)
	result, err := central.UpdateCtx(ctx, "venues", updateData, map[string]interface{}{
		"id": venueID,
	})

	if err != nil {
		log.Printf("[UpdateVenueAdmin] Error updating venue: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update venue", "details": err.Error()})
		return
	}

	if len(result) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}

	// Handle database_config update/insert
	if dbConfig != nil {
		supabaseURL := services.GetString(dbConfig, "supabase_url")
		supabaseServiceKey := services.GetString(dbConfig, "supabase_service_key")
		supabaseAnonKey := services.GetString(dbConfig, "supabase_anon_key")

		if supabaseURL != "" || supabaseServiceKey != "" || supabaseAnonKey != "" {
			// Check if config exists
			existingConfig, _ := central.QueryOne(ctx, "venue_database_configs", map[string]interface{}{
				"select": "id",
				"where": map[string]interface{}{
					"venue_id": venueID,
				},
			})

			// Build config data
			configData := map[string]interface{}{
				"updated_at": time.Now().Format(time.RFC3339),
			}

			if supabaseURL != "" {
				configData["supabase_url"] = supabaseURL
			}
			if supabaseAnonKey != "" {
				configData["supabase_anon_key"] = supabaseAnonKey
			}
			// Encrypt service key if provided
			if supabaseServiceKey != "" {
				encryptedKey, err := services.EncryptServiceKey(supabaseServiceKey)
				if err != nil {
					log.Printf("[UpdateVenueAdmin] Error encrypting service key: %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encrypt service key"})
					return
				}
				configData["supabase_service_key_encrypted"] = encryptedKey
			}

			if existingConfig != nil {
				// Update existing config
				_, err = central.UpdateCtx(ctx, "venue_database_configs", configData, map[string]interface{}{
					"venue_id": venueID,
				})
			} else {
				// Insert new config
				configData["venue_id"] = venueID
				configData["is_active"] = true
				_, err = central.InsertCtx(ctx, "venue_database_configs", configData)
			}

			if err != nil {
				log.Printf("[UpdateVenueAdmin] Error saving database config: %v", err)
				// Don't fail the whole request, just log the error
			} else {
				// Refresh the database router to pick up new config
				go func() {
					if err := services.DB.RefreshVenue(venueID); err != nil {
						log.Printf("[UpdateVenueAdmin] Error refreshing venue in router: %v", err)
					}
				}()
			}
		}
	}

	// Invalidate caches
	if slug := services.GetString(result[0], "slug"); slug != "" {
		services.AppCache.Delete(fmt.Sprintf("public:venue:%s", slug))
	}
	services.AppCache.Delete("public:venues:list")

	c.JSON(http.StatusOK, gin.H{
		"message": "Venue updated successfully",
		"venue":   result[0],
	})
}

// CreateVenueRequest represents the create venue request
type CreateVenueRequest struct {
	OrganizationID     string  `json:"organization_id" binding:"required,uuid"`
	Name               string  `json:"name" binding:"required,min=2"`
	Slug               string  `json:"slug" binding:"required,min=2"`
	Description        string  `json:"description"`
	Location           string  `json:"location"`
	Country            string  `json:"country"`
	Currency           string  `json:"currency"`
	Timezone           string  `json:"timezone"`
	PlatformFeePercent float64 `json:"platform_fee_percent"`
	PlatformFeeFixed   float64 `json:"platform_fee_fixed"`
	SubscriptionPlan   string  `json:"subscription_plan"`
}

// CreateVenue creates a new venue (platform admin only)
// POST /api/v1/admin/venues
func CreateVenue(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Escritura: exige admin (o super_admin, comodín).
	if !requirePlatformRole(c, platformRoleAdmin) {
		return
	}

	var req CreateVenueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Set defaults
	if req.Country == "" {
		req.Country = "GT"
	}
	if req.Currency == "" {
		req.Currency = "GTQ"
	}
	if req.Timezone == "" {
		req.Timezone = "America/Guatemala"
	}
	if req.PlatformFeePercent == 0 {
		req.PlatformFeePercent = 5.00
	}
	if req.SubscriptionPlan == "" {
		req.SubscriptionPlan = "basic"
	}

	// Create venue
	venue, err := central.InsertCtx(ctx, "venues", map[string]interface{}{
		"organization_id":      req.OrganizationID,
		"name":                 req.Name,
		"slug":                 req.Slug,
		"description":          req.Description,
		"location":             req.Location,
		"country":              req.Country,
		"currency":             req.Currency,
		"timezone":             req.Timezone,
		"platform_fee_percent": req.PlatformFeePercent,
		"platform_fee_fixed":   req.PlatformFeeFixed,
		"subscription_status":  "trial",
		"subscription_plan":    req.SubscriptionPlan,
		"is_active":            true,
		"trial_ends_at":        time.Now().Add(14 * 24 * time.Hour).Format(time.RFC3339),
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to create venue",
			"details": err.Error(),
		})
		return
	}

	// Invalidate venues list cache
	services.AppCache.Delete("public:venues:list")

	c.JSON(http.StatusCreated, gin.H{
		"message": "Venue created successfully",
		"venue":   venue,
	})
}

// CreateVenueFullRequest represents the full venue creation request with org
type CreateVenueFullRequest struct {
	// Organization (optional - if org_id not provided, creates new org)
	OrganizationID    string `json:"organization_id"`
	OrganizationName  string `json:"organization_name"`
	OrganizationEmail string `json:"organization_email"`
	// Venue Basic Info
	Name           string   `json:"name" binding:"required,min=2"`
	Slug           string   `json:"slug" binding:"required,min=2"`
	Description    string   `json:"description"`
	OpenTime       string   `json:"open_time"`
	CloseTime      string   `json:"close_time"`
	Timezone       string   `json:"timezone"`
	Currency       string   `json:"currency"`
	Days           []string `json:"days"`
	ContactEmail   string   `json:"contact_email"`
	ContactPhone   string   `json:"contact_phone"`
	WhatsappNumber string   `json:"whatsapp_number"`
	// Location
	Location  string   `json:"location"`
	Address   string   `json:"address"`
	City      string   `json:"city"`
	Country   string   `json:"country"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	// Payment & Flow
	FlowCode              string  `json:"flow_code"`
	PlatformFeePercent    float64 `json:"platform_fee_percent"`
	PlatformFeeFixed      float64 `json:"platform_fee_fixed"`
	PrimaryPaymentGateway string  `json:"primary_payment_gateway"`
	SubscriptionPlan      string  `json:"subscription_plan"`
	// Social Links
	SocialLinks map[string]string `json:"social_links"`
	// Features
	UseVipListFlow bool `json:"use_vip_list_flow"`
	UseGuestList   bool `json:"use_guest_list"`
	// Database config (optional)
	SupabaseURL        string `json:"supabase_url"`
	SupabaseAnonKey    string `json:"supabase_anon_key"`
	SupabaseServiceKey string `json:"supabase_service_key"`
}

// CreateVenueFull creates a new venue with optional organization and database config
// POST /api/v1/admin/venues/full
func CreateVenueFull(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// Escritura: exige admin (o super_admin, comodín). Este handler además
	// acepta credenciales Supabase del venue en el body — con más razón.
	if !requirePlatformRole(c, platformRoleAdmin) {
		return
	}

	var req CreateVenueFullRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fmt.Printf("[ERROR] CreateVenueFull - JSON binding failed: %v\n", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	fmt.Printf("[DEBUG] CreateVenueFull - Request: name=%s, slug=%s, org_name=%s\n", req.Name, req.Slug, req.OrganizationName)

	central := services.DB.Central()
	if central == nil {
		fmt.Println("[ERROR] CreateVenueFull - Central database not available")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Set defaults
	if req.Country == "" {
		req.Country = "GT"
	}
	if req.Currency == "" {
		req.Currency = "GTQ"
	}
	if req.Timezone == "" {
		req.Timezone = "America/Guatemala"
	}
	if req.PlatformFeePercent == 0 {
		req.PlatformFeePercent = 5.00
	}
	if req.SubscriptionPlan == "" {
		req.SubscriptionPlan = "basic"
	}
	if req.FlowCode == "" {
		req.FlowCode = "standard"
	}
	if req.PrimaryPaymentGateway == "" {
		req.PrimaryPaymentGateway = "stripe"
	}

	var orgID string

	// Step 1: Create or use existing organization
	if req.OrganizationID != "" {
		// Use existing org
		orgID = req.OrganizationID
		fmt.Printf("[DEBUG] CreateVenueFull - Using existing org: %s\n", orgID)
	} else if req.OrganizationName != "" {
		// Create new organization
		// First, create a public_user as owner
		fmt.Printf("[DEBUG] CreateVenueFull - Creating owner user for org\n")
		ownerUser, err := central.InsertCtx(ctx, "public_users", map[string]interface{}{
			"email": req.OrganizationEmail,
			"name":  req.OrganizationName,
		})
		if err != nil {
			fmt.Printf("[ERROR] CreateVenueFull - Failed to create owner user: %v\n", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "Failed to create owner user",
				"details": err.Error(),
			})
			return
		}
		ownerID := services.GetString(ownerUser, "id")
		fmt.Printf("[DEBUG] CreateVenueFull - Owner user created with ID: %s\n", ownerID)

		fmt.Printf("[DEBUG] CreateVenueFull - Creating org with name=%s, email=%s, country=%s, owner_id=%s\n", req.OrganizationName, req.OrganizationEmail, req.Country, ownerID)
		org, err := central.InsertCtx(ctx, "organizations", map[string]interface{}{
			"owner_id": ownerID,
			"name":     req.OrganizationName,
			"email":    req.OrganizationEmail,
			"country":  req.Country,
		})
		if err != nil {
			fmt.Printf("[ERROR] CreateVenueFull - Failed to create organization: %v\n", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "Failed to create organization",
				"details": err.Error(),
			})
			return
		}
		orgID = services.GetString(org, "id")
		fmt.Printf("[DEBUG] CreateVenueFull - Organization created with ID: %s\n", orgID)
	} else {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Either organization_id or organization_name is required",
		})
		return
	}

	// Set defaults for required fields
	location := req.Location
	if location == "" {
		location = req.Name // Use venue name as default location
	}
	openTime := req.OpenTime
	if openTime == "" {
		openTime = "21:00"
	}
	closeTime := req.CloseTime
	if closeTime == "" {
		closeTime = "04:00"
	}

	// Step 2: Create venue
	venueData := map[string]interface{}{
		"organization_id":      orgID,
		"name":                 req.Name,
		"slug":                 req.Slug,
		"location":             location,
		"open_time":            openTime,
		"close_time":           closeTime,
		"timezone":             req.Timezone,
		"currency":             req.Currency,
		"country":              req.Country,
		"flow_code":            req.FlowCode,
		"platform_fee_percent": req.PlatformFeePercent,
		"platform_fee_fixed":   req.PlatformFeeFixed,
		"is_active":            true,
	}

	// Add optional string fields (only if not empty)
	if req.Description != "" {
		venueData["description"] = req.Description
	}
	if req.ContactEmail != "" {
		venueData["contact_email"] = req.ContactEmail
	}
	if req.ContactPhone != "" {
		venueData["contact_phone"] = req.ContactPhone
	}
	if req.WhatsappNumber != "" {
		venueData["whatsapp_number"] = req.WhatsappNumber
	}
	if req.Address != "" {
		venueData["address"] = req.Address
	}
	if req.City != "" {
		venueData["city"] = req.City
	}
	if len(req.Days) > 0 {
		venueData["days"] = req.Days
	}
	if req.Latitude != nil {
		venueData["latitude"] = *req.Latitude
	}
	if req.Longitude != nil {
		venueData["longitude"] = *req.Longitude
	}
	if len(req.SocialLinks) > 0 {
		venueData["social_links"] = req.SocialLinks
	}

	fmt.Printf("[DEBUG] CreateVenueFull - Inserting venue with data: %+v\n", venueData)

	venue, err := central.InsertCtx(ctx, "venues", venueData)

	if err != nil {
		fmt.Printf("[ERROR] CreateVenueFull - Insert failed: %v\n", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to create venue",
			"details": err.Error(),
		})
		return
	}

	venueID := services.GetString(venue, "id")

	// Step 3: Create database config if Supabase credentials provided
	if req.SupabaseURL != "" && req.SupabaseServiceKey != "" {
		// Encrypt the service key before storing
		encryptedKey, err := services.EncryptServiceKey(req.SupabaseServiceKey)
		if err != nil {
			// Log but don't fail - venue is created, db config can be added later
			fmt.Printf("[WARN] Failed to encrypt service key for venue %s: %v\n", venueID, err)
		} else {
			// ⚠️ EL NOMBRE DE ESTA COLUMNA IMPORTA MUCHO. Tiene que ser
			// "supabase_service_key_encrypted", que es lo que lee el router
			// (services/database_router.go:277 y :289) y lo que escribe el otro
			// camino de alta (linea ~738 de este mismo fichero).
			//
			// Estuvo escrito como "service_key_encrypted" (sin prefijo) hasta el
			// 2026-08-20. Consecuencia: el router no encontraba la clave, caia
			// por `return r.defaultDB` (database_router.go:225) — que en
			// produccion es la BD del venue de 511 — y el cliente nuevo empezaba
			// a escribir sus eventos y pedidos DENTRO de los datos de 511, sin
			// un solo error visible. No llego a pasar porque todavia no hay un
			// segundo cliente.
			_, err = central.InsertCtx(ctx, "venue_database_configs", map[string]interface{}{
				"venue_id":                       venueID,
				"supabase_url":                   req.SupabaseURL,
				"supabase_anon_key":              req.SupabaseAnonKey,
				"supabase_service_key_encrypted": encryptedKey,
				"is_active":                      true,
			})
			if err != nil {
				fmt.Printf("[WARN] Failed to create database config for venue %s: %v\n", venueID, err)
			}
		}
	}

	// Invalidate venues list cache
	services.AppCache.Delete("public:venues:list")

	// Refresh database router to pick up new venue
	go func() {
		if err := services.DB.RefreshVenue(venueID); err != nil {
			fmt.Printf("[WARN] Failed to refresh venue %s in router: %v\n", venueID, err)
		}
	}()

	c.JSON(http.StatusCreated, gin.H{
		"message":         "Venue created successfully",
		"venue":           venue,
		"organization_id": orgID,
	})
}

// DeleteVenue deletes a venue and all related data (cascade)
// DELETE /api/v1/secure-admin/venues/:id
func DeleteVenue(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// SOLO super_admin: esto borra en cascada el venue y su historial contable
	// (transacciones incluidas). Es irreversible y no había ninguna
	// comprobación de rol.
	if !requirePlatformRole(c, platformRoleSuperAdmin) {
		return
	}

	venueID := c.Param("id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Venue ID is required"})
		return
	}

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Step 1: Get venue info before deletion (for cache invalidation and getting org_id)
	venue, err := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id,slug,organization_id",
		"where": map[string]interface{}{
			"id":         venueID,
			"deleted_at": "is.null",
		},
	})

	if err != nil {
		log.Printf("[DeleteVenue] Error querying venue: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to query venue", "details": err.Error()})
		return
	}

	if venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}

	slug := services.GetString(venue, "slug")
	orgID := services.GetString(venue, "organization_id")

	// Delete all dependent tables in correct order (respecting foreign keys)
	// Order matters: delete children before parents

	// Step 2: Delete webhook_logs (references venues)
	err = central.DeleteCtx(ctx, "webhook_logs", map[string]interface{}{
		"venue_id": venueID,
	})
	if err != nil {
		log.Printf("[DeleteVenue] Warning: Failed to delete webhook_logs: %v", err)
	}

	// Step 3: Delete support_requests (references venues)
	err = central.DeleteCtx(ctx, "support_requests", map[string]interface{}{
		"venue_id": venueID,
	})
	if err != nil {
		log.Printf("[DeleteVenue] Warning: Failed to delete support_requests: %v", err)
	}

	// Step 4: Delete transactions (references venues) - first delete line items
	// Get transaction IDs first
	transactions, _ := central.QueryCtx(ctx, "transactions", map[string]interface{}{
		"select": "id",
		"where": map[string]interface{}{
			"venue_id": venueID,
		},
	})
	for _, tx := range transactions {
		txID := services.GetString(tx, "id")
		if txID != "" {
			central.DeleteCtx(ctx, "transaction_line_items", map[string]interface{}{
				"transaction_id": txID,
			})
		}
	}
	err = central.DeleteCtx(ctx, "transactions", map[string]interface{}{
		"venue_id": venueID,
	})
	if err != nil {
		log.Printf("[DeleteVenue] Warning: Failed to delete transactions: %v", err)
	}

	// Step 5: Delete venue_fee_history (references venues)
	err = central.DeleteCtx(ctx, "venue_fee_history", map[string]interface{}{
		"venue_id": venueID,
	})
	if err != nil {
		log.Printf("[DeleteVenue] Warning: Failed to delete venue_fee_history: %v", err)
	}

	// Step 6: Delete venue_customization (references venues)
	err = central.DeleteCtx(ctx, "venue_customization", map[string]interface{}{
		"venue_id": venueID,
	})
	if err != nil {
		log.Printf("[DeleteVenue] Warning: Failed to delete venue_customization: %v", err)
	}

	// Step 7: Delete payment_gateway_credentials (references venues)
	err = central.DeleteCtx(ctx, "payment_gateway_credentials", map[string]interface{}{
		"venue_id": venueID,
	})
	if err != nil {
		log.Printf("[DeleteVenue] Warning: Failed to delete payment_gateway_credentials: %v", err)
	}

	// Step 8: Delete venue_database_configs (references venues)
	err = central.DeleteCtx(ctx, "venue_database_configs", map[string]interface{}{
		"venue_id": venueID,
	})
	if err != nil {
		log.Printf("[DeleteVenue] Warning: Failed to delete venue_database_configs: %v", err)
	}

	// Step 9: Delete organization_workers (references venues)
	err = central.DeleteCtx(ctx, "organization_workers", map[string]interface{}{
		"venue_id": venueID,
	})
	if err != nil {
		log.Printf("[DeleteVenue] Warning: Failed to delete organization_workers: %v", err)
	}

	// Step 10: Delete the venue itself
	err = central.DeleteCtx(ctx, "venues", map[string]interface{}{
		"id": venueID,
	})
	if err != nil {
		log.Printf("[DeleteVenue] Error deleting venue: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete venue", "details": err.Error()})
		return
	}

	// Step 11: Delete the organization (if no other venues use it)
	if orgID != "" {
		// Check if other venues use this organization
		otherVenues, err := central.QueryCtx(ctx, "venues", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"organization_id": orgID,
			},
			"limit": "1",
		})

		if err == nil && len(otherVenues) == 0 {
			// No other venues, safe to delete organization
			err = central.DeleteCtx(ctx, "organizations", map[string]interface{}{
				"id": orgID,
			})
			if err != nil {
				log.Printf("[DeleteVenue] Warning: Failed to delete organization: %v", err)
			}
		}
	}

	// Step 6: Invalidate caches
	if slug != "" {
		services.AppCache.Delete(fmt.Sprintf("public:venue:%s", slug))
	}
	services.AppCache.Delete("public:venues:list")

	// Step 7: Remove from database router
	services.DB.RemoveVenue(venueID)

	log.Printf("[DeleteVenue] Successfully deleted venue %s (slug: %s)", venueID, slug)

	c.JSON(http.StatusOK, gin.H{
		"message": "Venue deleted successfully",
	})
}

// UpdateVenueFees updates the platform fees for a venue
// PUT /api/v1/admin/venues/:id/fees
func UpdateVenueFees(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Escritura: exige admin (o super_admin, comodín). Esto cambia la comisión
	// que se le cobra al cliente en cada venta — no es una lectura.
	if !requirePlatformRole(c, platformRoleAdmin) {
		return
	}

	venueID := c.Param("id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Venue ID is required"})
		return
	}

	var req struct {
		PlatformFeePercent float64 `json:"platform_fee_percent"`
		PlatformFeeFixed   float64 `json:"platform_fee_fixed"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not available"})
		return
	}

	// Update fees
	result, err := central.UpdateCtx(ctx, "venues", map[string]interface{}{
		"platform_fee_percent": req.PlatformFeePercent,
		"platform_fee_fixed":   req.PlatformFeeFixed,
		"updated_at":           time.Now().Format(time.RFC3339),
	}, map[string]interface{}{
		"id": venueID,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update fees"})
		return
	}

	if len(result) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Fees updated successfully",
		"venue":   result[0],
	})
}
