package controllers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"pull-api-v2/middleware"
	"pull-api-v2/models"
	"pull-api-v2/services"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v76/webhook"
)

// =============================================
// ORDER CREATION
// =============================================

// CreateOrderRequest represents the order creation request
type CreateOrderRequest struct {
	EventID      string `json:"event_id" binding:"required,uuid"`
	TicketTypeID string `json:"ticket_type_id" binding:"required,uuid"`
	Quantity     int    `json:"quantity" binding:"required,gte=1"`
	VenueID      string `json:"venue_id" binding:"required,uuid"`

	// User info (if not authenticated)
	UserName  string `json:"user_name"`
	UserEmail string `json:"user_email" binding:"required,email"`

	// Optional
	Gender string `json:"gender"` // For gender-priced tickets
}

// CreateOrder creates a new order (reservation)
// POST /api/v1/orders/create
// OPTIMIZED: Parallel user lookup + ticket validation
func CreateOrder(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.SafeError(c, http.StatusBadRequest, "Invalid request format", err)
		return
	}

	venueDB := services.DB.ForVenue(req.VenueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// SECURITY: Sanitize user inputs
	email := middleware.SanitizeEmail(req.UserEmail)
	if email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid email"})
		return
	}
	userName := middleware.SanitizeName(req.UserName)

	// OPTIMIZATION: Parallel ticket type + user lookup
	var ticketType, user map[string]interface{}
	var ticketErr error
	var wg sync.WaitGroup

	wg.Add(2)

	// Get ticket type
	go func() {
		defer wg.Done()
		ticketType, ticketErr = venueDB.QueryOne(ctx, "ticket_types", map[string]interface{}{
			"select": services.TicketTypeSelectColumns,
			"where": map[string]interface{}{
				"id":        req.TicketTypeID,
				"is_active": true,
			},
		})
		if ticketType != nil {
			services.EnrichTicketType(ticketType)
		}
	}()

	// Find user
	go func() {
		defer wg.Done()
		user, _ = venueDB.QueryOne(ctx, "public_users", map[string]interface{}{
			"select": "id",
			"where":  map[string]interface{}{"email": email},
		})
	}()

	wg.Wait()

	if ticketErr != nil || ticketType == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Ticket type not found"})
		return
	}

	// Validate event matches
	if services.GetString(ticketType, "event_id") != req.EventID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Ticket type does not belong to this event"})
		return
	}

	// Check quantity limits
	minQty := services.GetInt(ticketType, "min_quantity")
	maxQty := services.GetInt(ticketType, "max_quantity")
	availableQty := services.GetInt(ticketType, "available_quantity")

	if req.Quantity < minQty || req.Quantity > maxQty {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Quantity must be between %d and %d", minQty, maxQty),
		})
		return
	}

	if req.Quantity > availableQty {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":     "Not enough tickets available",
			"available": availableQty,
		})
		return
	}

	// Calculate price
	var unitPrice float64
	hasGenderPricing := services.GetBool(ticketType, "has_gender_pricing")

	if hasGenderPricing && req.Gender != "" {
		switch req.Gender {
		case "male":
			unitPrice = services.GetFloat64(ticketType, "male_price")
		case "female":
			unitPrice = services.GetFloat64(ticketType, "female_price")
		default:
			unitPrice = services.GetFloat64(ticketType, "price")
		}
	} else {
		unitPrice = services.GetFloat64(ticketType, "price")
	}

	total := unitPrice * float64(req.Quantity)

	// Get or create user
	var userID string
	if user == nil {
		newUser, err := venueDB.InsertCtx(ctx, "public_users", map[string]interface{}{
			"email": email,
			"name":  userName,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user"})
			return
		}
		userID = services.GetString(newUser, "id")
	} else {
		userID = services.GetString(user, "id")
	}

	// Generate payment link code (stored in metadata; payment_link_code column
	// doesn't exist in current schema, so we surface it back via response only).
	paymentLinkCode, _ := generateRandomCode(16)

	// Create order
	expiresAt := time.Now().Add(30 * time.Minute)

	order, err := venueDB.InsertCtx(ctx, "orders", map[string]interface{}{
		"event_id":       req.EventID,
		"ticket_type_id": req.TicketTypeID,
		"user_id":        userID,
		"quantity":       req.Quantity,
		"unit_price":     unitPrice,
		"subtotal":       total,
		"total":          total,
		"currency":       "GTQ",
		"status":         "pending",
		"user_name":      userName,
		"user_email":     email,
		"expires_at":     expiresAt.Format(time.RFC3339),
		"metadata":       map[string]interface{}{"payment_link_code": paymentLinkCode},
	})

	if err != nil {
		middleware.SafeError(c, http.StatusInternalServerError, "Failed to create order", err)
		return
	}

	// Reserve tickets (fire-and-forget): increment quantity_reserved
	currentReserved := services.GetInt(ticketType, "quantity_reserved")
	newReserved := currentReserved + req.Quantity
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()
		venueDB.UpdateNoReturn(bgCtx, "ticket_types", map[string]interface{}{
			"quantity_reserved": newReserved,
		}, map[string]interface{}{
			"id": req.TicketTypeID,
		})
	}()
	_ = availableQty // value already validated above

	c.JSON(http.StatusCreated, gin.H{
		"message": "Order created successfully",
		"order": map[string]interface{}{
			"id":                services.GetString(order, "id"),
			"order_number":      services.GetString(order, "order_number"),
			"payment_link_code": paymentLinkCode,
			"quantity":          req.Quantity,
			"total":             total,
			"currency":          "GTQ",
			"expires_at":        expiresAt.Format(time.RFC3339),
		},
	})
}

// =============================================
// CHECKOUT & PAYMENT
// =============================================

// CheckoutRequest represents the checkout request
type CheckoutRequest struct {
	OrderID   string `json:"order_id" binding:"required,uuid"`
	VenueID   string `json:"venue_id" binding:"required,uuid"`
	ReturnURL string `json:"return_url"`
	CancelURL string `json:"cancel_url"`
}

// CreateCheckout initiates payment for an order
// POST /api/v1/orders/checkout
// OPTIMIZED: Parallel order + processor fetch
func CreateCheckout(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req CheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request",
			"details": err.Error(),
		})
		return
	}

	venueDB := services.DB.ForVenue(req.VenueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// OPTIMIZATION: Parallel order + processor fetch
	var order map[string]interface{}
	var processor services.PaymentProcessor
	var orderErr, processorErr error
	var wg sync.WaitGroup

	wg.Add(2)

	// Get order
	go func() {
		defer wg.Done()
		order, orderErr = venueDB.QueryOne(ctx, "orders", map[string]interface{}{
			"select": "id,order_number,event_id,ticket_type_id,user_id,quantity,total,currency,status,user_email",
			"where": map[string]interface{}{
				"id":     req.OrderID,
				"status": "pending",
			},
		})
	}()

	// Get payment processor
	go func() {
		defer wg.Done()
		processor, processorErr = services.Payments.GetProcessor(ctx, req.VenueID)
	}()

	wg.Wait()

	if orderErr != nil || order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found or already processed"})
		return
	}

	if processorErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway not configured"})
		return
	}

	// Get ticket type name for description
	ticketType, _ := venueDB.QueryOne(ctx, "ticket_types", map[string]interface{}{
		"select": "name",
		"where":  map[string]interface{}{"id": services.GetString(order, "ticket_type_id")},
	})
	ticketTypeName := "Tickets"
	if ticketType != nil {
		ticketTypeName = services.GetString(ticketType, "name")
	}

	// Create checkout session
	checkoutParams := models.CheckoutParams{
		Amount:        services.GetFloat64(order, "total"),
		Currency:      services.GetString(order, "currency"),
		OrderID:       services.GetString(order, "id"),
		ProductName:   fmt.Sprintf("%d x %s", services.GetInt(order, "quantity"), ticketTypeName),
		CustomerEmail: services.GetString(order, "user_email"),
		SuccessURL:    req.ReturnURL,
		CancelURL:     req.CancelURL,
		Metadata: map[string]string{
			"venue_id":     req.VenueID,
			"order_id":     services.GetString(order, "id"),
			"event_id":     services.GetString(order, "event_id"),
			"order_number": services.GetString(order, "order_number"),
		},
	}

	checkout, err := processor.CreateCheckout(ctx, checkoutParams)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to create checkout",
			"details": err.Error(),
		})
		return
	}

	// Update order with payment session info (fire-and-forget)
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()
		venueDB.UpdateNoReturn(bgCtx, "orders", map[string]interface{}{
			"status":            "processing",
			"stripe_session_id": checkout.SessionID,
			"payment_gateway":   processor.GetGateway().String(),
		}, map[string]interface{}{
			"id": req.OrderID,
		})
	}()

	c.JSON(http.StatusOK, gin.H{
		"checkout_url": checkout.CheckoutURL,
		"session_id":   checkout.SessionID,
		"gateway":      processor.GetGateway().String(),
	})
}

// ConfirmPayment handles the payment callback
// GET /api/v1/orders/confirm
// OPTIMIZED: Batch ticket creation
// SECURITY: Atomic status update prevents race condition / duplicate tickets
func ConfirmPayment(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	sessionID := c.Query("session_id")
	venueID := c.Query("venue_id")

	if sessionID == "" || venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id and venue_id are required"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Find order by session ID
	order, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,event_id,ticket_type_id,user_id,quantity,total,currency,status,user_email,user_name,metadata",
		"where": map[string]interface{}{
			"stripe_session_id": sessionID,
		},
	})

	if err != nil || order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	orderID := services.GetString(order, "id")
	currentStatus := services.GetString(order, "status")

	// Check if already confirmed
	if currentStatus == "confirmed" {
		c.JSON(http.StatusOK, gin.H{
			"success":      true,
			"message":      "Order already confirmed",
			"order_number": services.GetString(order, "order_number"),
			"order_id":     orderID,
		})
		return
	}

	// SECURITY: Atomic status transition to prevent race condition.
	// order_status enum allows pending/processing/confirmed/cancelled/refunded/
	// expired — there is no intermediate "payment_authorized" state, so we
	// transition directly processing->confirmed. The UPDATE is atomic: only
	// the first request that flips the row succeeds; concurrent retries see
	// 0 rows updated and bail.
	lockResult, err := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
		"status":     "confirmed",
		"updated_at": time.Now().Format(time.RFC3339),
	}, map[string]interface{}{
		"id":     orderID,
		"status": "processing",
	})

	if err != nil || len(lockResult) == 0 {
		// Another request is already processing this order
		log.Printf("[ConfirmPayment] RACE CONDITION PREVENTED: Order %s already being processed", orderID)
		c.JSON(http.StatusConflict, gin.H{
			"error":   "Order is being processed",
			"message": "Please wait a moment and try again",
		})
		return
	}

	// OPTIMIZATION: Parallel processor + ticket type fetch
	var processor services.PaymentProcessor
	var ticketType map[string]interface{}
	var processorErr error
	var wg sync.WaitGroup

	ticketTypeID := services.GetString(order, "ticket_type_id")

	wg.Add(2)

	go func() {
		defer wg.Done()
		processor, processorErr = services.Payments.GetProcessor(ctx, venueID)
	}()

	go func() {
		defer wg.Done()
		ticketType, _ = venueDB.QueryOne(ctx, "ticket_types", map[string]interface{}{
			"select": "name",
			"where":  map[string]interface{}{"id": ticketTypeID},
		})
	}()

	wg.Wait()

	if processorErr != nil {
		// Revert status on failure
		venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
			"status": "processing",
		}, map[string]interface{}{"id": orderID})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway not configured"})
		return
	}

	// Confirm payment with gateway
	paymentResult, err := processor.ConfirmPayment(ctx, sessionID)
	if err != nil {
		// Revert status on failure
		venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
			"status": "processing",
		}, map[string]interface{}{"id": orderID})
		middleware.SafeError(c, http.StatusInternalServerError, "Failed to confirm payment", err)
		return
	}

	if !paymentResult.Success {
		// Revert status on payment failure
		venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
			"status": "processing",
		}, map[string]interface{}{"id": orderID})
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error":  "Payment not completed",
			"detail": paymentResult.ErrorMessage,
		})
		return
	}

	quantity := services.GetInt(order, "quantity")
	eventID := services.GetString(order, "event_id")
	userID := services.GetString(order, "user_id")
	userName := services.GetString(order, "user_name")
	userEmail := services.GetString(order, "user_email")

	ticketTypeName := ""
	if ticketType != nil {
		ticketTypeName = services.GetString(ticketType, "name")
	}

	// Pull any tickets_data the WebApp sent at create-time so we can preserve
	// per-ticket owner details (incl. Instagram) at confirm-time.
	var perTicketDetails []map[string]interface{}
	if md, ok := order["metadata"].(map[string]interface{}); ok {
		if td, ok := md["tickets_data"].([]interface{}); ok {
			for _, item := range td {
				if row, ok := item.(map[string]interface{}); ok {
					perTicketDetails = append(perTicketDetails, row)
				}
			}
		}
	}

	// OPTIMIZATION: Batch create tickets (single INSERT with multiple rows)
	ticketData := make([]map[string]interface{}, quantity)
	for i := 0; i < quantity; i++ {
		qrToken, _ := generateRandomCode(32)
		row := map[string]interface{}{
			"order_id":         orderID,
			"event_id":         eventID,
			"ticket_type_id":   ticketTypeID,
			"holder_id":        userID,
			"qr_token":         qrToken,
			"source":           "order",
			"ticket_type_name": ticketTypeName,
			"owner_name":       userName,
			"owner_email":      userEmail,
		}
		// Overlay per-ticket details (owner_last_name, owner_phone, gender,
		// instagram, etc.) when the WebApp captured a form per attendee.
		if i < len(perTicketDetails) {
			d := perTicketDetails[i]
			if v := services.GetString(d, "owner_name"); v != "" {
				row["owner_name"] = v
			}
			if v := services.GetString(d, "owner_last_name"); v != "" {
				row["owner_last_name"] = v
			}
			if v := services.GetString(d, "owner_email"); v != "" {
				row["owner_email"] = v
			}
			if v := services.GetString(d, "owner_phone"); v != "" {
				row["owner_phone"] = v
			}
			if v := services.GetString(d, "owner_gender"); v != "" {
				row["owner_gender"] = v
			}
			// Instagram → metadata.instagram for now (no dedicated column).
			if ig := services.GetString(d, "owner_instagram"); ig != "" {
				row["metadata"] = map[string]interface{}{"instagram": ig}
			}
		}
		ticketData[i] = row
	}

	// Single batch insert instead of N individual inserts
	ticketCount := quantity
	if err := venueDB.InsertBatch(ctx, "tickets", ticketData); err != nil {
		// Fallback: try individual inserts if batch fails
		ticketCount = 0
		for _, ticket := range ticketData {
			if _, err := venueDB.InsertCtx(ctx, "tickets", ticket); err == nil {
				ticketCount++
			}
		}
	}

	// Order was already flipped to "confirmed" by the atomic lock above; here
	// we only persist payment metadata.
	venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
		"stripe_payment_intent": paymentResult.TransactionID,
		"paid_at":               time.Now().Format(time.RFC3339),
	}, map[string]interface{}{
		"id": orderID,
	})

	// Record platform transaction (fire-and-forget)
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer bgCancel()
		recordPlatformTransaction(bgCtx, venueID, order, paymentResult)
	}()

	// Notify venue staff devices about the confirmed purchase (fire-and-forget).
	go func() {
		if services.Push == nil {
			return
		}
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer bgCancel()
		eventName := ""
		if ev, _ := venueDB.QueryOne(bgCtx, "events", map[string]interface{}{
			"select": "name", "where": map[string]interface{}{"id": eventID},
		}); ev != nil {
			eventName = services.GetString(ev, "name")
		}
		body := fmt.Sprintf("%s compró %d ticket(s)", userName, quantity)
		if eventName != "" {
			body += " — " + eventName
		}
		services.Push.NotifyVenueStaff(bgCtx, venueID, "Nueva compra confirmada", body, "bookings", map[string]interface{}{
			"type":         "order_confirmed",
			"order_id":     orderID,
			"order_number": services.GetString(order, "order_number"),
			"event_id":     eventID,
		})
	}()

	// Send confirmation email with PDF attached + inline QR codes. Runs on the
	// bounded task queue (NOT a raw goroutine): PDF+QR rendering is the
	// heaviest allocation in the app, and a purchase burst with unbounded
	// goroutines can OOM the 512MB machine.
	services.RunBackground("order-tickets-email", func(bgCtx context.Context) error {
		if services.Email == nil {
			return nil
		}

		// Resolve event + venue context for the email.
		eventName := ""
		eventDate := ""
		eventTime := ""
		eventImage := ""
		venueName := ""
		venueAddress := ""
		if ev, _ := venueDB.QueryOne(bgCtx, "events", map[string]interface{}{
			"select": "name,start_datetime,end_datetime,location,address,image,cover_image",
			"where":  map[string]interface{}{"id": eventID},
		}); ev != nil {
			eventName = services.GetString(ev, "name")
			eventImage = services.GetString(ev, "image")
			if eventImage == "" {
				eventImage = services.GetString(ev, "cover_image")
			}
			services.EnrichEvent(ev)
			eventDate = services.GetString(ev, "event_date")
			eventTime = services.GetString(ev, "start_time")
			venueName = services.GetString(ev, "location")
			venueAddress = services.GetString(ev, "address")
		}
		if vCentral, _ := services.DB.Central().QueryOne(bgCtx, "venues", map[string]interface{}{
			"select": "name,address", "where": map[string]interface{}{"id": venueID},
		}); vCentral != nil {
			if venueName == "" {
				venueName = services.GetString(vCentral, "name")
			}
			if venueAddress == "" {
				venueAddress = services.GetString(vCentral, "address")
			}
		}

		// Build per-ticket payload: PDF rows + QR images inline in the HTML.
		pdfTickets := make([]services.TicketPDFData, 0, len(ticketData))
		emailTickets := make([]services.TicketData, 0, len(ticketData))
		for _, td := range ticketData {
			qrToken := services.GetString(td, "qr_token")
			ownerName := services.GetString(td, "owner_name")
			if ln := services.GetString(td, "owner_last_name"); ln != "" {
				ownerName += " " + ln
			}
			ticketID := qrToken // stand-in until the row insert returns an id

			// Render the QR ONCE per ticket; the same PNG feeds both the
			// inline email image and the PDF attachment.
			var qrPNG []byte
			qrDataURL := ""
			if services.PDF != nil {
				if b, err := services.PDF.QRCodePNG(qrToken, 200); err == nil {
					qrPNG = b
					qrDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
				}
			}
			pdfTickets = append(pdfTickets, services.TicketPDFData{
				EventName:     eventName,
				EventDate:     eventDate,
				EventTime:     eventTime,
				VenueName:     venueName,
				VenueLocation: venueAddress,
				TicketType:    ticketTypeName,
				OwnerName:     ownerName,
				OrderNumber:   services.GetString(order, "order_number"),
				TicketID:      ticketID,
				QRCode:        qrToken,
				QRPNG:         qrPNG,
			})
			emailTickets = append(emailTickets, services.TicketData{
				ID:             ticketID,
				Type:           ticketTypeName,
				OwnerName:      ownerName,
				QRCode:         qrToken,
				QRImageDataURL: qrDataURL,
			})
		}

		var pdfBytes []byte
		if services.PDF != nil {
			if b, err := services.PDF.GenerateMultiTicketPDF(pdfTickets); err == nil {
				pdfBytes = b
				log.Printf("[Email/PDF] generated %d bytes for order=%s tickets=%d",
					len(pdfBytes), services.GetString(order, "order_number"), len(pdfTickets))
			} else {
				log.Printf("[Email/PDF] FAILED for order=%s: %v", services.GetString(order, "order_number"), err)
			}
		} else {
			log.Printf("[Email/PDF] services.PDF is nil — InitPDFService never ran")
		}

		totalStr := fmt.Sprintf("%.2f", services.GetFloat64(order, "total"))
		return services.Email.SendTickets(bgCtx, userEmail, services.TicketEmailData{
			OrderNumber:   services.GetString(order, "order_number"),
			CustomerName:  userName,
			EventName:     eventName,
			EventDate:     eventDate,
			EventTime:     eventTime,
			EventImage:    eventImage,
			EventLocation: venueAddress,
			VenueName:     venueName,
			TicketType:    ticketTypeName,
			Currency:      services.GetString(order, "currency"),
			Total:         totalStr,
			Tickets:       emailTickets,
		}, pdfBytes)
	})

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      "Payment confirmed",
		"order_number": services.GetString(order, "order_number"),
		"tickets":      ticketCount,
		"order_id":     orderID,
	})
}

// GetOrder returns an order by code
// GET /api/v1/orders/:code
// OPTIMIZED: Single query with OR condition
func GetOrder(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	code := c.Param("code")
	// SECURITY: block PostgREST operator injection via the path param.
	if !safeLookupCode(code) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid order code"})
		return
	}
	venueID := c.Query("venue_id")

	if code == "" || venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Order code and venue_id are required"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// OPTIMIZATION: Try order_number first (most common), then by id
	order, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,event_id,ticket_type_id,quantity,total,currency,status,user_name,user_email,created_at,paid_at",
		"where": map[string]interface{}{
			"order_number": code,
		},
	})

	if err != nil || order == nil {
		// Fallback: try by id (payment_link_code column doesn't exist in this
		// schema; clients pass either order_number or id).
		order, err = venueDB.QueryOne(ctx, "orders", map[string]interface{}{
			"select": "id,order_number,event_id,ticket_type_id,quantity,total,currency,status,user_name,user_email,created_at,paid_at",
			"where": map[string]interface{}{
				"id": code,
			},
		})

		if err != nil || order == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"order": order,
	})
}

// =============================================
// STAFF ORDER MANAGEMENT
// =============================================

// GetVenueOrders returns all orders for a venue (staff)
// GET /api/v1/orders-admin/venue
func GetVenueOrders(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
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

	// Parse filters
	status := c.Query("status")
	eventID := c.Query("event_id")
	limit := c.DefaultQuery("limit", "50")
	offset := c.DefaultQuery("offset", "0")

	whereClause := make(map[string]interface{})

	if status != "" && status != "all" {
		whereClause["status"] = status
	}
	if eventID != "" {
		whereClause["event_id"] = eventID
	}

	// Query orders
	orders, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,event_id,ticket_type_id,user_id,quantity,total,currency,status,user_name,user_email,payment_gateway,created_at,paid_at",
		"where":  whereClause,
		"order":  "created_at.desc",
		"limit":  limit,
		"offset": offset,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get orders"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"orders": orders,
		"count":  len(orders),
	})
}

// ApproveOrder approves a pending order (for manual approval flow)
// POST /api/v1/orders-admin/:id/approve
func ApproveOrder(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	_ = ctx

	venueID := c.GetString("venue_id")
	staffID := c.GetString("staff_id")
	orderID := c.Param("id")

	if venueID == "" || orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	// RETIRADO: este flujo legacy ponía una orden PENDING (sin pago) en
	// payment_authorized (que significa "fondos retenidos en la pasarela") y
	// escribía columnas que no existen en la BD viva. La aprobación real es
	// POST /orders/:orderId/approve (móvil) sobre órdenes payment_authorized.
	_ = staffID
	_ = venueDB
	_ = orderID
	c.JSON(http.StatusGone, gin.H{
		"error": "Flujo retirado. Las órdenes se aprueban desde la app de staff (requieren pago retenido).",
	})
}

// RejectOrder rejects a pending order
// POST /api/v1/orders-admin/:id/reject
// OPTIMIZED: Parallel order + ticket type fetch
func RejectOrder(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
	staffID := c.GetString("staff_id")
	orderID := c.Param("id")

	if venueID == "" || orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	c.ShouldBindJSON(&req)

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database not available"})
		return
	}

	// Get order
	order, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "ticket_type_id,quantity",
		"where":  map[string]interface{}{"id": orderID, "status": "pending"},
	})

	if order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found or not pending"})
		return
	}

	ticketTypeID := services.GetString(order, "ticket_type_id")
	quantity := services.GetInt(order, "quantity")

	// Update order — solo columnas REALES de la BD viva (rejected_by/
	// rejected_at/reject_reason no existen); quién rechazó va en la razón.
	reason := req.Reason
	if reason == "" {
		reason = "Rechazada por staff"
	}
	result, err := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
		"status":              "cancelled",
		"cancellation_reason": fmt.Sprintf("%s (staff %s)", reason, staffID),
		"cancelled_at":        time.Now().Format(time.RFC3339),
	}, map[string]interface{}{
		"id": orderID,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reject order"})
		return
	}

	// Restore ticket quantity (fire-and-forget): decrement quantity_reserved
	if ticketTypeID != "" && quantity > 0 {
		go func() {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer bgCancel()

			ticketType, _ := venueDB.QueryOne(bgCtx, "ticket_types", map[string]interface{}{
				"select": "quantity_reserved",
				"where":  map[string]interface{}{"id": ticketTypeID},
			})
			if ticketType != nil {
				currentReserved := services.GetInt(ticketType, "quantity_reserved")
				newReserved := currentReserved - quantity
				if newReserved < 0 {
					newReserved = 0
				}
				venueDB.UpdateNoReturn(bgCtx, "ticket_types", map[string]interface{}{
					"quantity_reserved": newReserved,
				}, map[string]interface{}{"id": ticketTypeID})
			}
		}()
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Order rejected",
		"order":   result[0],
	})
}

// =============================================
// WEBHOOKS
// =============================================

// HandleStripeWebhook handles Stripe webhooks with signature validation
// POST /webhooks/stripe/:venue_id
// SECURITY: Validates Stripe-Signature header to prevent webhook forgery
func HandleStripeWebhook(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	venueID := c.Param("venue_id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Venue ID is required"})
		return
	}

	// SECURITY: Get signature header BEFORE reading body
	signature := c.GetHeader("Stripe-Signature")
	if signature == "" {
		log.Printf("[Webhook] SECURITY: Missing Stripe-Signature for venue %s", venueID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing signature header"})
		return
	}

	// Read body
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read body"})
		return
	}

	// Get venue's payment config for webhook validation
	paymentConfig, err := services.Payments.GetGatewayForVenue(ctx, venueID)
	if err != nil {
		log.Printf("[Webhook] Failed to get payment config for venue %s: %v", venueID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get payment config"})
		return
	}

	// SECURITY: Validate webhook secret exists
	webhookSecret := ""
	if paymentConfig.Credentials != nil {
		webhookSecret = paymentConfig.Credentials.StripeWebhookSecret
	}
	if webhookSecret == "" {
		log.Printf("[Webhook] SECURITY: No webhook secret configured for venue %s", venueID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Webhook not configured"})
		return
	}

	// SECURITY: Validate signature using Stripe SDK
	event, err := webhook.ConstructEvent(body, signature, webhookSecret)
	if err != nil {
		log.Printf("[Webhook] SECURITY: Invalid signature for venue %s: %v", venueID, err)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
		return
	}

	// Log validated webhook
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()

		central := services.DB.Central()
		if central != nil {
			central.InsertCtx(bgCtx, "webhook_logs", map[string]interface{}{
				"gateway":    "stripe",
				"venue_id":   venueID,
				"endpoint":   c.Request.URL.Path,
				"method":     c.Request.Method,
				"event_type": event.Type,
				"event_id":   event.ID,
				"validated":  true,
				"processed":  false,
			})
		}
	}()

	// Process validated event
	switch event.Type {
	case "checkout.session.completed":
		handleStripeCheckoutCompleted(ctx, venueID, event.Data.Raw)
	case "payment_intent.succeeded":
		handleStripePaymentSucceeded(ctx, venueID, event.Data.Raw)
	case "payment_intent.payment_failed":
		handleStripePaymentFailed(ctx, venueID, event.Data.Raw)
	case "charge.refunded":
		handleStripeRefund(ctx, venueID, event.Data.Raw)
	default:
		log.Printf("[Webhook] Unhandled Stripe event type: %s", event.Type)
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

// handleStripeCheckoutCompleted processes completed checkout sessions
func handleStripeCheckoutCompleted(ctx context.Context, venueID string, rawData json.RawMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(rawData, &data); err != nil {
		log.Printf("[Webhook] Failed to parse checkout.session.completed: %v", err)
		return
	}

	sessionID := services.GetString(data, "id")
	if sessionID == "" {
		return
	}

	// Find order by Stripe session ID and confirm payment
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		log.Printf("[Webhook] Failed to get venue DB for %s", venueID)
		return
	}

	orders, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
		"select": "id,status",
		"where": map[string]interface{}{
			"stripe_session_id": sessionID,
			"status":            "processing",
		},
	})
	if err != nil || len(orders) == 0 {
		log.Printf("[Webhook] No matching order for session %s", sessionID)
		return
	}

	orderID := services.GetString(orders[0], "id")
	log.Printf("[Webhook] Processing checkout completion for order %s", orderID)

	// Update order status atomically
	_, err = venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
		"status":     "processing", // claim idempotente ("confirming" no existe en el enum)
		"updated_at": time.Now(),
	}, map[string]interface{}{
		"id":     orderID,
		"status": "processing", // Only if still processing (atomic check)
	})
	if err != nil {
		log.Printf("[Webhook] Failed to update order %s: %v", orderID, err)
	}
}

// handleStripePaymentSucceeded processes successful payments
func handleStripePaymentSucceeded(ctx context.Context, venueID string, rawData json.RawMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(rawData, &data); err != nil {
		log.Printf("[Webhook] Failed to parse payment_intent.succeeded: %v", err)
		return
	}

	paymentIntentID := services.GetString(data, "id")
	log.Printf("[Webhook] Payment succeeded: %s for venue %s", paymentIntentID, venueID)
}

// handleStripePaymentFailed processes failed payments
func handleStripePaymentFailed(ctx context.Context, venueID string, rawData json.RawMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(rawData, &data); err != nil {
		log.Printf("[Webhook] Failed to parse payment_intent.payment_failed: %v", err)
		return
	}

	paymentIntentID := services.GetString(data, "id")
	log.Printf("[Webhook] Payment failed: %s for venue %s", paymentIntentID, venueID)
}

// handleStripeRefund processes refund events
func handleStripeRefund(ctx context.Context, venueID string, rawData json.RawMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(rawData, &data); err != nil {
		log.Printf("[Webhook] Failed to parse charge.refunded: %v", err)
		return
	}

	chargeID := services.GetString(data, "id")
	log.Printf("[Webhook] Refund processed: %s for venue %s", chargeID, venueID)
}

// HandleNeoNetWebhook handles NeoNet/Cybersource webhooks with signature validation
// POST /webhooks/neonet/:venue_id
// SECURITY: Validates HMAC-SHA256 signature to prevent webhook forgery
func HandleNeoNetWebhook(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	venueID := c.Param("venue_id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Venue ID is required"})
		return
	}

	// SECURITY: Get signature headers
	signature := c.GetHeader("X-NeoNet-Signature")
	timestamp := c.GetHeader("X-NeoNet-Timestamp")

	if signature == "" {
		log.Printf("[Webhook] SECURITY: Missing X-NeoNet-Signature for venue %s", venueID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing signature header"})
		return
	}

	// SECURITY: Validate timestamp to prevent replay attacks (5 minute window)
	if timestamp != "" {
		ts, err := strconv.ParseInt(timestamp, 10, 64)
		if err == nil {
			webhookTime := time.Unix(ts, 0)
			if time.Since(webhookTime) > 5*time.Minute {
				log.Printf("[Webhook] SECURITY: Timestamp too old for venue %s", venueID)
				c.JSON(http.StatusUnauthorized, gin.H{"error": "Timestamp expired"})
				return
			}
		}
	}

	body, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read body"})
		return
	}

	// Get venue's payment config for webhook validation
	paymentConfig, err := services.Payments.GetGatewayForVenue(ctx, venueID)
	if err != nil {
		log.Printf("[Webhook] Failed to get payment config for venue %s: %v", venueID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get payment config"})
		return
	}

	// SECURITY: Validate webhook using NeoNet secret key
	secretKey := ""
	if paymentConfig.Credentials != nil {
		secretKey = paymentConfig.Credentials.NeoNetSecretKey
	}
	if secretKey == "" {
		log.Printf("[Webhook] SECURITY: No NeoNet secret key configured for venue %s", venueID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Webhook not configured"})
		return
	}

	// SECURITY: Compute expected signature (HMAC-SHA256)
	signaturePayload := timestamp + "." + string(body)
	expectedSig := computeHMACSHA256(signaturePayload, secretKey)

	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		log.Printf("[Webhook] SECURITY: Invalid signature for venue %s", venueID)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
		return
	}

	// Parse validated webhook body
	var webhookData map[string]interface{}
	if err := json.Unmarshal(body, &webhookData); err != nil {
		log.Printf("[Webhook] Failed to parse NeoNet body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	eventType := services.GetString(webhookData, "event_type")
	transactionID := services.GetString(webhookData, "transaction_id")

	// Log validated webhook
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()

		central := services.DB.Central()
		if central != nil {
			central.InsertCtx(bgCtx, "webhook_logs", map[string]interface{}{
				"gateway":        "neonet",
				"venue_id":       venueID,
				"endpoint":       c.Request.URL.Path,
				"method":         c.Request.Method,
				"event_type":     eventType,
				"transaction_id": transactionID,
				"validated":      true,
				"processed":      false,
			})
		}
	}()

	// Process validated event
	switch eventType {
	case "payment.completed", "AUTHORIZED":
		handleNeoNetPaymentCompleted(ctx, venueID, webhookData)
	case "payment.failed", "DECLINED":
		handleNeoNetPaymentFailed(ctx, venueID, webhookData)
	case "refund.completed":
		handleNeoNetRefund(ctx, venueID, webhookData)
	default:
		log.Printf("[Webhook] Unhandled NeoNet event type: %s", eventType)
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

// computeHMACSHA256 computes HMAC-SHA256 signature
func computeHMACSHA256(payload, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}

// handleNeoNetPaymentCompleted processes completed NeoNet payments
func handleNeoNetPaymentCompleted(ctx context.Context, venueID string, data map[string]interface{}) {
	transactionID := services.GetString(data, "transaction_id")
	referenceID := services.GetString(data, "merchant_reference")

	log.Printf("[Webhook] NeoNet payment completed: %s (ref: %s) for venue %s", transactionID, referenceID, venueID)

	if referenceID == "" {
		return
	}

	// Find and update order
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		return
	}

	// Update order status atomically
	_, err := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
		"status":                  "processing", // claim idempotente ("confirming" no existe en el enum)
		"neonet_transaction_id":   transactionID,
		"neonet_authorization_code": services.GetString(data, "authorization_code"),
		"updated_at":              time.Now(),
	}, map[string]interface{}{
		"id":     referenceID,
		"status": "processing",
	})
	if err != nil {
		log.Printf("[Webhook] Failed to update order %s: %v", referenceID, err)
	}
}

// handleNeoNetPaymentFailed processes failed NeoNet payments
func handleNeoNetPaymentFailed(ctx context.Context, venueID string, data map[string]interface{}) {
	transactionID := services.GetString(data, "transaction_id")
	reasonCode := services.GetString(data, "reason_code")
	log.Printf("[Webhook] NeoNet payment failed: %s (reason: %s) for venue %s", transactionID, reasonCode, venueID)
}

// handleNeoNetRefund processes NeoNet refund events
func handleNeoNetRefund(ctx context.Context, venueID string, data map[string]interface{}) {
	transactionID := services.GetString(data, "transaction_id")
	log.Printf("[Webhook] NeoNet refund processed: %s for venue %s", transactionID, venueID)
}

// =============================================
// HELPERS
// =============================================

// generateRandomCode generates a random hex code
func generateRandomCode(length int) (string, error) {
	bytes := make([]byte, length/2)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// recordPlatformTransaction records a transaction in the central database
func recordPlatformTransaction(ctx context.Context, venueID string, order map[string]interface{}, paymentResult *models.PaymentResult) {
	central := services.DB.Central()
	if central == nil {
		return
	}

	// OPTIMIZATION: Parallel venue info + fees fetch
	var venue *models.Venue
	var feePercent, feeFixed float64
	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()
		venue, _ = services.DB.GetVenue(ctx, venueID)
	}()

	go func() {
		defer wg.Done()
		feePercent, feeFixed, _ = services.DB.GetVenueFees(ctx, venueID)
	}()

	wg.Wait()

	grossAmount := services.GetFloat64(order, "total")
	// El total YA incluye el fee (total = subtotal * (1+fee)). El fee real
	// cobrado es total - total/(1+fee) — igual que el split de PayOrder. La
	// fórmula anterior (total*fee) sobreestimaba el fee ~8% en los reportes.
	platformFee := feeFixed
	if feePercent > 0 {
		platformFee += round2(grossAmount - grossAmount/(1+feePercent/100))
	}
	gatewayFee := 0.0
	venueNet := grossAmount - platformFee - gatewayFee

	orgID := ""
	if venue != nil {
		orgID = venue.OrganizationID
	}

	userID := services.GetString(order, "user_id")
	orderID := services.GetString(order, "id")
	eventID := services.GetString(order, "event_id")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}

	now := time.Now()
	tx := &models.Transaction{
		TransactionType:    models.TxTypeIndividualTicket,
		Status:             models.TxStatusCompleted,
		GrossAmount:        grossAmount,
		Currency:           currency,
		PlatformFeePercent: feePercent,
		PlatformFeeAmount:  platformFee,
		GatewayFeeAmount:   gatewayFee,
		NetToVenue:         venueNet,
		VenueID:            venueID,
		OrganizationID:     orgID,
		UserID:             userID,
		PaymentGateway:     paymentResult.Gateway,
		CapturedAt:         &now,
	}

	if eventID != "" {
		tx.EventID = &eventID
	}
	if orderID != "" {
		tx.OrderID = &orderID
	}
	if paymentResult.TransactionID != "" {
		tx.StripePaymentIntent = &paymentResult.TransactionID
	}

	services.DB.RecordTransaction(ctx, tx)
}
