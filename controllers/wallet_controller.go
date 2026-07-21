package controllers

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"

	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// =============================================
// WALLET (web cliente) — endpoints que la web llama y no existían.
// =============================================

// walletVenueID resuelve el venue como el resto de la web single-venue:
// query param si viene, primer venue activo si no.
func walletVenueID(ctx context.Context, c *gin.Context) string {
	venueID := c.Query("venue_id")
	if venueID == "" {
		if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		}); v != nil {
			venueID = services.GetString(v, "id")
		}
	}
	return venueID
}

// DownloadTicketPDF genera el PDF de UN ticket del usuario autenticado y lo
// devuelve como data-URL ({signed_url}) — sin storage de por medio.
// GET /api/v1/tickets/:id/download-pdf  (y alias /:id/pdf)
func DownloadTicketPDF(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	ticketID := c.Param("id")
	userID := c.GetString("user_id")
	if ticketID == "" || userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Ticket ID required"})
		return
	}
	venueID := walletVenueID(ctx, c)
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// holder_id = dueño: nadie descarga tickets ajenos.
	ticket, err := venueDB.QueryOne(ctx, "tickets", map[string]interface{}{
		"select": "id,order_id,event_id,qr_token,ticket_type_name,owner_name,owner_last_name",
		"where":  map[string]interface{}{"id": ticketID, "holder_id": userID},
	})
	if err != nil || ticket == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Ticket not found"})
		return
	}
	if services.PDF == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "PDF service unavailable"})
		return
	}

	eventName, eventDate, eventTime, venueName, venueLoc := "", "", "", "", ""
	if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": "name,start_datetime,end_datetime,location,address",
		"where":  map[string]interface{}{"id": services.GetString(ticket, "event_id")},
	}); ev != nil {
		services.EnrichEvent(ev)
		eventName = services.GetString(ev, "name")
		eventDate = services.GetString(ev, "event_date")
		eventTime = services.GetString(ev, "start_time")
		venueName = services.GetString(ev, "location")
		venueLoc = services.GetString(ev, "address")
	}
	orderNumber := ""
	if o, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "order_number",
		"where":  map[string]interface{}{"id": services.GetString(ticket, "order_id")},
	}); o != nil {
		orderNumber = services.GetString(o, "order_number")
	}

	ownerName := services.GetString(ticket, "owner_name")
	if ln := services.GetString(ticket, "owner_last_name"); ln != "" {
		ownerName += " " + ln
	}
	pdfBytes, err := services.PDF.GenerateTicketPDF(services.TicketPDFData{
		EventName:     eventName,
		EventDate:     eventDate,
		EventTime:     eventTime,
		VenueName:     venueName,
		VenueLocation: venueLoc,
		TicketType:    services.GetString(ticket, "ticket_type_name"),
		OwnerName:     ownerName,
		OrderNumber:   orderNumber,
		TicketID:      services.GetString(ticket, "id"),
		QRCode:        services.GetString(ticket, "qr_token"),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate PDF"})
		return
	}

	dataURL := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdfBytes)
	c.JSON(http.StatusOK, gin.H{"signed_url": dataURL, "pdf_url": dataURL})
}

// GetUserVenueSpending devuelve el gasto del usuario por venue (el wallet lo
// pinta como tarjetas de "total gastado / entradas").
// GET /api/v1/users/spending/venues
func GetUserVenueSpending(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}
	venueID := walletVenueID(ctx, c)
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusOK, gin.H{"spending": []interface{}{}})
		return
	}

	orders, _ := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
		"select": "total,quantity,currency",
		"where":  map[string]interface{}{"user_id": userID, "status": "confirmed"},
	})
	if len(orders) == 0 {
		c.JSON(http.StatusOK, gin.H{"spending": []interface{}{}})
		return
	}
	totalSpent, totalTickets := 0.0, 0
	currency := "GTQ"
	for _, o := range orders {
		totalSpent += services.GetFloat64(o, "total")
		totalTickets += services.GetInt(o, "quantity")
		if cur := services.GetString(o, "currency"); cur != "" {
			currency = cur
		}
	}
	venueName, venueLocation := "", ""
	if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
		"select": "name,address", "where": map[string]interface{}{"id": venueID},
	}); v != nil {
		venueName = services.GetString(v, "name")
		venueLocation = services.GetString(v, "address")
	}
	c.JSON(http.StatusOK, gin.H{"spending": []gin.H{{
		"id": venueID,
		"venue": gin.H{
			"id":       venueID,
			"name":     venueName,
			"location": venueLocation,
			"currency": currency,
		},
		"total_spent":   round2(totalSpent),
		"total_tickets": totalTickets,
	}}})
}

// GetCancelledOrderData devuelve los datos del formulario de una orden
// cancelada para repoblar el checkout ("volver a intentarlo").
// GET /api/v1/orders/cancelled/:orderId
func GetCancelledOrderData(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	orderID := c.Param("orderId")
	if !safeLookupCode(orderID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid order id"})
		return
	}
	venueID := walletVenueID(ctx, c)
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	order, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,event_id,ticket_type_id,quantity,metadata",
		"where":  map[string]interface{}{"id": orderID},
	})
	if err != nil || order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}
	metadata, _ := order["metadata"].(map[string]interface{})
	var ticketsData interface{}
	if metadata != nil {
		ticketsData = metadata["tickets_data"]
	}
	c.JSON(http.StatusOK, gin.H{
		"order_data": gin.H{
			"event_id":       services.GetString(order, "event_id"),
			"ticket_type_id": services.GetString(order, "ticket_type_id"),
			"quantity":       services.GetInt(order, "quantity"),
			"tickets_data":   ticketsData,
		},
	})
}
