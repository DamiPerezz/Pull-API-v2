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
// STAFF DASHBOARD (OPTIMIZED)
// - Single query for all order data
// - Parallel queries for events/recent orders
// - Pre-allocated slices
// =============================================

// DashboardResponse represents the dashboard data
type DashboardResponse struct {
	TodaySales     float64                  `json:"today_sales"`
	TodayOrders    int                      `json:"today_orders"`
	TodayTickets   int                      `json:"today_tickets"`
	WeekSales      float64                  `json:"week_sales"`
	WeekOrders     int                      `json:"week_orders"`
	MonthSales     float64                  `json:"month_sales"`
	UpcomingEvents []map[string]interface{} `json:"upcoming_events"`
	RecentOrders   []map[string]interface{} `json:"recent_orders"`
}

// GetDashboard returns dashboard data for staff (OPTIMIZED)
// GET /api/v1/staff/dashboard
func GetDashboard(c *gin.Context) {
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

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekStart := todayStart.AddDate(0, 0, -7)
	monthStart := todayStart.AddDate(0, -1, 0)

	// Results containers
	var (
		monthOrders    []map[string]interface{}
		upcomingEvents []map[string]interface{}
		recentOrders   []map[string]interface{}
		wg             sync.WaitGroup
		mu             sync.Mutex
	)

	// Execute 3 queries in parallel
	wg.Add(3)

	// Query 1: All orders from month (single query for today/week/month stats)
	go func() {
		defer wg.Done()
		result, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "total,quantity,created_at",
			"where": map[string]interface{}{
				// Ver estados_orden.go: sin checked_in, los ingresos de hoy /
				// semana / mes BAJABAN según la gente entraba al local.
				"status":     estadosCobrados,
				"created_at": "gte." + monthStart.Format(time.RFC3339),
			},
		})
		if err == nil {
			mu.Lock()
			monthOrders = result
			mu.Unlock()
		}
	}()

	// Query 2: Upcoming events
	go func() {
		defer wg.Done()
		result, _ := venueDB.QueryCtx(ctx, "events", map[string]interface{}{
			"select": "id,name,slug,event_date,start_time,end_time,ticket_limit",
			"where": map[string]interface{}{
				"event_date":   "gte." + now.Format("2006-01-02"),
				"is_active":    true,
				"is_published": true,
				"deleted_at":   "is.null",
			},
			"order": "event_date.asc,start_time.asc",
			"limit": 5,
		})
		mu.Lock()
		upcomingEvents = result
		mu.Unlock()
	}()

	// Query 3: Recent orders
	go func() {
		defer wg.Done()
		result, _ := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "id,order_number,user_email,user_name,total,currency,status,quantity,created_at",
			"order":  "created_at.desc",
			"limit":  10,
		})
		mu.Lock()
		recentOrders = result
		mu.Unlock()
	}()

	wg.Wait()

	// Calculate stats from single month query (no additional loops)
	var todaySales, weekSales, monthSales float64
	var todayOrders, weekOrders, todayTickets int

	for _, order := range monthOrders {
		createdAt := services.GetTime(order, "created_at")
		if createdAt == nil {
			continue
		}

		total := services.GetFloat64(order, "total")
		qty := services.GetInt(order, "quantity")

		// Month stats (all orders in result)
		monthSales += total

		// Week stats
		if !createdAt.Before(weekStart) {
			weekSales += total
			weekOrders++

			// Today stats
			if !createdAt.Before(todayStart) {
				todaySales += total
				todayOrders++
				todayTickets += qty
			}
		}
	}

	c.JSON(http.StatusOK, DashboardResponse{
		TodaySales:     todaySales,
		TodayOrders:    todayOrders,
		TodayTickets:   todayTickets,
		WeekSales:      weekSales,
		WeekOrders:     weekOrders,
		MonthSales:     monthSales,
		UpcomingEvents: upcomingEvents,
		RecentOrders:   recentOrders,
	})
}

// =============================================
// ANALYTICS (OPTIMIZED)
// - Parallel queries for tickets
// - Pre-allocated maps
// =============================================

// GetAnalytics returns analytics data for staff
// GET /api/v1/staff/analytics
func GetAnalytics(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
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

	// Parse date range
	startDateStr := c.Query("start_date")
	endDateStr := c.Query("end_date")

	now := time.Now()
	startDate := now.AddDate(0, 0, -30)
	endDate := now

	if startDateStr != "" {
		if t, err := time.Parse("2006-01-02", startDateStr); err == nil {
			startDate = t
		}
	}
	if endDateStr != "" {
		if t, err := time.Parse("2006-01-02", endDateStr); err == nil {
			endDate = t.Add(24 * time.Hour)
		}
	}

	dateFilter := "gte." + startDate.Format(time.RFC3339)

	// Execute queries in parallel
	var (
		orders          []map[string]interface{}
		ticketsByType   []map[string]interface{}
		ticketsBySource []map[string]interface{}
		wg              sync.WaitGroup
		mu              sync.Mutex
	)

	wg.Add(3)

	// Query 1: Orders
	go func() {
		defer wg.Done()
		result, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "total,created_at",
			"where": map[string]interface{}{
				// Ver estados_orden.go.
				"status":     estadosCobrados,
				"created_at": dateFilter,
			},
		})
		if err == nil {
			mu.Lock()
			orders = result
			mu.Unlock()
		}
	}()

	// Query 2: Tickets by type (using RPC for aggregation if available, else client-side)
	go func() {
		defer wg.Done()
		result, _ := venueDB.QueryCtx(ctx, "tickets", map[string]interface{}{
			"select": "ticket_type_name",
			"where": map[string]interface{}{
				"created_at": dateFilter,
			},
		})
		// Group client-side (Supabase REST doesn't support GROUP BY directly)
		counts := make(map[string]int)
		for _, t := range result {
			name := services.GetString(t, "ticket_type_name")
			counts[name]++
		}
		grouped := make([]map[string]interface{}, 0, len(counts))
		for name, count := range counts {
			grouped = append(grouped, map[string]interface{}{
				"ticket_type_name": name,
				"count":            count,
			})
		}
		mu.Lock()
		ticketsByType = grouped
		mu.Unlock()
	}()

	// Query 3: Tickets by source
	go func() {
		defer wg.Done()
		result, _ := venueDB.QueryCtx(ctx, "tickets", map[string]interface{}{
			"select": "source",
			"where": map[string]interface{}{
				"created_at": dateFilter,
			},
		})
		counts := make(map[string]int)
		for _, t := range result {
			source := services.GetString(t, "source")
			counts[source]++
		}
		grouped := make([]map[string]interface{}, 0, len(counts))
		for source, count := range counts {
			grouped = append(grouped, map[string]interface{}{
				"source": source,
				"count":  count,
			})
		}
		mu.Lock()
		ticketsBySource = grouped
		mu.Unlock()
	}()

	wg.Wait()

	// Pre-allocate maps with estimated capacity
	days := int(endDate.Sub(startDate).Hours()/24) + 1
	salesByDay := make(map[string]float64, days)
	ordersByDay := make(map[string]int, days)

	// Single pass aggregation
	totalSales := 0.0
	totalOrders := 0
	for _, order := range orders {
		createdAt := services.GetTime(order, "created_at")
		if createdAt != nil && createdAt.Before(endDate) {
			total := services.GetFloat64(order, "total")
			day := createdAt.Format("2006-01-02")
			salesByDay[day] += total
			ordersByDay[day]++
			totalSales += total
			totalOrders++
		}
	}

	// Build chart data with pre-allocated slice
	salesData := make([]map[string]interface{}, 0, days)
	for d := startDate; d.Before(endDate); d = d.AddDate(0, 0, 1) {
		day := d.Format("2006-01-02")
		salesData = append(salesData, map[string]interface{}{
			"date":   day,
			"sales":  salesByDay[day],
			"orders": ordersByDay[day],
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"period": gin.H{
			"start_date": startDate.Format("2006-01-02"),
			"end_date":   endDate.AddDate(0, 0, -1).Format("2006-01-02"),
		},
		"summary": gin.H{
			"total_sales":  totalSales,
			"total_orders": totalOrders,
			"avg_order":    safeDiv(totalSales, float64(totalOrders)),
		},
		"sales_by_day":      salesData,
		"tickets_by_type":   ticketsByType,
		"tickets_by_source": ticketsBySource,
	})
}

// safeDiv performs safe division, returning 0 if divisor is 0
func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
