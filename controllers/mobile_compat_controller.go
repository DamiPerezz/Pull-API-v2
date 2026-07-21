package controllers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"pull-api-v2/services"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// =============================================
// MOBILE APP COMPAT LAYER
// =============================================
//
// PullMobileApp-GL was built against Pull-API-Go (v1). It expects path
// shapes like:
//
//   GET  /event/upcoming-events/:venueId
//   GET  /event/get-event-details/:eventId
//   POST /ticket-validation/validate-ticket
//   GET  /orders/venue/:venueId
//   POST /orders/:orderId/approve
//   POST /orders/:orderId/reject
//   GET  /group-reservations/details/:reservationId
//   POST /group-reservations/:id/approve
//   POST /group-reservations/:id/reject
//   GET  /venue/get-venue-info/:venueId
//   POST /notifications/register-token
//   POST /notifications/unregister-token
//
// We adapt these to the v2 lookup logic + multi-tenant DatabaseRouter.

// MobileGetUpcomingEvents handles GET /event/upcoming-events/:venueId
func MobileGetUpcomingEvents(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.Param("venueId")

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	events, err := venueDB.QueryCtx(ctx, "events", map[string]interface{}{
		"select": services.EventSelectColumns,
		"where": map[string]interface{}{
			"status":         services.PublishedEventStatuses,
			"deleted_at":     "is.null",
			"start_datetime": "gte." + time.Now().Format(time.RFC3339),
		},
		"order": "start_datetime.asc",
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get events"})
		return
	}
	services.EnrichEvents(events)
	for i := range events {
		events[i]["venue_id"] = venueID
	}
	// PullMobileApp-GL expects a bare array — eventService maps response.data
	// directly to its store. If we returned {events: [...]} the store would
	// receive an object and silently render no events.
	c.JSON(http.StatusOK, events)
}

// MobileCreateEventWithTickets handles POST /event/create-event-with-tickets.
// Body matches the EventoNuevo screen — name/description/event_date/start_time/
// end_time/ticket_limit/dress_code/min_age/custom_location/ticket_types[].
func MobileCreateEventWithTickets(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	venueID := c.GetString("venue_id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id missing in token"})
		return
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	startDT, endDT := combineDateTime(
		services.GetString(body, "event_date"),
		services.GetString(body, "start_time"),
		services.GetString(body, "end_time"),
	)
	if startDT == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "event_date and start_time required"})
		return
	}
	name := services.GetString(body, "name")
	slug := slugify(name) + "-" + time.Now().Format("20060102150405")

	// A "private" event needs staff approval before the ticket is issued; we
	// tie is_private and require_approval together (one toggle in the app).
	private := services.GetBool(body, "is_private") || services.GetBool(body, "require_approval")

	// The venue DB is single-tenant: events carries no venue_id or
	// organization_id column (tenancy is the database itself).
	insertPayload := map[string]interface{}{
		"name":           name,
		"description":    services.GetString(body, "description"),
		"slug":           slug,
		"start_datetime": startDT,
		"end_datetime":   endDT,
		// The staff app has no separate publish step — a created event must
		// show up in the (published-only) event lists right away.
		"status":     "published",
		"image":      services.GetString(body, "image"),
		"dress_code": services.GetString(body, "dress_code"),
		"min_age":    services.GetInt(body, "min_age"),
		// The app calls it custom_location; the events schema column is location.
		"location":         services.GetString(body, "custom_location"),
		"capacity":         services.GetInt(body, "ticket_limit"),
		"is_private":       private,
		"require_approval": private,
	}
	// table_capacity has no backing column in events — tables are the
	// event_vip_ticket_types rows. Accepted in the payload but not persisted.

	created, err := venueDB.InsertCtx(ctx, "events", insertPayload)
	if err != nil || created == nil {
		log.Printf("[Mobile/CreateEvent] insert err=%v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create event", "details": fmt.Sprintf("%v", err)})
		return
	}
	eventID := services.GetString(created, "id")

	// Insert ticket types if provided.
	tts, _ := body["ticket_types"].([]interface{})
	createdTickets := []map[string]interface{}{}
	for _, t := range tts {
		tt, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		var price float64
		switch v := tt["price"].(type) {
		case float64:
			price = v
		case int:
			price = float64(v)
		case string:
			var p float64
			fmt.Sscanf(v, "%f", &p)
			price = p
		}
		quantity := services.GetInt(tt, "quantity")
		if quantity == 0 {
			quantity = services.GetInt(tt, "initialQuantity")
		}
		ttPayload := map[string]interface{}{
			"event_id":       eventID,
			"name":           services.GetString(tt, "name"),
			"price":          price,
			"quantity_total": quantity,
			"benefits":       splitBenefits(services.GetString(tt, "benefits")),
			"min_per_order":  1,
			"max_per_order":  10,
			"is_active":      true,
			"is_visible":     true,
		}
		row, terr := venueDB.InsertCtx(ctx, "ticket_types", ttPayload)
		if terr == nil && row != nil {
			createdTickets = append(createdTickets, row)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"event_id":     eventID,
		"event":        created,
		"ticket_types": createdTickets,
	})
}

// MobileCreateEvent handles POST /event/create-event (no ticket types).
func MobileCreateEvent(c *gin.Context) {
	// Same impl as CreateEventWithTickets but ignores ticket_types.
	MobileCreateEventWithTickets(c)
}

// MobileUpdateEvent handles PUT /event/update-event/:eventId.
func MobileUpdateEvent(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.GetString("venue_id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}
	updates := map[string]interface{}{}
	for _, k := range []string{"name", "description", "image", "dress_code", "status", "deleted_at"} {
		if v, ok := body[k]; ok {
			updates[k] = v
		}
	}
	// The app calls it custom_location; the events schema column is location.
	if v, ok := body["custom_location"]; ok {
		updates["location"] = v
	}
	if v, ok := body["min_age"]; ok {
		updates["min_age"] = v
	}
	if v, ok := body["ticket_limit"]; ok {
		updates["capacity"] = v
	}
	// Private/approval toggle (tied together).
	if _, ok := body["is_private"]; ok {
		private := services.GetBool(body, "is_private") || services.GetBool(body, "require_approval")
		updates["is_private"] = private
		updates["require_approval"] = private
	}
	// table_capacity has no backing column in events; ignore it (tables are
	// managed as event_vip_ticket_types rows).
	startDT, endDT := combineDateTime(
		services.GetString(body, "event_date"),
		services.GetString(body, "start_time"),
		services.GetString(body, "end_time"),
	)
	if startDT != "" {
		updates["start_datetime"] = startDT
	}
	if endDT != "" {
		updates["end_datetime"] = endDT
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no updates supplied"})
		return
	}
	result, err := venueDB.UpdateCtx(ctx, "events", updates, map[string]interface{}{
		"id": c.Param("eventId"),
	})
	if err != nil || len(result) == 0 {
		log.Printf("[Mobile/UpdateEvent] err=%v rows=%d", err, len(result))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update event"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "event": result[0]})
}

// MobileDeleteEvent handles DELETE /event/delete-event/:eventId (soft delete).
func MobileDeleteEvent(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.GetString("venue_id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	now := time.Now().Format(time.RFC3339)
	result, err := venueDB.UpdateCtx(ctx, "events", map[string]interface{}{
		"deleted_at": now,
		"status":     "cancelled",
	}, map[string]interface{}{"id": c.Param("eventId")})
	if err != nil || len(result) == 0 {
		log.Printf("[Mobile/DeleteEvent] err=%v rows=%d", err, len(result))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete event"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// combineDateTime takes "2026-07-03" + "22:00:00" / "03:00:00" and returns
// RFC3339 start/end datetimes. End times before start times roll over to the
// next day (e.g. doors at 22:00, closing at 03:00 the next morning).
func combineDateTime(eventDate, startTime, endTime string) (string, string) {
	if eventDate == "" || startTime == "" {
		return "", ""
	}
	// Normalise time to HH:MM:SS
	if len(startTime) == 5 {
		startTime += ":00"
	}
	if endTime != "" && len(endTime) == 5 {
		endTime += ":00"
	}
	startStr := eventDate + "T" + startTime + "-06:00"
	if endTime == "" {
		return startStr, ""
	}
	// Same date for end; if end < start hour, roll one day forward.
	endStr := eventDate + "T" + endTime + "-06:00"
	if endTime < startTime {
		t, err := time.Parse("2006-01-02", eventDate)
		if err == nil {
			endStr = t.Add(24*time.Hour).Format("2006-01-02") + "T" + endTime + "-06:00"
		}
	}
	return startStr, endStr
}

// slugify lowercases, replaces spaces with -, and strips non-alphanumeric chars.
func slugify(s string) string {
	out := strings.Builder{}
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == ' ', r == '-', r == '_':
			out.WriteRune('-')
		}
	}
	res := out.String()
	if res == "" {
		res = "event"
	}
	return res
}

// MobileGetEventDetails handles GET /event/get-event-details/:eventId
func MobileGetEventDetails(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	eventID := c.Param("eventId")

	// Try to find the event across the staff's venue (taken from JWT claims).
	venueID := c.GetString("venue_id")
	if venueID == "" {
		v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		})
		if v != nil {
			venueID = services.GetString(v, "id")
		}
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Accept either UUID or slug.
	event, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": services.EventSelectColumns,
		"where":  map[string]interface{}{"id": eventID, "deleted_at": "is.null"},
	})
	if event == nil {
		event, _ = venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": services.EventSelectColumns,
			"where":  map[string]interface{}{"slug": eventID, "deleted_at": "is.null"},
		})
	}
	if event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}
	services.EnrichEvent(event)
	event["venue_id"] = venueID

	// table_capacity has no events column — report the real table count: the
	// sum of VIP table types configured for this event.
	if vips, err := venueDB.QueryCtx(ctx, "event_vip_ticket_types", map[string]interface{}{
		"select": "quantity_total",
		"where":  map[string]interface{}{"event_id": services.GetString(event, "id")},
	}); err == nil {
		tables := 0
		for _, vt := range vips {
			tables += services.GetInt(vt, "quantity_total")
		}
		event["table_capacity"] = tables
	}

	// PullMobileApp-GL's eventService reads response.data directly, expecting
	// top-level event fields. Wrapping in {event: ...} would leave the UI
	// blank because currentEvent.name etc. would be undefined.
	c.JSON(http.StatusOK, event)
}

// MobileGetVenueInfo handles GET /venue/get-venue-info/:venueId
func MobileGetVenueInfo(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
		"select": "*",
		"where":  map[string]interface{}{"id": c.Param("venueId"), "deleted_at": "is.null"},
	})
	if v == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	enrichVenueForLegacyFrontend(v)
	c.JSON(http.StatusOK, v)
}

// MobileValidateTicket handles POST /ticket-validation/validate-ticket
// Body: { qr_token, venue_id, organization_id, worker_id }
func MobileValidateTicket(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	var req struct {
		QRToken        string `json:"qr_token"`
		VenueID        string `json:"venue_id"`
		OrganizationID string `json:"organization_id"`
		WorkerID       string `json:"worker_id"`
	}
	_ = c.ShouldBindJSON(&req)
	if req.QRToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "qr_token is required", "valid": false})
		return
	}
	// SECURITY: a scanned QR is untrusted input — block PostgREST operator
	// injection before it reaches the filter.
	if !safeLookupCode(req.QRToken) {
		c.JSON(http.StatusOK, gin.H{"valid": false, "message": "Ticket no encontrado"})
		return
	}
	venueID := req.VenueID
	if venueID == "" {
		venueID = c.GetString("venue_id")
	}
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id is required", "valid": false})
		return
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue", "valid": false})
		return
	}

	ticket, _ := venueDB.QueryOne(ctx, "tickets", map[string]interface{}{
		"select": "id,qr_token,event_id,order_id,ticket_type_name,owner_name,owner_email,owner_phone,is_valid,checked_in_at,checked_in_by",
		"where":  map[string]interface{}{"qr_token": req.QRToken},
	})
	if ticket == nil {
		c.JSON(http.StatusOK, gin.H{
			"valid":   false,
			"message": "Ticket no encontrado",
		})
		return
	}
	if !services.GetBool(ticket, "is_valid") {
		c.JSON(http.StatusOK, gin.H{
			"valid":   false,
			"message": "Ticket inválido",
			"ticket":  ticket,
		})
		return
	}
	if checkedAt := services.GetString(ticket, "checked_in_at"); checkedAt != "" {
		c.JSON(http.StatusOK, gin.H{
			"valid":         false,
			"already_used":  true,
			"checked_in_at": checkedAt,
			"message":       "Ticket ya fue usado",
			"ticket":        ticket,
		})
		return
	}

	// Mark as checked in ATOMICALLY: the conditional update is the guard
	// against two doormen scanning the same QR in the same instant — only
	// the request that flips checked_in_at from NULL wins.
	ticketID := services.GetString(ticket, "id")
	// checked_in_by es uuid: sin worker en el body, usar el staff del JWT;
	// si tampoco hay, dejarlo NULL (un "" rompe el cast a uuid).
	workerID := req.WorkerID
	if workerID == "" {
		workerID = c.GetString("user_id")
	}
	var checkedInBy interface{}
	if workerID != "" {
		checkedInBy = workerID
	}
	lock, lockErr := venueDB.UpdateCtx(ctx, "tickets", map[string]interface{}{
		"checked_in_at": time.Now().Format(time.RFC3339),
		"checked_in_by": checkedInBy,
	}, map[string]interface{}{"id": ticketID, "checked_in_at": "is.null"})
	if lockErr != nil {
		// Fallo de BD/red, NO un duplicado — decirlo claro para que el
		// portero reintente en vez de rechazar a la persona.
		log.Printf("[Scan] claim ERROR venue=%s ticket=%s worker=%s: %v", venueID, ticketID, req.WorkerID, lockErr)
		c.JSON(http.StatusInternalServerError, gin.H{
			"valid":   false,
			"message": "No se pudo validar el ticket. Reintenta.",
		})
		return
	}
	if len(lock) == 0 {
		log.Printf("[Scan] DUPLICATE race venue=%s ticket=%s worker=%s", venueID, ticketID, req.WorkerID)
		c.JSON(http.StatusOK, gin.H{
			"valid":        false,
			"already_used": true,
			"message":      "Ticket ya fue usado",
			"ticket":       ticket,
		})
		return
	}
	log.Printf("[Scan] OK venue=%s ticket=%s owner=%q worker=%s", venueID, ticketID, services.GetString(ticket, "owner_name"), req.WorkerID)

	// Enrich event info for the staff UI.
	var event map[string]interface{}
	if eid := services.GetString(ticket, "event_id"); eid != "" {
		event, _ = venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "name,start_datetime,location",
			"where":  map[string]interface{}{"id": eid},
		})
	}

	// The app reads event_name/ticket_type/status at the top level
	// (ticketService.js), not nested under ticket/event.
	c.JSON(http.StatusOK, gin.H{
		"valid":       true,
		"message":     "Acceso permitido",
		"status":      "checked_in",
		"event_name":  services.GetString(event, "name"),
		"ticket_type": services.GetString(ticket, "ticket_type_name"),
		"owner_name":  services.GetString(ticket, "owner_name"),
		"ticket":      ticket,
		"event":       event,
	})
}

// MobileGetVenueOrders handles GET /orders/venue/:venueId
// Query params: page, limit, status
func MobileGetVenueOrders(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.Param("venueId")
	// SECURITY: staff can only read their own venue (IDOR guard).
	if tokenVenue := c.GetString("venue_id"); tokenVenue != "" && venueID != tokenVenue {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied for this venue"})
		return
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	where := map[string]interface{}{}
	if status := c.Query("status"); status != "" && status != "All" {
		where["status"] = status
	}
	limit := 20
	if l := c.Query("limit"); l != "" {
		_, _ = fmtSscanInt(l, &limit)
	}
	page := 1
	if p := c.Query("page"); p != "" {
		_, _ = fmtSscanInt(p, &page)
	}
	offset := (page - 1) * limit
	if offset < 0 {
		offset = 0
	}

	orders, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
		"select": "*",
		"where":  where,
		"order":  "created_at.desc",
		"limit":  limit,
		"offset": offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get orders"})
		return
	}
	// Total count for pagination — HEAD + count=exact, no rows transferred.
	totalCount, cntErr := venueDB.CountCtx(ctx, "orders", where)
	if cntErr != nil {
		// Fallback: page-sized guess (better a soft count than a 500 here).
		totalCount = offset + len(orders)
	}
	totalPages := (totalCount + limit - 1) / limit
	if totalPages < 1 {
		totalPages = 1
	}
	c.JSON(http.StatusOK, gin.H{
		"orders": orders,
		"pagination": gin.H{
			"currentPage": page,
			"totalPages":  totalPages,
			"totalCount":  totalCount,
			"hasMore":     page < totalPages,
			"limit":       limit,
		},
	})
}

// MobileGetOrderDetails handles GET /orders/details/:orderId
func MobileGetOrderDetails(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.GetString("venue_id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id missing"})
		return
	}
	venueDB := services.DB.ForVenue(venueID)
	order, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"id": c.Param("orderId")},
	})
	if order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": order})
}

// MobileApproveOrder handles POST /orders/:orderId/approve. Idempotent: a
// request against an already-confirmed order succeeds with the current row.
func MobileApproveOrder(c *gin.Context) {
	// 60s, no 10s: aprobar un evento privado hace DOS capturas Cybersource
	// (venue + fee) que pueden tardar hasta 30s cada una — un ctx corto
	// abortaba con la captura ya ejecutada en la pasarela.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	// Aprobar CAPTURA dinero retenido: solo admin/manager, no un portero.
	if role := c.GetString("role"); role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Solo un admin o manager puede aprobar cobros"})
		return
	}
	venueID := c.GetString("venue_id")
	if venueID == "" {
		v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		})
		if v != nil {
			venueID = services.GetString(v, "id")
		}
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	orderID := c.Param("orderId")

	current, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,status,stripe_session_id,currency,metadata", "where": map[string]interface{}{"id": orderID},
	})
	if current == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}
	if services.GetString(current, "status") == "confirmed" {
		c.JSON(http.StatusOK, gin.H{"success": true, "order": current, "already_approved": true})
		return
	}

	// Guard: an expired/cancelled order cannot be approved (its hold is gone).
	if st := services.GetString(current, "status"); st == "expired" || st == "cancelled" {
		c.JSON(http.StatusConflict, gin.H{"error": "Esta solicitud ya no se puede aprobar (" + st + ")"})
		return
	}
	// RACE GUARD: for held orders, atomically claim payment_authorized→processing
	// so a concurrent reject or the 48h expiry job can't act on the same hold.
	if services.GetString(current, "status") == "payment_authorized" {
		if !claimHeldOrder(ctx, venueDB, orderID) {
			c.JSON(http.StatusConflict, gin.H{"error": "Esta solicitud ya está siendo procesada"})
			return
		}
	}

	// PRIVATE-EVENT APPROVAL: if this order holds Cybersource authorizations
	// (payment_authorized), settle them now (capture the money) and then issue
	// tickets via the shared confirmation rail.
	if sid, outcome := captureHeldOrder(ctx, venueID, current); outcome != notHeld {
		if outcome == captureFailed {
			// The gateway refused the capture (e.g. auth expired issuer-side).
			// Leave the order as-is so staff can retry or reject.
			c.JSON(http.StatusBadGateway, gin.H{
				"error": "No se pudo cobrar el pago retenido. La autorización pudo expirar; intenta de nuevo o rechaza la solicitud.",
			})
			return
		}
		// Persist the captured flag + WHO approved and WHEN. Trazabilidad va en
		// metadata: la tabla orders VIVA de 511 no tiene columnas
		// approved_by/approved_at (el template sí — divergencia de esquema), y
		// una columna fantasma tumba el UPDATE entero: la captura ya se hizo
		// pero stripe_session_id no se guardaba y el ticket nunca se emitía.
		metadata, _ := current["metadata"].(map[string]interface{})
		if metadata == nil {
			metadata = map[string]interface{}{}
		}
		metadata["captured_at"] = time.Now().Format(time.RFC3339)
		metadata["approved_by"] = c.GetString("staff_id")
		metadata["approved_at"] = time.Now().Format(time.RFC3339)
		if err := venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
			"status":            "processing",
			"stripe_session_id": sid,
			"metadata":          metadata,
		}, map[string]interface{}{"id": orderID}); err != nil {
			// Ruta de dinero: la captura YA se ejecutó en la pasarela. Si no
			// podemos persistirla, gritar — no seguir como si nada.
			log.Printf("[Approve] ALERT: capture OK but order update FAILED order=%s session=%s: %v", orderID, sid, err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "El cobro se realizó pero no se pudo actualizar la orden. NO reintentes; avisa a soporte técnico.",
			})
			return
		}
		q := c.Request.URL.Query()
		q.Set("session_id", sid)
		q.Set("venue_id", venueID)
		c.Request.URL.RawQuery = q.Encode()
		ConfirmPayment(c)
		return
	}

	// No held gateway authorization to settle. With real payments there is
	// nothing to "approve into" a paid order — this is a stale/unpaid order
	// (e.g. an abandoned pending order or leftover test data). Return a clear
	// message instead of a 500 from trying to confirm a non-existent payment.
	sessionID := services.GetString(current, "stripe_session_id")
	if sessionID == "" || !strings.HasPrefix(sessionID, "neonet_") {
		c.JSON(http.StatusConflict, gin.H{
			"error": "Esta orden no tiene un pago pendiente de aprobar (no se completó el pago o es una orden antigua).",
		})
		return
	}
	q := c.Request.URL.Query()
	q.Set("session_id", sessionID)
	q.Set("venue_id", venueID)
	c.Request.URL.RawQuery = q.Encode()
	ConfirmPayment(c)
}

// MobileRejectOrder handles POST /orders/:orderId/reject
func MobileRejectOrder(c *gin.Context) {
	// 45s: rechazar libera DOS autorizaciones contra la pasarela (venue+fee).
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	// Rechazar mueve dinero (reversa de retención): solo admin/manager.
	if role := c.GetString("role"); role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Solo un admin o manager puede rechazar solicitudes"})
		return
	}
	venueID := c.GetString("venue_id")
	staffID := c.GetString("staff_id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id missing"})
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&body)

	venueDB := services.DB.ForVenue(venueID)
	reason := body.Reason
	// orders has no rejected_by column; keep the acting staff in the reason
	// trail instead.
	if staffID != "" {
		reason = strings.TrimSpace(reason + " (rechazada por staff " + staffID + ")")
	}

	// PRIVATE-EVENT REJECTION: release any held Cybersource authorizations so
	// the buyer is NOT charged (both the ticket and our fee are freed), and
	// email them that the request was declined and the hold released.
	orderID := c.Param("orderId")
	if held, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,event_id,status,currency,total,user_name,user_email,metadata", "where": map[string]interface{}{"id": orderID},
	}); held != nil {
		// RACE GUARD: claim the hold before releasing so approve/expiry can't
		// also act on it. Non-held (pending) orders skip the claim.
		if services.GetString(held, "status") == "payment_authorized" && !claimHeldOrder(ctx, venueDB, orderID) {
			c.JSON(http.StatusConflict, gin.H{"error": "Esta solicitud ya está siendo procesada"})
			return
		}
		if wasHeld, released := reverseHeldOrder(ctx, venueID, held); wasHeld {
			log.Printf("[Mobile/RejectOrder] held authorizations release attempted order=%s released=%v", orderID, released)
			cur := services.GetString(held, "currency")
			if cur == "" {
				cur = "GTQ"
			}
			go func() {
				bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer bgCancel()
				sendApprovalStatusEmail(bgCtx, venueID, held, services.GetFloat64(held, "total"), cur, "rejected", false)
			}()
			if !released {
				// El rechazo sigue adelante (la auth caduca sola en el emisor),
				// pero el staff debe saber que la liberación no se confirmó.
				defer func() {
					log.Printf("[Mobile/RejectOrder] order=%s cancelled with UNCONFIRMED reversal", orderID)
				}()
			}
		}
	}

	result, err := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
		"status":              "cancelled",
		"cancelled_at":        time.Now().Format(time.RFC3339),
		"cancellation_reason": reason,
	}, map[string]interface{}{"id": orderID})
	if err != nil || len(result) == 0 {
		log.Printf("[Mobile/RejectOrder] failed id=%s err=%v rows=%d", c.Param("orderId"), err, len(result))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reject"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "order": result[0]})
}

// MobileGetGroupReservationDetails handles GET /group-reservations/details/:reservationId
func MobileGetGroupReservationDetails(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.GetString("venue_id")
	if venueID == "" {
		v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		})
		if v != nil {
			venueID = services.GetString(v, "id")
		}
	}
	venueDB := services.DB.ForVenue(venueID)
	resv, _ := venueDB.QueryOne(ctx, "vip_list_reservations", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"id": c.Param("reservationId")},
	})
	if resv == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Reservation not found"})
		return
	}
	resvID := services.GetString(resv, "id")
	guests, _ := venueDB.QueryCtx(ctx, "vip_list_guests", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"reservation_id": resvID},
	})
	bottles, _ := venueDB.QueryCtx(ctx, "vip_list_bottles", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"reservation_id": resvID},
	})
	if guests == nil {
		guests = []map[string]interface{}{}
	}
	if bottles == nil {
		bottles = []map[string]interface{}{}
	}

	// GroupReservaDetalle reads a flat object under `data`: status_name,
	// total_amount, guest_count, pending_amount, event/venue names and the
	// guests embedded.
	pending := 0.0
	for _, g := range guests {
		if !services.GetBool(g, "has_paid") {
			pending += services.GetFloat64(g, "ticket_price")
		}
	}
	eventName, venueName := "", ""
	if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": "name,location",
		"where":  map[string]interface{}{"id": services.GetString(resv, "event_id")},
	}); ev != nil {
		eventName = services.GetString(ev, "name")
		venueName = services.GetString(ev, "location")
	}
	currency := services.GetString(resv, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	guestCount := len(guests)
	if guestCount == 0 {
		guestCount = services.GetInt(resv, "max_guests")
	}

	flat := map[string]interface{}{}
	for k, v := range resv {
		flat[k] = v
	}
	flat["status_name"] = services.GetString(resv, "status")
	flat["total_amount"] = services.GetFloat64(resv, "total")
	flat["pending_amount"] = pending
	flat["guest_count"] = guestCount
	flat["event_name"] = eventName
	flat["venue_name"] = venueName
	flat["currency"] = currency
	flat["guests"] = guests
	flat["bottles"] = bottles

	c.JSON(http.StatusOK, gin.H{
		"data":        flat,
		"reservation": resv,
		"guests":      guests,
		"bottles":     bottles,
	})
}

// MobileApproveGroupReservation handles POST /group-reservations/:id/approve
func MobileApproveGroupReservation(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.GetString("venue_id")
	staffID := c.GetString("staff_id")
	if venueID == "" {
		v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		})
		if v != nil {
			venueID = services.GetString(v, "id")
		}
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	id := c.Param("id")
	result, err := venueDB.UpdateCtx(ctx, "vip_list_reservations", map[string]interface{}{
		"status":      "confirmed",
		"approved_at": time.Now().Format(time.RFC3339),
		"approved_by": nullableEnum(staffID),
	}, map[string]interface{}{"id": id})
	if err != nil || len(result) == 0 {
		log.Printf("[Mobile/ApproveGroupReservation] failed venue=%s id=%s err=%v rows=%d", venueID, id, err, len(result))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to approve", "details": fmt.Sprintf("%v", err)})
		return
	}
	resv := result[0]

	// Send the organizer the approval email with the shared group payment
	// link (fire-and-forget so the staff app isn't blocked on Brevo).
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer bgCancel()
		if services.Email == nil {
			return
		}
		orgEmail := services.GetString(resv, "organizer_email")
		if orgEmail == "" {
			log.Printf("[Mobile/ApproveGroupReservation] no organizer_email for %s — approval email skipped", id)
			return
		}
		reservationNumber := services.GetString(resv, "reservation_number")
		code := strings.TrimPrefix(reservationNumber, "GRP-")

		eventName, eventDate := "", ""
		if ev, _ := venueDB.QueryOne(bgCtx, "events", map[string]interface{}{
			"select": "name,start_datetime,end_datetime",
			"where":  map[string]interface{}{"id": services.GetString(resv, "event_id")},
		}); ev != nil {
			eventName = services.GetString(ev, "name")
			services.EnrichEvent(ev)
			eventDate = services.GetString(ev, "event_date")
		}

		hostPaid := 0
		guestCount := services.GetInt(resv, "max_guests")
		if guests, _ := venueDB.QueryCtx(bgCtx, "vip_list_guests", map[string]interface{}{
			"select": "id,has_paid,paid_at",
			"where":  map[string]interface{}{"reservation_id": id},
		}); len(guests) > 0 {
			guestCount = len(guests)
			for _, g := range guests {
				if services.GetBool(g, "has_paid") && services.GetString(g, "paid_at") == "" {
					hostPaid++
				}
			}
		}

		if err := services.Email.SendGroupReservationApproved(bgCtx, services.GroupReservationApprovedData{
			To:                  orgEmail,
			OrganizerName:       services.GetString(resv, "organizer_name"),
			EventName:           eventName,
			EventDate:           eventDate,
			ReservationNumber:   reservationNumber,
			PaymentLinkCode:     code,
			GuestCount:          guestCount,
			HostPaidGuestsCount: hostPaid,
		}); err != nil {
			log.Printf("[Mobile/ApproveGroupReservation] approval email failed for %s: %v", id, err)
		}
	}()

	c.JSON(http.StatusOK, gin.H{"success": true, "reservation": resv})
}

// MobileRejectGroupReservation handles POST /group-reservations/:id/reject
func MobileRejectGroupReservation(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.GetString("venue_id")
	staffID := c.GetString("staff_id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id missing"})
		return
	}
	venueDB := services.DB.ForVenue(venueID)
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&body)
	result, err := venueDB.UpdateCtx(ctx, "vip_list_reservations", map[string]interface{}{
		"status": "cancelled",
		"notes":  body.Reason,
	}, map[string]interface{}{"id": c.Param("id")})
	if err != nil || len(result) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reject"})
		return
	}
	_ = staffID
	c.JSON(http.StatusOK, gin.H{"success": true, "reservation": result[0]})
}

// MobileRegisterPushToken handles POST /notifications/register-token
// Body: { push_token, venue_id, employee_id, device_type }
func MobileRegisterPushToken(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	var req struct {
		PushToken  string `json:"push_token"`
		VenueID    string `json:"venue_id"`
		EmployeeID string `json:"employee_id"`
		DeviceType string `json:"device_type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.PushToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "push_token is required"})
		return
	}
	// SECURITY: venue and employee come from the authenticated JWT, never the
	// body — otherwise anyone could subscribe to another venue's staff pushes.
	venueID := c.GetString("venue_id")
	employeeID := c.GetString("staff_id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	// Upsert: check if a row with this push_token already exists.
	existing, _ := venueDB.QueryOne(ctx, "staff_push_tokens", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"push_token": req.PushToken},
	})
	if existing != nil {
		venueDB.UpdateNoReturn(ctx, "staff_push_tokens", map[string]interface{}{
			"is_active":   true,
			"device_type": req.DeviceType,
			"employee_id": employeeID,
		}, map[string]interface{}{"id": services.GetString(existing, "id")})
	} else {
		venueDB.InsertCtx(ctx, "staff_push_tokens", map[string]interface{}{
			"employee_id": employeeID,
			"push_token":  req.PushToken,
			"device_type": req.DeviceType,
			"is_active":   true,
		})
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// MobileUnregisterPushToken handles POST /notifications/unregister-token
func MobileUnregisterPushToken(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	var req struct {
		PushToken string `json:"push_token"`
		VenueID   string `json:"venue_id"`
	}
	_ = c.ShouldBindJSON(&req)
	if req.PushToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "push_token is required"})
		return
	}
	venueID := req.VenueID
	if venueID == "" {
		venueID = c.GetString("venue_id")
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	venueDB.UpdateNoReturn(ctx, "staff_push_tokens", map[string]interface{}{
		"is_active": false,
	}, map[string]interface{}{"push_token": req.PushToken})
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// fmtSscanInt is a tiny helper that wraps fmt.Sscan so we can swap libs later
// without changing every caller.
func fmtSscanInt(s string, out *int) (int, error) {
	if s == "" {
		return 0, nil
	}
	v := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, nil
		}
		v = v*10 + int(ch-'0')
	}
	*out = v
	return v, nil
}

// resolveVenueID returns the venue id stored on the JWT context, or the first
// active venue if none is set (mobile sometimes calls public endpoints before
// staff context is established).
func resolveVenueID(c *gin.Context) string {
	if vid := c.GetString("venue_id"); vid != "" {
		return vid
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 4*time.Second)
	defer cancel()
	v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
	})
	if v != nil {
		return services.GetString(v, "id")
	}
	return ""
}

// MobileGetPendingGuestLists handles GET /guest-lists/venue/:venueId/pending.
// The `guest_list_signups` table doesn't have `venue_id` directly; we look up
// pending signups by event ids belonging to the venue.
func MobileGetPendingGuestLists(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.Param("venueId")
	if venueID == "" {
		venueID = resolveVenueID(c)
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	events, _ := venueDB.QueryCtx(ctx, "events", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{},
	})
	eventIDs := make([]string, 0, len(events))
	for _, e := range events {
		eventIDs = append(eventIDs, services.GetString(e, "id"))
	}
	if len(eventIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"signups": []interface{}{}, "count": 0})
		return
	}
	signups, err := venueDB.QueryCtx(ctx, "guest_list_signups", map[string]interface{}{
		"select": "*",
		"where": map[string]interface{}{
			"event_id": "in.(" + strings.Join(eventIDs, ",") + ")",
			"status":   "pending",
		},
		"order": "created_at.desc",
	})
	if err != nil {
		log.Printf("[Mobile/GuestListsPending] err=%v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not load signups", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"signups": signups, "count": len(signups)})
}

// MobileApproveGuestListSignup handles POST /guest-lists/:signupId/approve.
func MobileApproveGuestListSignup(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	result, err := venueDB.UpdateCtx(ctx, "guest_list_signups", map[string]interface{}{
		"status":      "approved",
		"approved_at": time.Now().Format(time.RFC3339),
	}, map[string]interface{}{"id": c.Param("signupId")})
	if err != nil || len(result) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to approve"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "signup": result[0]})
}

// MobileRejectGuestListSignup handles POST /guest-lists/:signupId/reject.
func MobileRejectGuestListSignup(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&body)
	// guest_list_signups (BD viva 511) no tiene rejected_at/rejection_reason —
	// el motivo va en notes, la fecha la da updated_at.
	rejectData := map[string]interface{}{"status": "rejected"}
	if body.Reason != "" {
		rejectData["notes"] = "Rechazado: " + body.Reason
	}
	result, err := venueDB.UpdateCtx(ctx, "guest_list_signups", rejectData,
		map[string]interface{}{"id": c.Param("signupId")})
	if err != nil || len(result) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reject"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "signup": result[0]})
}

// MobileBatchApproveGuestList handles POST /guest-lists/batch/approve.
// Body: { signup_ids: ["uuid", ...] }
func MobileBatchApproveGuestList(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	var body struct {
		SignupIDs []string `json:"signup_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.SignupIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "signup_ids required"})
		return
	}
	updated := 0
	for _, id := range body.SignupIDs {
		r, _ := venueDB.UpdateCtx(ctx, "guest_list_signups", map[string]interface{}{
			"status":      "approved",
			"approved_at": time.Now().Format(time.RFC3339),
		}, map[string]interface{}{"id": id})
		updated += len(r)
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "updated": updated})
}

// MobileBatchRejectGuestList handles POST /guest-lists/batch/reject.
func MobileBatchRejectGuestList(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	var body struct {
		SignupIDs []string `json:"signup_ids"`
		Reason    string   `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.SignupIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "signup_ids required"})
		return
	}
	updated := 0
	bulkReject := map[string]interface{}{"status": "rejected"}
	if body.Reason != "" {
		bulkReject["notes"] = "Rechazado: " + body.Reason
	}
	for _, id := range body.SignupIDs {
		r, _ := venueDB.UpdateCtx(ctx, "guest_list_signups", bulkReject,
			map[string]interface{}{"id": id})
		updated += len(r)
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "updated": updated})
}

// MobileGetGuestListTypes handles GET /guest-lists/types/event/:eventId.
func MobileGetGuestListTypes(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	types, _ := venueDB.QueryCtx(ctx, "guest_list_types", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"event_id": c.Param("eventId")},
		"order": "name.asc",
	})
	c.JSON(http.StatusOK, types)
}

// MobileGetBookings handles GET /bookings/get-bookings/:venueId.
// In this demo, bookings == vip_list_reservations rows that are NOT group
// reservations (group ones have reservation_number starting with "GRP-").
func MobileGetBookings(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.Param("venueId")
	if venueID == "" {
		venueID = resolveVenueID(c)
	}
	// SECURITY: staff can only read their own venue (IDOR guard).
	if tokenVenue := c.GetString("venue_id"); tokenVenue != "" && venueID != tokenVenue {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied for this venue"})
		return
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	page, limit := 1, 20
	_, _ = fmtSscanInt(c.Query("page"), &page)
	_, _ = fmtSscanInt(c.Query("limit"), &limit)
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	where := map[string]interface{}{}
	if s := c.Query("status"); s != "" {
		where["status"] = s
	}
	bookings, _ := venueDB.QueryCtx(ctx, "vip_list_reservations", map[string]interface{}{
		"select": "*",
		"where":  where,
		"order":  "created_at.desc",
		"limit":  limit,
		"offset": (page - 1) * limit,
	})
	c.JSON(http.StatusOK, gin.H{
		"bookings": bookings,
		"count":    len(bookings),
		"page":     page,
		"limit":    limit,
	})
}

// MobileGetBookingDetails handles GET /bookings/get-booking-details/:bookingId.
func MobileGetBookingDetails(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	resv, _ := venueDB.QueryOne(ctx, "vip_list_reservations", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"id": c.Param("bookingId")},
	})
	if resv == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
		return
	}
	id := services.GetString(resv, "id")
	guests, _ := venueDB.QueryCtx(ctx, "vip_list_guests", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"reservation_id": id},
	})
	bottles, _ := venueDB.QueryCtx(ctx, "vip_list_bottles", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"reservation_id": id},
	})
	c.JSON(http.StatusOK, gin.H{
		"booking": resv, "guests": guests, "bottles": bottles,
	})
}

// MobileGetEmployees handles GET /employees/employees.
func MobileGetEmployees(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	emps, err := venueDB.QueryCtx(ctx, "organization_workers", map[string]interface{}{
		"select": "id,first_name,last_name,email,role_id,is_active,created_at",
		"where":  map[string]interface{}{"deleted_at": "is.null"},
		"order":  "created_at.desc",
	})
	if err != nil {
		log.Printf("[Mobile/Employees] err=%v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not load employees", "details": err.Error()})
		return
	}
	if emps == nil {
		emps = []map[string]interface{}{}
	}
	// Resolve role_id -> human-readable role name for each employee. The
	// mobile UI groups by role string (admin/staff), so a raw uuid would
	// make every employee fall into the "staff" bucket.
	roleMap := loadRoleMap(ctx, venueDB)
	for i, e := range emps {
		if rid := services.GetString(e, "role_id"); rid != "" {
			if name, ok := roleMap[rid]; ok {
				emps[i]["role"] = name
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": emps})
}

// MobileGetEmployee handles GET /employees/employees/:employeeId.
func MobileGetEmployee(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	emp, _ := venueDB.QueryOne(ctx, "organization_workers", map[string]interface{}{
		"select": "id,first_name,last_name,email,role_id,is_active,created_at",
		"where":  map[string]interface{}{"id": c.Param("employeeId"), "deleted_at": "is.null"},
	})
	if emp == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Employee not found"})
		return
	}
	if rid := services.GetString(emp, "role_id"); rid != "" {
		roleMap := loadRoleMap(ctx, venueDB)
		if name, ok := roleMap[rid]; ok {
			emp["role"] = name
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": emp})
}

// loadRoleMap returns a map of role.id -> role.name for the venue. Best-effort:
// if the roles table is missing or empty we return an empty map and the
// mobile UI will fall back to its "staff" default.
func loadRoleMap(ctx context.Context, venueDB *services.SupabaseClient) map[string]string {
	out := map[string]string{}
	if venueDB == nil {
		return out
	}
	roles, _ := venueDB.QueryCtx(ctx, "roles", map[string]interface{}{
		"select": "id,name",
	})
	for _, r := range roles {
		id := services.GetString(r, "id")
		name := services.GetString(r, "name")
		if id != "" && name != "" {
			out[id] = name
		}
	}
	return out
}

// MobileUploadEventImage handles POST /upload/event-image.
// The demo doesn't have S3/Supabase storage configured for staff uploads —
// we accept the payload, return a placeholder image url so the client UI
// can continue, and log what was attempted.
func MobileUploadEventImage(c *gin.Context) {
	var body struct {
		ImageBase64 string `json:"image_base64"`
		ContentType string `json:"content_type"`
		FileName    string `json:"file_name"`
	}
	_ = c.ShouldBindJSON(&body)
	size := len(body.ImageBase64)
	log.Printf("[Mobile/UploadEventImage] file=%s ct=%s size=%d (demo: not persisted)", body.FileName, body.ContentType, size)
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"image_url":  "https://images.unsplash.com/photo-1492684223066-81342ee5ff30?w=1200&q=80",
		"file_name":  body.FileName,
		"size_bytes": size,
		"note":       "demo: upload acepted but not persisted, returned placeholder url",
	})
}

// MobileOrdersSearch handles GET /orders/search/:venueId.
func MobileOrdersSearch(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	// SECURITY: staff can only search their own venue (IDOR guard).
	if tokenVenue := c.GetString("venue_id"); tokenVenue != "" && c.Param("venueId") != tokenVenue {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied for this venue"})
		return
	}
	venueDB := services.DB.ForVenue(c.Param("venueId"))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	search := strings.TrimSpace(c.Query("search"))
	if search == "" {
		c.JSON(http.StatusOK, gin.H{"orders": []interface{}{}, "count": 0})
		return
	}
	page, limit := 1, 20
	_, _ = fmtSscanInt(c.Query("page"), &page)
	_, _ = fmtSscanInt(c.Query("limit"), &limit)
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	// Search across customer_name (ilike). PostgREST takes percent-wildcards.
	q := strings.ReplaceAll(search, "%", "")
	orders, _ := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
		"select": "*",
		"where":  map[string]interface{}{"customer_name": "ilike.*" + q + "*"},
		"order":  "created_at.desc",
		"limit":  limit,
		"offset": (page - 1) * limit,
	})
	c.JSON(http.StatusOK, gin.H{
		"orders": orders,
		"count":  len(orders),
		"page":   page,
		"limit":  limit,
	})
}

// MobileOrdersRefreshView is a no-op endpoint the mobile app pings to ask
// the server to invalidate any cached/materialised views. We have none.
func MobileOrdersRefreshView(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"success": true, "refreshed": true})
}

// MobileBottleRedemptionPreview / Validate are stubs for the bottle voucher
// scanner. Aurora Hall demo doesn't have any vouchers issued so we always
// answer "not found" — keeps the mobile UI from crashing.
func MobileBottleRedemptionPreview(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"valid":   false,
		"message": "Voucher no encontrado (demo)",
	})
}

func MobileBottleRedemptionValidate(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"valid":   false,
		"message": "Voucher no encontrado (demo)",
	})
}

// MobileGetTicketTypesByEvent handles GET /ticket-types/event/:eventId.
func MobileGetTicketTypesByEvent(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	eventID := c.Param("eventId")
	tts, err := venueDB.QueryCtx(ctx, "ticket_types", map[string]interface{}{
		"select": services.TicketTypeSelectColumns + ",metadata",
		"where":  map[string]interface{}{"event_id": eventID, "is_active": true},
		"order":  "price.asc",
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not load ticket types"})
		return
	}
	if tts == nil {
		tts = []map[string]interface{}{}
	}
	for _, tt := range tts {
		liftTicketTypeMetadata(tt)
	}
	services.EnrichTicketTypes(tts)
	for _, tt := range tts {
		tt["benefits"] = flattenBenefits(tt["benefits"])
	}

	// Surface VIP table types as group tickets. These are the same rows the
	// WebApp sells as "Mesa Premium", so staff see the groups clients can book.
	vips, err := venueDB.QueryCtx(ctx, "event_vip_ticket_types", map[string]interface{}{
		"select": services.VIPTicketTypeSelectColumns,
		"where":  map[string]interface{}{"event_id": eventID},
		"order":  "sort_order.asc",
	})
	if err != nil {
		log.Printf("[Mobile/GetTicketTypes] vip types err=%v", err)
	}
	services.EnrichVIPTicketTypes(vips)
	for _, vt := range vips {
		total := services.GetInt(vt, "quantity_total")
		available := total - services.GetInt(vt, "quantity_sold")
		if available < 0 {
			available = 0
		}
		vt["is_group"] = true
		vt["is_vip_table"] = true
		vt["has_gender_pricing"] = true
		vt["price"] = services.GetFloat64(vt, "price_male")
		vt["available_quantity"] = available
		vt["initial_quantity"] = total
		// The WebApp group flow reserves tables of 4+; mirror that here.
		if services.GetInt(vt, "min_quantity") <= 1 {
			vt["min_quantity"] = 4
		}
		vt["benefits"] = services.GetString(vt, "description")
		tts = append(tts, vt)
	}
	c.JSON(http.StatusOK, tts)
}

// liftTicketTypeMetadata copies mobile-only fields persisted in the metadata
// jsonb column (is_group, gender pricing, expenses) to the row's top level so
// EnrichTicketType and the app can read them.
func liftTicketTypeMetadata(tt map[string]interface{}) {
	meta, _ := tt["metadata"].(map[string]interface{})
	if meta == nil {
		return
	}
	for _, k := range []string{"is_group", "has_gender_pricing", "male_price", "female_price", "expenses"} {
		if v, ok := meta[k]; ok {
			if _, exists := tt[k]; !exists {
				tt[k] = v
			}
		}
	}
}

// flattenBenefits renders the benefits column (text[] in the venue DB) as the
// comma-separated string the mobile app shows and edits.
func flattenBenefits(v interface{}) string {
	switch b := v.(type) {
	case string:
		return b
	case []interface{}:
		parts := make([]string, 0, len(b))
		for _, item := range b {
			if s, ok := item.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	default:
		return ""
	}
}

// splitBenefits converts the app's comma-separated benefits string back to
// the text[] shape the venue DB stores.
func splitBenefits(s string) []string {
	parts := []string{}
	for _, p := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}

// MobileCreateTicketType handles POST /ticket-types/event/:eventId.
func MobileCreateTicketType(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}
	eventID := c.Param("eventId")
	quantity := services.GetInt(body, "initialQuantity")
	if quantity == 0 {
		quantity = services.GetInt(body, "quantity")
	}

	// Group tickets from the staff app are VIP table types — the same table
	// the WebApp sells as "Mesa Premium".
	if services.GetBool(body, "isGroup") {
		maxGuests := services.GetInt(body, "maxQuantity")
		if maxGuests == 0 {
			maxGuests = 10
		}
		payload := map[string]interface{}{
			"event_id":       eventID,
			"name":           services.GetString(body, "name"),
			"description":    services.GetString(body, "benefits"),
			"price_male":     services.GetFloat64(body, "malePrice"),
			"price_female":   services.GetFloat64(body, "femalePrice"),
			"currency":       "GTQ",
			"quantity_total": quantity,
			"max_guests":     maxGuests,
		}
		result, err := venueDB.InsertCtx(ctx, "event_vip_ticket_types", payload)
		if err != nil || result == nil {
			log.Printf("[Mobile/CreateTicketType] vip err=%v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create group ticket type", "details": fmt.Sprintf("%v", err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
		return
	}

	minPer := services.GetInt(body, "minQuantity")
	if minPer == 0 {
		minPer = 1
	}
	maxPer := services.GetInt(body, "maxQuantity")
	if maxPer == 0 {
		maxPer = 10
	}
	metadata := map[string]interface{}{}
	if services.GetBool(body, "hasGenderPricing") {
		metadata["has_gender_pricing"] = true
		metadata["male_price"] = services.GetFloat64(body, "malePrice")
		metadata["female_price"] = services.GetFloat64(body, "femalePrice")
	}
	if v, ok := body["expenses"]; ok && v != nil {
		metadata["expenses"] = services.GetFloat64(body, "expenses")
	}
	payload := map[string]interface{}{
		"event_id":       eventID,
		"name":           services.GetString(body, "name"),
		"price":          services.GetFloat64(body, "price"),
		"quantity_total": quantity,
		"benefits":       splitBenefits(services.GetString(body, "benefits")),
		"min_per_order":  minPer,
		"max_per_order":  maxPer,
		"is_active":      true,
		"is_visible":     true,
		"metadata":       metadata,
	}
	result, err := venueDB.InsertCtx(ctx, "ticket_types", payload)
	if err != nil || result == nil {
		log.Printf("[Mobile/CreateTicketType] err=%v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create ticket type", "details": fmt.Sprintf("%v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// MobileUpdateTicketType handles PUT /ticket-types/:ticketTypeId.
func MobileUpdateTicketType(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}
	ticketTypeID := c.Param("ticketTypeId")

	// Build the updates for a regular ticket_types row.
	updates := map[string]interface{}{}
	if v, ok := body["name"]; ok {
		updates["name"] = v
	}
	if v, ok := body["is_active"]; ok {
		updates["is_active"] = v
	}
	if _, ok := body["benefits"]; ok {
		updates["benefits"] = splitBenefits(services.GetString(body, "benefits"))
	}
	if _, ok := body["price"]; ok {
		updates["price"] = services.GetFloat64(body, "price")
	}
	if _, ok := body["initialQuantity"]; ok {
		updates["quantity_total"] = services.GetInt(body, "initialQuantity")
	} else if _, ok := body["quantity"]; ok {
		updates["quantity_total"] = services.GetInt(body, "quantity")
	}
	if _, ok := body["minQuantity"]; ok {
		if v := services.GetInt(body, "minQuantity"); v > 0 {
			updates["min_per_order"] = v
		}
	}
	if _, ok := body["maxQuantity"]; ok {
		if v := services.GetInt(body, "maxQuantity"); v > 0 {
			updates["max_per_order"] = v
		}
	}
	if services.GetBool(body, "hasGenderPricing") || services.GetBool(body, "isGroup") {
		updates["metadata"] = map[string]interface{}{
			"is_group":           services.GetBool(body, "isGroup"),
			"has_gender_pricing": services.GetBool(body, "hasGenderPricing"),
			"male_price":         services.GetFloat64(body, "malePrice"),
			"female_price":       services.GetFloat64(body, "femalePrice"),
		}
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no updates supplied"})
		return
	}
	result, err := venueDB.UpdateCtx(ctx, "ticket_types", updates, map[string]interface{}{
		"id": ticketTypeID,
	})
	if err == nil && len(result) > 0 {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": result[0]})
		return
	}

	// No ticket_types row matched — the id may be a VIP table type (surfaced
	// as a group ticket in the app). Retry against event_vip_ticket_types.
	vipUpdates := map[string]interface{}{}
	if v, ok := body["name"]; ok {
		vipUpdates["name"] = v
	}
	if _, ok := body["benefits"]; ok {
		vipUpdates["description"] = services.GetString(body, "benefits")
	}
	if _, ok := body["malePrice"]; ok {
		vipUpdates["price_male"] = services.GetFloat64(body, "malePrice")
	}
	if _, ok := body["femalePrice"]; ok {
		vipUpdates["price_female"] = services.GetFloat64(body, "femalePrice")
	}
	if _, ok := body["initialQuantity"]; ok {
		vipUpdates["quantity_total"] = services.GetInt(body, "initialQuantity")
	}
	if _, ok := body["maxQuantity"]; ok {
		if v := services.GetInt(body, "maxQuantity"); v > 0 {
			vipUpdates["max_guests"] = v
		}
	}
	if len(vipUpdates) > 0 {
		vipResult, vipErr := venueDB.UpdateCtx(ctx, "event_vip_ticket_types", vipUpdates, map[string]interface{}{
			"id": ticketTypeID,
		})
		if vipErr == nil && len(vipResult) > 0 {
			c.JSON(http.StatusOK, gin.H{"success": true, "data": vipResult[0]})
			return
		}
	}

	log.Printf("[Mobile/UpdateTicketType] err=%v rows=%d id=%s", err, len(result), ticketTypeID)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update ticket type"})
}

// MobileDeleteTicketType handles DELETE /ticket-types/:ticketTypeId.
// Soft-disable instead of hard delete to preserve order/ticket history.
func MobileDeleteTicketType(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB := services.DB.ForVenue(resolveVenueID(c))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	ticketTypeID := c.Param("ticketTypeId")
	result, err := venueDB.UpdateCtx(ctx, "ticket_types", map[string]interface{}{
		"is_active": false,
	}, map[string]interface{}{"id": ticketTypeID})
	if err == nil && len(result) > 0 {
		c.JSON(http.StatusOK, gin.H{"success": true})
		return
	}

	// Not a ticket_types row — try the VIP table types surfaced as group
	// tickets. That table has no is_active column, so delete the row; the FK
	// from reservations blocks the delete if the type is already in use.
	if vipErr := venueDB.DeleteCtx(ctx, "event_vip_ticket_types", map[string]interface{}{
		"id": ticketTypeID,
	}); vipErr == nil {
		c.JSON(http.StatusOK, gin.H{"success": true})
		return
	} else {
		log.Printf("[Mobile/DeleteTicketType] err=%v vipErr=%v id=%s", err, vipErr, ticketTypeID)
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete ticket type"})
}
