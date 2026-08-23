package controllers

import (
	"context"
	"net/http"
	"pull-api-v2/models"
	"pull-api-v2/services"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// maxVenueScanRows es el techo de filas de las lecturas agregadas contra la BD
// de un venue. No es paginación: es una red de seguridad para que una tabla
// que crece (orders, tickets) no se traiga entera a memoria en cada request.
// Muy por encima del volumen de un evento real.
const maxVenueScanRows = 20000

// =============================================
// DASHBOARD ENDPOINTS (ULTRA-OPTIMIZED)
// =============================================

// GetDashboardStats returns venue dashboard overview stats
// GET /staff/dashboard/stats
// OPTIMIZED: 11 parallel queries instead of sequential
func GetDashboardStats(c *gin.Context) {
	claims, exists := c.Get("staff")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	staffClaims := claims.(*models.StaffClaims)
	venueID := staffClaims.VenueID
	orgID := staffClaims.OrganizationID

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	client := services.DB.ForVenue(venueID)

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekStart := todayStart.AddDate(0, 0, -7)
	monthStart := todayStart.AddDate(0, -1, 0)

	// Result containers
	var (
		monthOrders         []map[string]interface{} // Single query for today/week/month
		todayCheckIns       []map[string]interface{}
		activeEvents        []map[string]interface{}
		pendingOrders       []map[string]interface{}
		pendingReservations []map[string]interface{}
		pendingGuestLists   []map[string]interface{}
		unreadNotifications []map[string]interface{}
		currentEvents       []map[string]interface{}
		wg                  sync.WaitGroup
		mu                  sync.Mutex
	)

	// Execute 8 queries in parallel
	wg.Add(8)

	// Query 1: ALL orders from month (single query for today/week/month stats)
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "total_amount,quantity,status,created_at",
			"where": map[string]interface{}{
				"created_at": "gte." + monthStart.Format(time.RFC3339),
			},
		})
		mu.Lock()
		monthOrders = result
		mu.Unlock()
	}()

	// Query 2: Today's check-ins
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "tickets", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"is_checked_in": true,
				"checked_in_at": "gte." + todayStart.Format(time.RFC3339),
			},
		})
		mu.Lock()
		todayCheckIns = result
		mu.Unlock()
	}()

	// Query 3: Active events
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "events", map[string]interface{}{
			"select": "id,name,capacity,event_datetime",
			"where": map[string]interface{}{
				"status":         "active",
				"event_datetime": "gte." + todayStart.Format(time.RFC3339),
				"deleted_at":     "is.null",
			},
		})
		mu.Lock()
		activeEvents = result
		mu.Unlock()
	}()

	// Query 4: Pending orders
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"status": "pending",
			},
		})
		mu.Lock()
		pendingOrders = result
		mu.Unlock()
	}()

	// Query 5: Pending reservations
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "group_reservations", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"status": "pending",
			},
		})
		mu.Lock()
		pendingReservations = result
		mu.Unlock()
	}()

	// Query 6: Pending guest lists
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "guest_list_signups", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"status": "pending",
			},
		})
		mu.Lock()
		pendingGuestLists = result
		mu.Unlock()
	}()

	// Query 7: Unread notifications
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "notifications", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"organization_id": orgID,
				"is_read":         false,
				"is_archived":     false,
			},
		})
		mu.Lock()
		unreadNotifications = result
		mu.Unlock()
	}()

	// Query 8: Current events
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "events", map[string]interface{}{
			"select": "id,name,capacity",
			"where": map[string]interface{}{
				"status":         "active",
				"event_datetime": "lte." + now.Format(time.RFC3339),
				"deleted_at":     "is.null",
			},
			"order": "event_datetime.desc",
			"limit": 1,
		})
		mu.Lock()
		currentEvents = result
		mu.Unlock()
	}()

	wg.Wait()

	// Calculate stats from single month query (today/week/month)
	stats := models.DashboardStats{}

	for _, order := range monthOrders {
		if services.GetString(order, "status") != "paid" {
			continue
		}

		createdAt := services.GetTime(order, "created_at")
		if createdAt == nil {
			continue
		}

		amount := services.GetFloat64(order, "total_amount")
		qty := services.GetInt(order, "quantity")

		// Month stats (all paid orders in result)
		stats.MonthRevenue += amount
		stats.MonthOrders++
		stats.MonthTicketsSold += qty

		// Week stats
		if !createdAt.Before(weekStart) {
			stats.WeekRevenue += amount
			stats.WeekOrders++
			stats.WeekTicketsSold += qty

			// Today stats
			if !createdAt.Before(todayStart) {
				stats.TodayRevenue += amount
				stats.TodayOrders++
				stats.TodayTicketsSold += qty
			}
		}
	}

	stats.TodayCheckIns = len(todayCheckIns)
	stats.ActiveEvents = len(activeEvents)

	// Count upcoming events from active events
	for _, event := range activeEvents {
		eventDate := services.GetTime(event, "event_datetime")
		if eventDate != nil && eventDate.After(now) {
			stats.UpcomingEvents++
		}
	}

	stats.PendingOrders = len(pendingOrders)
	stats.PendingReservations = len(pendingReservations)
	stats.PendingGuestLists = len(pendingGuestLists)
	stats.UnreadNotifications = len(unreadNotifications)

	// Current event
	if len(currentEvents) > 0 {
		currentEvent := currentEvents[0]
		eventID := services.GetString(currentEvent, "id")
		eventName := services.GetString(currentEvent, "name")
		stats.CurrentEventID = &eventID
		stats.CurrentEventName = &eventName
		stats.CurrentEventCapacity = services.GetInt(currentEvent, "capacity")

		// Get check-ins for current event (separate query only if needed)
		currentCheckIns, _ := client.QueryCtx(ctx, "tickets", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"event_id":      eventID,
				"is_checked_in": true,
			},
		})
		stats.CurrentEventCheckIns = len(currentCheckIns)
	}

	c.JSON(http.StatusOK, stats)
}

// =============================================
// EVENT ANALYTICS (ULTRA-OPTIMIZED)
// =============================================

// GetEventAnalytics returns detailed analytics for a specific event
// GET /staff/events/:id/analytics
// OPTIMIZED: 9 parallel queries instead of sequential
func GetEventAnalytics(c *gin.Context) {
	claims, exists := c.Get("staff")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	staffClaims := claims.(*models.StaffClaims)
	venueID := staffClaims.VenueID
	eventID := c.Param("id")

	if eventID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Event ID is required"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	client := services.DB.ForVenue(venueID)

	// Get event details first (needed for validation)
	event, err := client.QueryOne(ctx, "events", map[string]interface{}{
		"select": "id,name,event_datetime",
		"where": map[string]interface{}{
			"id": eventID,
		},
	})
	if err != nil || event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}

	analytics := models.EventAnalytics{
		EventID:   eventID,
		EventName: services.GetString(event, "name"),
		EventDate: services.GetString(event, "event_datetime"),
	}

	// Result containers
	var (
		orders           []map[string]interface{}
		ticketTypes      []map[string]interface{}
		tickets          []map[string]interface{}
		reservations     []map[string]interface{}
		vipLists         []map[string]interface{}
		vipGuests        []map[string]interface{}
		guestListTypes   []map[string]interface{}
		guestListSignups []map[string]interface{}
		resGuests        []map[string]interface{}
		wg               sync.WaitGroup
		mu               sync.Mutex
	)

	// Execute 9 queries in parallel
	wg.Add(9)

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "total_amount,subtotal,platform_fee,gateway_fee,quantity",
			"where": map[string]interface{}{
				"event_id": eventID,
				"status":   "paid",
			},
		})
		mu.Lock()
		orders = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "ticket_types", map[string]interface{}{
			"select": "id,name,price,initial_quantity,quantity_available",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
		})
		mu.Lock()
		ticketTypes = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "tickets", map[string]interface{}{
			"select": "id,ticket_type_id,is_checked_in",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
		})
		mu.Lock()
		tickets = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "group_reservations", map[string]interface{}{
			"select": "status,total_guests,total_paid",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
		})
		mu.Lock()
		reservations = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "vip_list_reservations", map[string]interface{}{
			"select": "guest_count,total_paid,is_paid",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
		})
		mu.Lock()
		vipLists = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "vip_list_guests", map[string]interface{}{
			"select": "id,is_checked_in",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
		})
		mu.Lock()
		vipGuests = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "guest_list_types", map[string]interface{}{
			"select": "id",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
		})
		mu.Lock()
		guestListTypes = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "guest_list_signups", map[string]interface{}{
			"select": "status,is_checked_in",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
		})
		mu.Lock()
		guestListSignups = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "group_reservation_guests", map[string]interface{}{
			"select": "id,is_checked_in",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
		})
		mu.Lock()
		resGuests = result
		mu.Unlock()
	}()

	wg.Wait()

	// Process orders
	for _, order := range orders {
		analytics.TotalRevenue += services.GetFloat64(order, "total_amount")
		analytics.TicketRevenue += services.GetFloat64(order, "subtotal")
		analytics.PlatformFees += services.GetFloat64(order, "platform_fee")
		analytics.GatewayFees += services.GetFloat64(order, "gateway_fee")
		analytics.TicketsSold += services.GetInt(order, "quantity")
	}
	analytics.NetRevenue = analytics.TotalRevenue - analytics.PlatformFees - analytics.GatewayFees

	// Build ticket type check-in map from tickets (single pass)
	ticketTypeCheckIns := make(map[string]int, len(ticketTypes))
	analytics.TotalCapacity = len(tickets)
	for _, ticket := range tickets {
		ttID := services.GetString(ticket, "ticket_type_id")
		if services.GetBool(ticket, "is_checked_in") {
			ticketTypeCheckIns[ttID]++
			analytics.TicketsCheckedIn++
			analytics.TotalCheckedIn++
		}
	}

	// Process ticket types (no N+1 query!)
	analytics.TicketTypeStats = make([]models.TicketTypeStats, 0, len(ticketTypes))
	for _, tt := range ticketTypes {
		ttID := services.GetString(tt, "id")
		initial := services.GetInt(tt, "initial_quantity")
		remaining := services.GetInt(tt, "quantity_available")
		sold := initial - remaining
		price := services.GetFloat64(tt, "price")

		stat := models.TicketTypeStats{
			TicketTypeID:    ttID,
			Name:            services.GetString(tt, "name"),
			Price:           price,
			InitialQuantity: initial,
			Sold:            sold,
			Remaining:       remaining,
			Revenue:         float64(sold) * price,
			CheckedIn:       ticketTypeCheckIns[ttID], // O(1) lookup instead of query
		}
		if initial > 0 {
			stat.SellThrough = float64(sold) / float64(initial) * 100
		}
		analytics.TicketTypeStats = append(analytics.TicketTypeStats, stat)
		analytics.TotalTicketCapacity += initial
	}
	analytics.TicketsRemaining = analytics.TotalTicketCapacity - analytics.TicketsSold
	if analytics.TotalTicketCapacity > 0 {
		analytics.TicketSellThrough = float64(analytics.TicketsSold) / float64(analytics.TotalTicketCapacity) * 100
	}

	// Process reservations
	for _, res := range reservations {
		status := services.GetString(res, "status")
		analytics.TotalReservations++
		analytics.TotalReservationGuests += services.GetInt(res, "total_guests")
		switch status {
		case "confirmed":
			analytics.ConfirmedReservations++
			analytics.ReservationRevenue += services.GetFloat64(res, "total_paid")
		case "pending":
			analytics.PendingReservations++
		case "cancelled":
			analytics.CancelledReservations++
		}
	}

	// Process VIP lists
	analytics.TotalVIPLists = len(vipLists)
	for _, vl := range vipLists {
		analytics.TotalVIPGuests += services.GetInt(vl, "guest_count")
		analytics.VIPListRevenue += services.GetFloat64(vl, "total_paid")
		if services.GetBool(vl, "is_paid") {
			analytics.VIPGuestsPaid += services.GetInt(vl, "guest_count")
		}
	}

	// Process VIP guests check-ins
	for _, vg := range vipGuests {
		if services.GetBool(vg, "is_checked_in") {
			analytics.VIPCheckedIn++
			analytics.TotalCheckedIn++
		}
	}

	// Process guest list
	analytics.TotalGuestListTypes = len(guestListTypes)
	analytics.TotalGuestListSignups = len(guestListSignups)
	for _, gls := range guestListSignups {
		status := services.GetString(gls, "status")
		if status == "approved" {
			analytics.ApprovedSignups++
		} else if status == "pending" {
			analytics.PendingSignups++
		}
		if services.GetBool(gls, "is_checked_in") {
			analytics.GuestListCheckedIn++
			analytics.TotalCheckedIn++
		}
	}

	// Process reservation guests check-ins
	for _, rg := range resGuests {
		if services.GetBool(rg, "is_checked_in") {
			analytics.ReservationsCheckedIn++
			analytics.TotalCheckedIn++
		}
	}

	totalAttendees := analytics.TotalCapacity + analytics.TotalVIPGuests + analytics.TotalGuestListSignups + analytics.TotalReservationGuests
	if totalAttendees > 0 {
		analytics.CheckInRate = float64(analytics.TotalCheckedIn) / float64(totalAttendees) * 100
	}

	c.JSON(http.StatusOK, analytics)
}

// =============================================
// VENUE ANALYTICS
// =============================================

// GetVenueAnalytics returns overall venue performance analytics
// GET /staff/analytics/venue
//
// TODO(panel-venue): ESTE HANDLER SIGUE DEVOLVIENDO DATOS FALSOS. Se arregló
// el mecanismo (los filtros ya van dentro de "where" y por tanto se aplican de
// verdad), pero las consultas piden columnas que NO EXISTEN en la BD del
// venue. Una columna inexistente da 42703 y `QueryCtx` devuelve la lista
// VACÍA sin error visible, así que las cifras salen a cero en vez de salir
// mal. Antes salían mal en silencio; ahora salen a cero. Las dos cosas están
// rotas: no consumas este endpoint hasta reescribirlo.
//
// Columnas fantasma → columna real (schema en sql/venue_template.sql):
//   - TODAS las tablas: `venue_id` NO EXISTE. La tenencia ES la base de datos
//     por venue (services.DB.ForVenue), no una columna.
//   - orders.status = "paid" no es válido. El enum order_status es
//     pending|processing|confirmed|failed|cancelled → el estado pagado es
//     "confirmed".
//   - orders.total_amount → `total`. orders.gateway_fee no existe
//     (`platform_fee` sí).
//   - events.event_datetime → `start_datetime`.
//   - tickets.is_checked_in → no existe; el check-in es `checked_in_at`
//     (NOT NULL = presentado).
//   - group_reservations.total_paid → no existe (`total`, `deposit_amount`).
//   - vip_list_reservations: el flujo VIP list está RETIRADO (producto,
//     2026-07). Esta query sobra entera.
func GetVenueAnalytics(c *gin.Context) {
	claims, exists := c.Get("staff")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	staffClaims := claims.(*models.StaffClaims)
	venueID := staffClaims.VenueID

	// Parse date range
	period := c.DefaultQuery("period", "month")
	startDateStr := c.Query("start_date")
	endDateStr := c.Query("end_date")

	now := time.Now()
	var startDate, endDate time.Time

	switch period {
	case "day":
		startDate = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		endDate = now
	case "week":
		startDate = now.AddDate(0, 0, -7)
		endDate = now
	case "month":
		startDate = now.AddDate(0, -1, 0)
		endDate = now
	case "year":
		startDate = now.AddDate(-1, 0, 0)
		endDate = now
	case "custom":
		if startDateStr != "" {
			if t, err := time.Parse("2006-01-02", startDateStr); err == nil {
				startDate = t
			}
		}
		if endDateStr != "" {
			if t, err := time.Parse("2006-01-02", endDateStr); err == nil {
				endDate = t
			}
		}
	default:
		startDate = now.AddDate(0, -1, 0)
		endDate = now
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	client := services.DB.ForVenue(venueID)
	startDateRFC := startDate.Format(time.RFC3339)

	// =============================================
	// PARALLEL QUERY EXECUTION (7 queries in parallel)
	// =============================================
	var (
		venue            map[string]interface{}
		orders           []map[string]interface{}
		events           []map[string]interface{}
		reservations     []map[string]interface{}
		vipLists         []map[string]interface{}
		guestListSignups []map[string]interface{}
		tickets          []map[string]interface{}
		mu               sync.Mutex
		wg               sync.WaitGroup
	)

	wg.Add(7)

	// NOTA: los filtros van dentro de "where". A nivel raíz, buildQueryParams
	// los IGNORA en silencio (services/supabase.go) y la consulta devolvía la
	// tabla entera sin filtrar. Ver el TODO de la cabecera del handler: las
	// columnas que se piden aquí siguen siendo, muchas de ellas, inexistentes.
	go func() {
		defer wg.Done()
		result, _ := client.QueryOne(ctx, "venues", map[string]interface{}{
			"where": map[string]interface{}{
				"id": venueID,
			},
		})
		mu.Lock()
		venue = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "orders", map[string]interface{}{
			"where": map[string]interface{}{
				"venue_id":   venueID,
				"status":     "paid",
				"created_at": "gte." + startDateRFC,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		orders = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "events", map[string]interface{}{
			"where": map[string]interface{}{
				"venue_id":       venueID,
				"event_datetime": "gte." + startDateRFC,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		events = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "group_reservations", map[string]interface{}{
			"where": map[string]interface{}{
				"venue_id":   venueID,
				"status":     "confirmed",
				"created_at": "gte." + startDateRFC,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		reservations = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "vip_list_reservations", map[string]interface{}{
			"where": map[string]interface{}{
				"venue_id":   venueID,
				"created_at": "gte." + startDateRFC,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		vipLists = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "guest_list_signups", map[string]interface{}{
			"where": map[string]interface{}{
				"venue_id":   venueID,
				"status":     "approved",
				"created_at": "gte." + startDateRFC,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		guestListSignups = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "tickets", map[string]interface{}{
			"where": map[string]interface{}{
				"venue_id":      venueID,
				"is_checked_in": true,
				"checked_in_at": "gte." + startDateRFC,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		tickets = result
		mu.Unlock()
	}()

	wg.Wait()

	// =============================================
	// PROCESS RESULTS
	// =============================================
	analytics := models.VenueAnalytics{
		VenueID:   venueID,
		VenueName: services.GetString(venue, "name"),
		Period:    period,
		StartDate: startDate.Format("2006-01-02"),
		EndDate:   endDate.Format("2006-01-02"),
	}

	// Pre-allocate slices with capacity hints
	orderValues := make([]float64, 0, len(orders))
	ticketsPerOrder := make([]int, 0, len(orders))
	dailySalesMap := make(map[string]*models.DailySales, 30) // ~30 days typical

	for _, order := range orders {
		createdAt := services.GetString(order, "created_at")
		orderDate := ""
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			if t.Before(endDate) {
				orderDate = t.Format("2006-01-02")
			}
		}
		if orderDate == "" {
			continue
		}

		amount := services.GetFloat64(order, "total_amount")
		qty := services.GetInt(order, "quantity")
		platformFee := services.GetFloat64(order, "platform_fee")
		gatewayFee := services.GetFloat64(order, "gateway_fee")

		analytics.TotalRevenue += amount
		analytics.TotalOrders++
		analytics.TotalTicketsSold += qty
		analytics.TotalPlatformFees += platformFee
		analytics.TotalGatewayFees += gatewayFee
		analytics.TicketRevenue += services.GetFloat64(order, "subtotal")

		orderValues = append(orderValues, amount)
		ticketsPerOrder = append(ticketsPerOrder, qty)

		// Aggregate by day
		if dailySalesMap[orderDate] == nil {
			dailySalesMap[orderDate] = &models.DailySales{Date: orderDate}
		}
		dailySalesMap[orderDate].Orders++
		dailySalesMap[orderDate].Tickets += qty
		dailySalesMap[orderDate].Revenue += amount
	}

	analytics.NetRevenue = analytics.TotalRevenue - analytics.TotalPlatformFees - analytics.TotalGatewayFees

	// Calculate averages (single pass instead of two)
	if analytics.TotalOrders > 0 {
		var totalOrderValue float64
		var totalTickets int
		for i, v := range orderValues {
			totalOrderValue += v
			totalTickets += ticketsPerOrder[i]
		}
		analytics.AvgOrderValue = totalOrderValue / float64(analytics.TotalOrders)
		analytics.AvgTicketsPerOrder = float64(totalTickets) / float64(analytics.TotalOrders)
	}

	// Process events
	eventsInPeriod := 0
	for _, event := range events {
		eventDate := services.GetString(event, "event_datetime")
		if t, err := time.Parse(time.RFC3339, eventDate); err == nil {
			if t.Before(endDate) {
				eventsInPeriod++
			}
		}
	}
	analytics.TotalEvents = eventsInPeriod

	if analytics.TotalEvents > 0 {
		analytics.AvgRevenuePerEvent = analytics.TotalRevenue / float64(analytics.TotalEvents)
	}

	// Process reservations
	for _, res := range reservations {
		analytics.TotalReservations++
		analytics.ReservationRevenue += services.GetFloat64(res, "total_paid")
	}

	// Process VIP lists
	for _, vl := range vipLists {
		status := services.GetString(vl, "status")
		if status == "approved" || status == "confirmed" {
			analytics.TotalVIPLists++
			analytics.VIPListRevenue += services.GetFloat64(vl, "total_paid")
		}
	}

	// Process guest list signups
	analytics.TotalGuestListSignups = len(guestListSignups)

	// Process check-ins
	analytics.TotalCheckIns = len(tickets)

	// Calculate check-in rate
	if analytics.TotalTicketsSold > 0 {
		analytics.AvgCheckInRate = float64(analytics.TotalCheckIns) / float64(analytics.TotalTicketsSold) * 100
	}

	// Build revenue trend with pre-allocated capacity
	analytics.RevenueTrend = make([]models.DailySales, 0, len(dailySalesMap))
	for _, ds := range dailySalesMap {
		analytics.RevenueTrend = append(analytics.RevenueTrend, *ds)
	}

	c.JSON(http.StatusOK, analytics)
}

// =============================================
// REVENUE REPORTS
// =============================================

// GetRevenueReport returns detailed revenue breakdown
// GET /staff/reports/revenue
func GetRevenueReport(c *gin.Context) {
	claims, exists := c.Get("staff")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	staffClaims := claims.(*models.StaffClaims)
	venueID := staffClaims.VenueID

	// Parse date range
	startDateStr := c.Query("start_date")
	endDateStr := c.Query("end_date")

	now := time.Now()
	startDate := now.AddDate(0, -1, 0)
	endDate := now

	if startDateStr != "" {
		if t, err := time.Parse("2006-01-02", startDateStr); err == nil {
			startDate = t
		}
	}
	if endDateStr != "" {
		if t, err := time.Parse("2006-01-02", endDateStr); err == nil {
			endDate = t
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	client := services.DB.ForVenue(venueID)
	central := services.DB.Central()
	startDateRFC := startDate.Format(time.RFC3339)

	report := models.RevenueReport{
		VenueID:     venueID,
		Period:      "custom",
		StartDate:   startDate,
		EndDate:     endDate,
		GeneratedAt: now,
	}

	// =============================================
	// PARALLEL QUERY EXECUTION (4 queries in parallel)
	// =============================================
	var (
		transactions []map[string]interface{}
		refunds      []map[string]interface{}
		reservations []map[string]interface{}
		events       []map[string]interface{}
		mu           sync.Mutex
		wg           sync.WaitGroup
	)

	wg.Add(4)

	// *** FUGA ENTRE CLIENTES (arreglada aquí) ***
	// Estas dos consultas van contra la BD CENTRAL, que es COMPARTIDA por
	// todos los venues. Los filtros iban a nivel raíz y buildQueryParams los
	// descartaba en silencio: el informe de un venue traía las transacciones
	// de TODOS. Con un solo cliente pasaba inadvertido; con el segundo, un
	// venue vería la facturación del otro. Van dentro de "where".
	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "transactions", map[string]interface{}{
			"where": map[string]interface{}{
				"venue_id":   venueID,
				"status":     "completed",
				"created_at": "gte." + startDateRFC,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		transactions = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "transactions", map[string]interface{}{
			"where": map[string]interface{}{
				"venue_id":         venueID,
				"transaction_type": "refund",
				"created_at":       "gte." + startDateRFC,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		refunds = result
		mu.Unlock()
	}()

	// Las dos de abajo van contra la BD del venue (ForVenue), donde no hay
	// fuga posible — pero los filtros se descartaban igual, así que `status` y
	// `created_at` tampoco se aplicaban.
	//
	// Se quita `venue_id`: esa columna NO EXISTE en la BD del venue (la
	// tenencia ES la base de datos, ver sql/venue_template.sql). Mientras el
	// filtro se ignoraba daba igual; ahora que se aplica de verdad, dejarlo
	// haría fallar la consulta con 42703 y `QueryCtx` devolvería lista vacía
	// SIN error visible — el mapa de nombres de evento se quedaría en blanco.
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "group_reservations", map[string]interface{}{
			"where": map[string]interface{}{
				"status":     "confirmed",
				"created_at": "gte." + startDateRFC,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		reservations = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		// Get all events for this venue to build name map (eliminates N+1 query)
		result, _ := client.QueryCtx(ctx, "events", map[string]interface{}{
			"select": "id,name",
			"limit":  maxVenueScanRows,
		})
		mu.Lock()
		events = result
		mu.Unlock()
	}()

	wg.Wait()

	// Build event name map for O(1) lookups (eliminates N+1 query!)
	eventNameMap := make(map[string]string, len(events))
	for _, event := range events {
		eventNameMap[services.GetString(event, "id")] = services.GetString(event, "name")
	}

	// =============================================
	// PROCESS TRANSACTIONS
	// =============================================
	report.Transactions = make([]models.TransactionSummary, 0, len(transactions))

	for _, tx := range transactions {
		createdAt := services.GetString(tx, "created_at")
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			if t.After(endDate) {
				continue
			}
		}

		gross := services.GetFloat64(tx, "gross_amount")
		platformFee := services.GetFloat64(tx, "platform_fee_amount")
		gatewayFee := services.GetFloat64(tx, "gateway_fee_amount")
		net := services.GetFloat64(tx, "net_to_venue")

		report.GrossRevenue += gross
		report.PlatformFees += platformFee
		report.GatewayFees += gatewayFee
		report.NetRevenue += net

		txType := services.GetString(tx, "transaction_type")
		switch txType {
		case "individual_ticket", "group_organizer", "group_guest":
			report.TicketRevenue += gross
		case "vip_list_organizer", "vip_list_guest":
			report.VIPListRevenue += gross
		}

		gateway := services.GetString(tx, "payment_gateway")
		switch gateway {
		case "dlocal":
			// Pasarela activa: sin este case, TODA la facturación real caía
			// fuera del desglose y el informe cuadraba a cero por pasarela.
			report.DLocalRevenue += gross
		case "stripe":
			report.StripeRevenue += gross
		case "neonet":
			report.NeoNetRevenue += gross
		case "mercadopago":
			report.MercadoPagoRevenue += gross
		case "cash":
			report.CashRevenue += gross
		case "transfer":
			report.TransferRevenue += gross
		}

		// Build transaction summary
		txCreatedAt, _ := time.Parse(time.RFC3339, createdAt)
		summary := models.TransactionSummary{
			ID:             services.GetString(tx, "id"),
			Date:           txCreatedAt,
			Type:           txType,
			CustomerName:   services.GetString(tx, "payer_name"),
			CustomerEmail:  services.GetString(tx, "payer_email"),
			GrossAmount:    gross,
			PlatformFee:    platformFee,
			GatewayFee:     gatewayFee,
			NetAmount:      net,
			PaymentGateway: gateway,
			Status:         services.GetString(tx, "status"),
		}

		// Get event name via O(1) map lookup instead of N+1 query!
		if eventID := services.GetString(tx, "event_id"); eventID != "" {
			summary.EventName = eventNameMap[eventID]
		}

		report.Transactions = append(report.Transactions, summary)
	}

	// Process refunds
	for _, refund := range refunds {
		createdAt := services.GetString(refund, "created_at")
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			if t.Before(endDate) {
				report.RefundedAmount += services.GetFloat64(refund, "gross_amount")
			}
		}
	}

	// Process reservations
	for _, res := range reservations {
		report.ReservationRevenue += services.GetFloat64(res, "total_paid")
	}

	c.JSON(http.StatusOK, report)
}

// =============================================
// PLATFORM ANALYTICS (Admin only)
// =============================================

// GetPlatformAnalytics returns platform-wide analytics
// GET /admin/analytics
func GetPlatformAnalytics(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	central := services.DB.Central()

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekStart := todayStart.AddDate(0, 0, -7)
	monthStart := todayStart.AddDate(0, -1, 0)

	// Ventana de la consulta. Antes NO había ninguna: se leía la tabla
	// `transactions` entera, sin select y sin limit, en cada request. Con la
	// ventana, los totales que devuelve este endpoint son "de los últimos N
	// días", no "de siempre" — por eso se publica `window_days` en la
	// respuesta, para que el panel lo pueda rotular.
	windowDays := platformAnalyticsDefaultDays
	if d, err := strconv.Atoi(c.Query("days")); err == nil && d > 0 {
		windowDays = d
	}
	if windowDays > platformAnalyticsMaxDays {
		windowDays = platformAnalyticsMaxDays
	}
	windowStart := todayStart.AddDate(0, 0, -windowDays)

	// Cache: la clave lleva el rol y la ventana. Sin la ventana, un ?days=7 y
	// un ?days=365 compartirían snapshot.
	role := c.GetString("role")
	cacheKey := platformCacheKey("platform:analytics", role, strconv.Itoa(windowDays))
	if cached, ok := services.AppCache.Get(cacheKey); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	// =============================================
	// PARALLEL QUERY EXECUTION (4 queries in parallel)
	// =============================================
	var (
		allTransactions []map[string]interface{}
		venues          []map[string]interface{}
		users           []map[string]interface{}
		events          []map[string]interface{}
		mu              sync.Mutex
		wg              sync.WaitGroup
	)

	wg.Add(4)

	// *** La consulta más cara del repo (arreglada aquí) ***
	// Leía `transactions` COMPLETA: sin select (o sea `*`, decenas de
	// columnas), sin limit y sin filtro de fecha. Además `status` iba a nivel
	// raíz, así que buildQueryParams lo descartaba y ni siquiera filtraba por
	// completadas. Ahora: select explícito con las 4 columnas que se usan,
	// filtro dentro de "where", ventana de fecha y techo de filas.
	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "transactions", map[string]interface{}{
			"select": "venue_id,gross_amount,platform_fee_amount,created_at",
			"where": map[string]interface{}{
				"status":     "completed",
				"created_at": "gte." + windowStart.Format(time.RFC3339),
			},
			"limit": maxPlatformScanRows,
		})
		mu.Lock()
		allTransactions = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "venues", map[string]interface{}{
			"select": "id,name,is_active",
			"limit":  maxPlatformScanRows,
		})
		mu.Lock()
		venues = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		// Techo de filas: `users` es la tabla que más crece de la central y
		// aquí solo se cuenta. Si alguna vez se alcanza el techo, los
		// contadores de usuarios se saturan en ese número.
		result, _ := central.QueryCtx(ctx, "users", map[string]interface{}{
			"select": "id,last_login_at,created_at",
			"limit":  maxPlatformScanRows,
		})
		mu.Lock()
		users = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "events_summary", map[string]interface{}{
			"select": "id,event_datetime,status",
			"limit":  maxPlatformScanRows,
		})
		mu.Lock()
		events = result
		mu.Unlock()
	}()

	wg.Wait()

	// =============================================
	// BUILD VENUE NAME MAP (eliminates O(n²) lookup)
	// =============================================
	venueNameMap := make(map[string]string, len(venues))
	activeVenues := 0
	for _, venue := range venues {
		vID := services.GetString(venue, "id")
		venueNameMap[vID] = services.GetString(venue, "name")
		if services.GetBool(venue, "is_active") {
			activeVenues++
		}
	}

	// =============================================
	// PROCESS TRANSACTIONS
	// =============================================
	dashboard := models.PlatformDashboard{
		TotalVenues:  len(venues),
		ActiveVenues: activeVenues,
		TotalUsers:   len(users),
		TotalEvents:  len(events),
		WindowDays:   windowDays,
	}

	for _, tx := range allTransactions {
		gross := services.GetFloat64(tx, "gross_amount")
		platformFee := services.GetFloat64(tx, "platform_fee_amount")
		createdAt := services.GetString(tx, "created_at")

		dashboard.TotalPlatformRevenue += platformFee
		dashboard.TotalGrossVolume += gross
		dashboard.TotalTransactions++

		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			if t.After(todayStart) {
				dashboard.TodayPlatformRevenue += platformFee
				dashboard.TodayGrossVolume += gross
				dashboard.TodayTransactions++
			}
			if t.After(weekStart) {
				dashboard.WeekPlatformRevenue += platformFee
			}
			if t.After(monthStart) {
				dashboard.MonthPlatformRevenue += platformFee
			}
		}
	}

	// =============================================
	// PROCESS USERS
	// =============================================
	activeUsers := 0
	newUsersToday := 0
	newUsersWeek := 0
	for _, user := range users {
		lastLogin := services.GetString(user, "last_login_at")
		createdAt := services.GetString(user, "created_at")

		if t, err := time.Parse(time.RFC3339, lastLogin); err == nil {
			if t.After(monthStart) {
				activeUsers++
			}
		}
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			if t.After(todayStart) {
				newUsersToday++
			}
			if t.After(weekStart) {
				newUsersWeek++
			}
		}
	}
	dashboard.ActiveUsers = activeUsers
	dashboard.NewUsersToday = newUsersToday
	dashboard.NewUsersWeek = newUsersWeek

	// =============================================
	// PROCESS EVENTS
	// =============================================
	tomorrowStart := todayStart.AddDate(0, 0, 1)
	for _, event := range events {
		eventDate := services.GetString(event, "event_datetime")
		status := services.GetString(event, "status")

		if status == "active" {
			if t, err := time.Parse(time.RFC3339, eventDate); err == nil {
				if t.After(now) {
					dashboard.ActiveEvents++
				}
				if t.After(todayStart) && t.Before(tomorrowStart) {
					dashboard.EventsToday++
				}
				if t.After(weekStart) {
					dashboard.EventsThisWeek++
				}
			}
		}
	}

	// =============================================
	// TOP VENUES BY REVENUE (O(1) name lookup instead of O(n))
	// =============================================
	venueRevenueMap := make(map[string]*models.VenueRevenueSummary, len(venues))
	for _, tx := range allTransactions {
		vID := services.GetString(tx, "venue_id")
		if venueRevenueMap[vID] == nil {
			venueRevenueMap[vID] = &models.VenueRevenueSummary{
				VenueID:   vID,
				VenueName: venueNameMap[vID], // O(1) lookup instead of O(n) loop!
			}
		}
		venueRevenueMap[vID].TotalGrossVolume += services.GetFloat64(tx, "gross_amount")
		venueRevenueMap[vID].PlatformFees += services.GetFloat64(tx, "platform_fee_amount")
		venueRevenueMap[vID].TransactionCount++
	}

	// Build and sort top venues
	dashboard.TopVenues = make([]models.VenueRevenueSummary, 0, len(venueRevenueMap))
	for _, vrs := range venueRevenueMap {
		dashboard.TopVenues = append(dashboard.TopVenues, *vrs)
	}

	// Sort by gross volume descending and take top 10
	sort.Slice(dashboard.TopVenues, func(i, j int) bool {
		return dashboard.TopVenues[i].TotalGrossVolume > dashboard.TopVenues[j].TotalGrossVolume
	})
	if len(dashboard.TopVenues) > 10 {
		dashboard.TopVenues = dashboard.TopVenues[:10]
	}

	services.AppCache.Set(cacheKey, dashboard, platformAnalyticsTTL)
	c.JSON(http.StatusOK, dashboard)
}

// =============================================
// SALES BY TIME PERIOD
// =============================================

// GetSalesByPeriod returns sales data grouped by time period
// GET /staff/analytics/sales
func GetSalesByPeriod(c *gin.Context) {
	claims, exists := c.Get("staff")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	staffClaims := claims.(*models.StaffClaims)
	venueID := staffClaims.VenueID

	groupBy := c.DefaultQuery("group_by", "day") // day, week, month
	days, _ := strconv.Atoi(c.DefaultQuery("days", "30"))
	if days > 365 {
		days = 365
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	client := services.DB.ForVenue(venueID)

	now := time.Now()
	startDate := now.AddDate(0, 0, -days)

	// Filtros dentro de "where": a nivel raíz buildQueryParams los descartaba
	// y esto sumaba TODAS las órdenes del venue, pagadas o no.
	//   - `venue_id` fuera: no existe en la BD del venue (la tenencia ES la
	//     base de datos). Aplicado de verdad daría 42703 → lista vacía.
	//   - "paid" no es un valor del enum order_status
	//     (pending|processing|confirmed|payment_authorized|payment_failed|
	//     checked_in|cancelled|refunded|expired). El estado pagado es
	//     "confirmed", que es el que escribe order_controller.go al cobrar.
	orders, _ := client.QueryCtx(ctx, "orders", map[string]interface{}{
		"select": "quantity,total,status,created_at",
		"where": map[string]interface{}{
			// Ver estados_orden.go: con "confirmed" a secas, las ventas
			// DESAPARECÍAN de la serie según la gente entraba por la puerta.
			"status":     estadosCobrados,
			"created_at": "gte." + startDate.Format(time.RFC3339),
		},
		"limit": maxVenueScanRows,
	})

	salesMap := make(map[string]*models.DailySales)

	for _, order := range orders {
		createdAt := services.GetString(order, "created_at")
		t, err := time.Parse(time.RFC3339, createdAt)
		if err != nil {
			continue
		}

		var key string
		switch groupBy {
		case "week":
			year, week := t.ISOWeek()
			key = strconv.Itoa(year) + "-W" + strconv.Itoa(week)
		case "month":
			key = t.Format("2006-01")
		default:
			key = t.Format("2006-01-02")
		}

		if salesMap[key] == nil {
			salesMap[key] = &models.DailySales{Date: key}
		}
		salesMap[key].Orders++
		salesMap[key].Tickets += services.GetInt(order, "quantity")
		// `total`, no `total_amount`: esa columna no existe en `orders` y
		// GetFloat64 devolvía 0, así que la facturación salía plana a cero.
		salesMap[key].Revenue += services.GetFloat64(order, "total")
	}

	// Convert to slice
	sales := make([]models.DailySales, 0)
	for _, s := range salesMap {
		sales = append(sales, *s)
	}

	c.JSON(http.StatusOK, gin.H{
		"group_by": groupBy,
		"days":     days,
		"sales":    sales,
	})
}

// =============================================
// CHECK-IN ANALYTICS
// =============================================

// GetCheckInAnalytics returns check-in data for an event
// GET /staff/events/:id/check-ins/analytics
func GetCheckInAnalytics(c *gin.Context) {
	claims, exists := c.Get("staff")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	staffClaims := claims.(*models.StaffClaims)
	venueID := staffClaims.VenueID
	eventID := c.Param("id")

	if eventID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Event ID is required"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	client := services.DB.ForVenue(venueID)

	// Verify event exists. El filtro va dentro de "where" (a nivel raíz se
	// descartaba y esto devolvía el PRIMER evento de la tabla, fuese cual
	// fuese el :id pedido). `venue_id` fuera: no existe en la BD del venue, la
	// tenencia ya la da services.DB.ForVenue(venueID).
	event, err := client.QueryOne(ctx, "events", map[string]interface{}{
		"select": "id",
		"where": map[string]interface{}{
			"id": eventID,
		},
	})
	if err != nil || event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}

	// =============================================
	// PARALLEL QUERY EXECUTION (3 queries in parallel)
	// =============================================
	var (
		tickets          []map[string]interface{}
		vipGuests        []map[string]interface{}
		guestListSignups []map[string]interface{}
		mu               sync.Mutex
		wg               sync.WaitGroup
	)

	wg.Add(3)

	// `event_id`/`status` van dentro de "where" (a nivel raíz se descartaban:
	// se contaban los check-ins de TODOS los eventos del venue, no los del
	// :id pedido). Y `is_checked_in` fuera del select: esa columna no existe
	// en ninguna de las tres tablas, y una columna inexistente en el select da
	// 42703 → lista vacía sin error visible. El check-in real es
	// `checked_in_at` NOT NULL.
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "tickets", map[string]interface{}{
			"select": "id,checked_in_at",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		tickets = result
		mu.Unlock()
	}()

	// TODO(panel-venue): esta query siempre devuelve vacío. `vip_list_guests`
	// no tiene columna `event_id` (cuelga de vip_list_reservations vía
	// `reservation_id`), así que el filtro da 42703. Da igual de momento: el
	// flujo VIP list está RETIRADO (decisión de producto 2026-07). Cuando se
	// limpie el código muerto, esta consulta se va entera.
	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "vip_list_guests", map[string]interface{}{
			"select": "id,checked_in_at",
			"where": map[string]interface{}{
				"event_id": eventID,
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		vipGuests = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := client.QueryCtx(ctx, "guest_list_signups", map[string]interface{}{
			"select": "id,checked_in_at",
			"where": map[string]interface{}{
				"event_id": eventID,
				"status":   "approved",
			},
			"limit": maxVenueScanRows,
		})
		mu.Lock()
		guestListSignups = result
		mu.Unlock()
	}()

	wg.Wait()

	// =============================================
	// PROCESS RESULTS
	// =============================================
	totalTickets := len(tickets)
	checkedIn := 0
	checkInsByHour := make(map[int]int, 24)

	// El check-in es `checked_in_at` NOT NULL, no un booleano `is_checked_in`
	// (columna que no existe). GetBool sobre una clave ausente devolvía
	// siempre false: el endpoint reportaba 0 check-ins pasara lo que pasara.
	for _, ticket := range tickets {
		if services.GetString(ticket, "checked_in_at") != "" {
			checkedIn++
			checkedInAt := services.GetString(ticket, "checked_in_at")
			if t, err := time.Parse(time.RFC3339, checkedInAt); err == nil {
				checkInsByHour[t.Hour()]++
			}
		}
	}

	// Process VIP check-ins
	totalVIP := len(vipGuests)
	vipCheckedIn := 0
	for _, vg := range vipGuests {
		if services.GetString(vg, "checked_in_at") != "" {
			vipCheckedIn++
			checkedInAt := services.GetString(vg, "checked_in_at")
			if t, err := time.Parse(time.RFC3339, checkedInAt); err == nil {
				checkInsByHour[t.Hour()]++
			}
		}
	}

	// Process guest list check-ins
	totalGuestList := len(guestListSignups)
	guestListCheckedIn := 0
	for _, gls := range guestListSignups {
		if services.GetString(gls, "checked_in_at") != "" {
			guestListCheckedIn++
			checkedInAt := services.GetString(gls, "checked_in_at")
			if t, err := time.Parse(time.RFC3339, checkedInAt); err == nil {
				checkInsByHour[t.Hour()]++
			}
		}
	}

	// Build hourly data
	hourlyData := make([]models.HourlyCheckIns, 0)
	for hour := 0; hour < 24; hour++ {
		if count, ok := checkInsByHour[hour]; ok && count > 0 {
			hourlyData = append(hourlyData, models.HourlyCheckIns{
				Hour:     hour,
				CheckIns: count,
			})
		}
	}

	totalAttendees := totalTickets + totalVIP + totalGuestList
	totalCheckedIn := checkedIn + vipCheckedIn + guestListCheckedIn

	checkInRate := 0.0
	if totalAttendees > 0 {
		checkInRate = float64(totalCheckedIn) / float64(totalAttendees) * 100
	}

	c.JSON(http.StatusOK, gin.H{
		"event_id":   eventID,
		"event_name": services.GetString(event, "name"),
		"summary": gin.H{
			"total_attendees":       totalAttendees,
			"total_checked_in":      totalCheckedIn,
			"check_in_rate":         checkInRate,
			"tickets_total":         totalTickets,
			"tickets_checked_in":    checkedIn,
			"vip_total":             totalVIP,
			"vip_checked_in":        vipCheckedIn,
			"guest_list_total":      totalGuestList,
			"guest_list_checked_in": guestListCheckedIn,
		},
		"check_ins_by_hour": hourlyData,
	})
}
