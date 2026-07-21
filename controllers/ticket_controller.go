package controllers

import (
	"context"
	"net/http"
	"pull-api-v2/services"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// =============================================
// USER TICKET ENDPOINTS
// =============================================

// GetMyTickets returns tickets for the authenticated user
// GET /api/v1/tickets/my
// OPTIMIZED: Batch event fetching instead of N+1 queries
func GetMyTickets(c *gin.Context) {
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

	// Parse filters
	upcoming := c.Query("upcoming")
	eventID := c.Query("event_id")

	// Build where clause
	whereClause := map[string]interface{}{
		"holder_id": userID,
	}

	if eventID != "" {
		whereClause["event_id"] = eventID
	}

	// Get tickets (columnas REALES de la BD viva — pdf_url no existe)
	tickets, err := venueDB.QueryCtx(ctx, "tickets", map[string]interface{}{
		"select": "id,order_id,event_id,ticket_type_id,qr_token,source,ticket_type_name,owner_name,owner_last_name,owner_email,checked_in_at,created_at",
		"where":  whereClause,
		"order":  "created_at.desc",
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get tickets"})
		return
	}

	if len(tickets) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"tickets": []map[string]interface{}{},
			"count":   0,
		})
		return
	}

	// OPTIMIZATION: Extract unique event IDs and batch fetch all events
	eventIDs := services.ExtractIDs(tickets, "event_id")

	var events []map[string]interface{}
	if len(eventIDs) > 0 {
		// Columnas REALES (event_date/start_time no existen — los deriva
		// EnrichEvent de start_datetime).
		events, _ = venueDB.QueryCtx(ctx, "events", map[string]interface{}{
			"select": "id,name,slug,image,cover_image,start_datetime,end_datetime,location,address",
			"where":  map[string]interface{}{"id": "in.(" + joinIDs(eventIDs) + ")"},
		})
		for _, ev := range events {
			services.EnrichEvent(ev)
		}
	}

	// Build event map for O(1) lookup
	eventMap := services.BuildIDMap(events, "id")

	// Enrich tickets with event data
	today := time.Now().Format("2006-01-02")
	enrichedTickets := make([]map[string]interface{}, 0, len(tickets))

	for _, ticket := range tickets {
		evtID := services.GetString(ticket, "event_id")
		if event, ok := eventMap[evtID]; ok {
			ticket["event"] = event

			// Compat con el wallet de la web: espera campos planos
			// (event_name/event_date/start_time/validated_at) y los nests
			// de los joins v1 (events.image, events.venues.location,
			// ticket_types.name).
			img := services.GetString(event, "image")
			if img == "" {
				img = services.GetString(event, "cover_image")
			}
			ticket["event_name"] = services.GetString(event, "name")
			ticket["event_date"] = services.GetString(event, "event_date")
			ticket["start_time"] = services.GetString(event, "start_time")
			ticket["events"] = map[string]interface{}{
				"image": img,
				"venues": map[string]interface{}{
					"location": services.GetString(event, "location"),
				},
			}

			// Filter by upcoming if requested
			if upcoming == "true" {
				eventDate := services.GetString(event, "event_date")
				if eventDate != "" && eventDate < today {
					continue // Skip past events
				}
			}
		}
		ticket["ticket_types"] = map[string]interface{}{
			"name": services.GetString(ticket, "ticket_type_name"),
		}
		ticket["validated_at"] = ticket["checked_in_at"]

		enrichedTickets = append(enrichedTickets, ticket)
	}

	c.JSON(http.StatusOK, gin.H{
		"tickets": enrichedTickets,
		"count":   len(enrichedTickets),
	})
}

// GetTicketByQR returns a ticket by QR token (for display)
// GET /api/v1/tickets/qr/:token
func GetTicketByQR(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	qrToken := c.Param("token")
	venueID := c.Query("venue_id")

	if qrToken == "" || venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "QR token and venue_id are required"})
		return
	}
	// SECURITY: block PostgREST operator injection via the token param.
	if !safeLookupCode(qrToken) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid QR token"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Get ticket
	ticket, err := venueDB.QueryOne(ctx, "tickets", map[string]interface{}{
		"select": "id,order_id,event_id,ticket_type_id,holder_id,qr_token,source,ticket_type_name,owner_name,owner_last_name,owner_email,checked_in_at,pdf_url,created_at",
		"where": map[string]interface{}{
			"qr_token": qrToken,
		},
	})

	if err != nil || ticket == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Ticket not found"})
		return
	}

	// Get event info
	eventID := services.GetString(ticket, "event_id")
	if eventID != "" {
		event, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "name,slug,event_date,start_time,end_time,image,custom_location",
			"where":  map[string]interface{}{"id": eventID},
		})
		if event != nil {
			ticket["event"] = event
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"ticket": ticket,
	})
}

// GetTicketPDF returns the PDF URL for a ticket
// GET /api/v1/tickets/:id/pdf
func GetTicketPDF(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	ticketID := c.Param("id")
	userID := c.GetString("user_id")
	venueID := c.Query("venue_id")

	if ticketID == "" || venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Ticket ID and venue_id are required"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Get ticket (verify ownership if user is authenticated)
	whereClause := map[string]interface{}{
		"id": ticketID,
	}
	if userID != "" {
		whereClause["holder_id"] = userID
	}

	ticket, err := venueDB.QueryOne(ctx, "tickets", map[string]interface{}{
		"select": "id,pdf_url,qr_token",
		"where":  whereClause,
	})

	if err != nil || ticket == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Ticket not found"})
		return
	}

	pdfURL := services.GetString(ticket, "pdf_url")
	if pdfURL == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "PDF not available"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"pdf_url": pdfURL,
	})
}

// =============================================
// STAFF TICKET VALIDATION
// =============================================

// ValidateTicketRequest represents the validate ticket request
type ValidateTicketRequest struct {
	QRToken string `json:"qr_token" binding:"required"`
}

// ValidateTicket validates a ticket at entry (staff)
// POST /api/v1/validate/ticket
// OPTIMIZED: Parallel ticket + event fetch
func ValidateTicket(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	staffID := c.GetString("staff_id")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	var req ValidateTicketRequest
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

	// Find ticket by QR token
	ticket, err := venueDB.QueryOne(ctx, "tickets", map[string]interface{}{
		"select": "id,event_id,ticket_type_name,owner_name,owner_last_name,owner_email,owner_gender,checked_in_at,checked_in_by",
		"where": map[string]interface{}{
			"qr_token": req.QRToken,
		},
	})

	if err != nil || ticket == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"valid":   false,
			"error":   "Ticket not found",
			"message": "QR code not recognized",
		})
		return
	}

	ticketID := services.GetString(ticket, "id")
	eventID := services.GetString(ticket, "event_id")

	// Check if already validated
	checkedInAt := services.GetTime(ticket, "checked_in_at")
	if checkedInAt != nil {
		c.JSON(http.StatusConflict, gin.H{
			"valid":         false,
			"error":         "already_validated",
			"message":       "Ticket already used",
			"checked_in_at": checkedInAt.Format(time.RFC3339),
			"ticket":        ticket,
		})
		return
	}

	// Get event info (needed for response anyway)
	var event map[string]interface{}
	if eventID != "" {
		event, _ = venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "name,event_date,start_time",
			"where":  map[string]interface{}{"id": eventID},
		})

		if event != nil {
			eventDate := services.GetString(event, "event_date")
			today := time.Now().Format("2006-01-02")

			// Allow validation on event date only
			if eventDate != "" && eventDate != today {
				if eventDate > today {
					c.JSON(http.StatusBadRequest, gin.H{
						"valid":      false,
						"error":      "event_not_started",
						"message":    "Event has not started yet",
						"event_date": eventDate,
					})
					return
				}
			}
		}
	}

	// Validate ticket (mark as used). The checked_in_at guard makes the
	// update atomic: two simultaneous scans of the same QR can't both win.
	now := time.Now()
	lock, err := venueDB.UpdateCtx(ctx, "tickets", map[string]interface{}{
		"checked_in_at": now.Format(time.RFC3339),
		"checked_in_by": staffID,
		"validated_at":  now.Format(time.RFC3339),
	}, map[string]interface{}{
		"id":            ticketID,
		"checked_in_at": "is.null",
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"valid": false,
			"error": "Failed to validate ticket",
		})
		return
	}
	if len(lock) == 0 {
		c.JSON(http.StatusConflict, gin.H{
			"valid":   false,
			"error":   "already_validated",
			"message": "Ticket already used",
			"ticket":  ticket,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"valid":   true,
		"message": "Ticket validated successfully",
		"ticket": map[string]interface{}{
			"id":              ticketID,
			"ticket_type":     services.GetString(ticket, "ticket_type_name"),
			"owner_name":      services.GetString(ticket, "owner_name"),
			"owner_last_name": services.GetString(ticket, "owner_last_name"),
			"owner_gender":    services.GetString(ticket, "owner_gender"),
			"checked_in_at":   now.Format(time.RFC3339),
		},
		"event": event,
	})
}

// =============================================
// STAFF TICKET MANAGEMENT
// =============================================

// GetStaffEventTickets returns all tickets for an event (staff)
// GET /api/v1/staff/events/:id/tickets
// OPTIMIZED: Single-pass stats calculation
func GetStaffEventTickets(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	eventID := c.Param("id")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	if eventID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Event ID is required"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	// Parse filters
	checkedIn := c.Query("checked_in")
	source := c.Query("source")

	whereClause := map[string]interface{}{
		"event_id": eventID,
	}

	if checkedIn == "true" {
		whereClause["checked_in_at"] = "not.is.null"
	} else if checkedIn == "false" {
		whereClause["checked_in_at"] = "is.null"
	}

	if source != "" {
		whereClause["source"] = source
	}

	// Get tickets
	tickets, err := venueDB.QueryCtx(ctx, "tickets", map[string]interface{}{
		"select": "id,order_id,ticket_type_id,ticket_type_name,holder_id,owner_name,owner_last_name,owner_email,owner_gender,source,checked_in_at,checked_in_by,created_at",
		"where":  whereClause,
		"order":  "created_at.desc",
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get tickets"})
		return
	}

	// Calculate stats in single pass
	totalCount := len(tickets)
	checkedInCount := 0
	for _, t := range tickets {
		if services.GetTime(t, "checked_in_at") != nil {
			checkedInCount++
		}
	}

	// Calculate rate
	var checkInRate float64
	if totalCount > 0 {
		checkInRate = float64(checkedInCount) / float64(totalCount) * 100
	}

	c.JSON(http.StatusOK, gin.H{
		"tickets": tickets,
		"stats": map[string]interface{}{
			"total":         totalCount,
			"checked_in":    checkedInCount,
			"pending":       totalCount - checkedInCount,
			"check_in_rate": checkInRate,
		},
	})
}

// ManualCheckIn manually creates a ticket for walk-in (staff)
// POST /api/v1/staff/events/:id/manual-checkin
// OPTIMIZED: Parallel user lookup + event validation
func ManualCheckIn(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	staffID := c.GetString("staff_id")
	role := c.GetString("role")
	eventID := c.Param("id")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// Only admin or manager can do manual check-in
	if role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
		return
	}

	var req struct {
		Name     string `json:"name" binding:"required"`
		LastName string `json:"last_name"`
		Email    string `json:"email"`
		Gender   string `json:"gender"`
		Note     string `json:"note"`
	}

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

	// OPTIMIZATION: Parallel user lookup and event validation
	var user map[string]interface{}
	var event map[string]interface{}
	var wg sync.WaitGroup
	var mu sync.Mutex

	if req.Email != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, _ := venueDB.QueryOne(ctx, "public_users", map[string]interface{}{
				"select": "id",
				"where":  map[string]interface{}{"email": req.Email},
			})
			mu.Lock()
			user = u
			mu.Unlock()
		}()
	}

	// Validate event exists
	wg.Add(1)
	go func() {
		defer wg.Done()
		e, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "id,name",
			"where":  map[string]interface{}{"id": eventID},
		})
		mu.Lock()
		event = e
		mu.Unlock()
	}()

	wg.Wait()

	if event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}

	// Get or create user
	var userID string
	if user != nil {
		userID = services.GetString(user, "id")
	}

	if userID == "" {
		newUser, err := venueDB.InsertCtx(ctx, "public_users", map[string]interface{}{
			"email":   req.Email,
			"name":    req.Name,
			"surname": req.LastName,
			"gender":  req.Gender,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user"})
			return
		}
		userID = services.GetString(newUser, "id")
	}

	// Generate QR token
	qrToken, _ := generateRandomCode(32)

	// Create ticket (already checked in)
	now := time.Now()
	ticket, err := venueDB.InsertCtx(ctx, "tickets", map[string]interface{}{
		"event_id":         eventID,
		"holder_id":        userID,
		"qr_token":         qrToken,
		"source":           "manual",
		"ticket_type_name": "Manual Entry",
		"owner_name":       req.Name,
		"owner_last_name":  req.LastName,
		"owner_email":      req.Email,
		"owner_gender":     req.Gender,
		"checked_in_at":    now.Format(time.RFC3339),
		"checked_in_by":    staffID,
		"validated_at":     now.Format(time.RFC3339),
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to create ticket",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "Manual check-in successful",
		"ticket":  ticket,
	})
}

// UndoCheckIn reverts a check-in (staff)
// POST /api/v1/staff/tickets/:id/undo-checkin
func UndoCheckIn(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	role := c.GetString("role")
	ticketID := c.Param("id")

	if venueID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}

	// Only admin can undo check-in
	if role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	// Update ticket
	result, err := venueDB.UpdateCtx(ctx, "tickets", map[string]interface{}{
		"checked_in_at": nil,
		"checked_in_by": nil,
		"validated_at":  nil,
	}, map[string]interface{}{
		"id": ticketID,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to undo check-in"})
		return
	}

	if len(result) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Ticket not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Check-in undone successfully",
		"ticket":  result[0],
	})
}

// joinIDs joins string IDs for IN clause
func joinIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	result := ids[0]
	for i := 1; i < len(ids); i++ {
		result += "," + ids[i]
	}
	return result
}
