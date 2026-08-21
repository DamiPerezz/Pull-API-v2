package controllers

import (
	"context"
	"net/http"
	"pull-api-v2/services"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// =============================================
// CACHE Y TECHOS DEL PANEL CENTRAL
// =============================================

const (
	// platformDashboardTTL — el dashboard se refresca solo en el panel; 60s
	// basta para que no se note y quita la mayoría de las queries.
	platformDashboardTTL = 60 * time.Second
	// platformRevenueTTL — informes de dinero: cambian despacio y son las
	// queries más caras.
	platformRevenueTTL = 180 * time.Second
	// platformAnalyticsTTL — igual que revenue, pero además agrega usuarios y
	// eventos de toda la plataforma.
	platformAnalyticsTTL = 180 * time.Second

	// maxPlatformScanRows es el techo de filas de cualquier lectura agregada
	// contra la BD central. No es paginación: es una red de seguridad para que
	// una tabla que crece no se traiga entera a memoria (y a la factura de
	// Supabase). Muy por encima del volumen real de hoy.
	maxPlatformScanRows = 20000

	// platformAnalyticsDefaultDays es la ventana por defecto de
	// GetPlatformAnalytics cuando el cliente no pide otra. Antes no había
	// ninguna: leía la tabla `transactions` COMPLETA en cada request.
	platformAnalyticsDefaultDays = 90
	// platformAnalyticsMaxDays acota lo que un cliente puede pedir con ?days=.
	platformAnalyticsMaxDays = 730
)

// platformCacheKey construye la clave de cache de un endpoint del panel.
//
// IMPORTANTE: la clave incluye SIEMPRE el rol y todos los parámetros que
// cambian el resultado. Si dos peticiones con ámbitos distintos comparten
// clave, el segundo lector recibe el snapshot del primero — que en un panel
// multi-tenant es una fuga de datos entre clientes, no un bug de rendimiento.
// Los valores van separados por '|' (no aparece en UUIDs, fechas ni en los
// group_by admitidos), así que dos combinaciones distintas no pueden colapsar
// en la misma cadena.
func platformCacheKey(prefix, role string, parts ...string) string {
	key := prefix + "|role=" + role
	for _, p := range parts {
		key += "|" + p
	}
	return key
}

// =============================================
// PLATFORM DASHBOARD (OPTIMIZED)
// - Parallel queries for all data
// - Single transaction query for all revenue stats
// - Efficient sorting with sort.Slice
// =============================================

// PlatformDashboardResponse represents the platform dashboard data
type PlatformDashboardResponse struct {
	TotalVenues        int                      `json:"total_venues"`
	ActiveVenues       int                      `json:"active_venues"`
	TotalOrganizations int                      `json:"total_organizations"`
	TodayRevenue       float64                  `json:"today_revenue"`
	WeekRevenue        float64                  `json:"week_revenue"`
	MonthRevenue       float64                  `json:"month_revenue"`
	TotalRevenue       float64                  `json:"total_revenue"`
	RecentTransactions []map[string]interface{} `json:"recent_transactions"`
	TopVenues          []map[string]interface{} `json:"top_venues"`
}

// GetPlatformDashboard returns dashboard data for platform admin (OPTIMIZED)
// GET /api/v1/admin/dashboard
func GetPlatformDashboard(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	// super_admin incluido: es el rol más alto del enum y el único que existe
	// hoy en staging/prod — sin él, el panel entero devolvía 403.
	if !requirePlatformRole(c, platformRoleAdmin, platformRoleAnalyst) {
		return
	}
	role := c.GetString("role")

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Central database not available"})
		return
	}

	// Cache: el dashboard son 4 queries a la central y el panel refresca solo.
	// La clave lleva el rol (el ámbito visible depende de él) — ver
	// platformCacheKey.
	cacheKey := platformCacheKey("platform:dashboard", role)
	if cached, ok := services.AppCache.Get(cacheKey); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekStart := todayStart.AddDate(0, 0, -7)

	// Result containers
	var (
		venues     []map[string]interface{}
		orgs       []map[string]interface{}
		allTx      []map[string]interface{}
		recentTx   []map[string]interface{}
		wg         sync.WaitGroup
		mu         sync.Mutex
	)

	// Execute 4 queries in parallel
	wg.Add(4)

	// Query 1: Venues (for counts and name mapping)
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

	// Query 2: Organizations count
	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "organizations", map[string]interface{}{
			"select": "id",
			"limit":  maxPlatformScanRows,
		})
		mu.Lock()
		orgs = result
		mu.Unlock()
	}()

	// Query 3: Completed transactions for last 30 days (optimized for dashboard)
	// PERFORMANCE: Limit to 30 days to avoid loading entire transaction history
	thirtyDaysAgo := time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "transactions", map[string]interface{}{
			"select": "platform_fee_amount,venue_id,created_at",
			"where": map[string]interface{}{
				"status":     "completed",
				"created_at": "gte." + thirtyDaysAgo,
			},
			"limit": maxPlatformScanRows,
		})
		mu.Lock()
		allTx = result
		mu.Unlock()
	}()

	// Query 4: Recent transactions (for display)
	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "transactions", map[string]interface{}{
			"select": "id,venue_id,transaction_type,gross_amount,platform_fee_amount,currency,payment_gateway,status,created_at",
			// Acotado también por fecha: sin el filtro, PostgREST ordena la
			// tabla entera para devolver 10 filas.
			"where": map[string]interface{}{
				"created_at": "gte." + thirtyDaysAgo,
			},
			"order": "created_at.desc",
			"limit": 10,
		})
		mu.Lock()
		recentTx = result
		mu.Unlock()
	}()

	wg.Wait()

	// Build venue name map (pre-allocated)
	venueMap := make(map[string]string, len(venues))
	totalVenues := len(venues)
	activeVenues := 0
	for _, v := range venues {
		id := services.GetString(v, "id")
		venueMap[id] = services.GetString(v, "name")
		if services.GetBool(v, "is_active") {
			activeVenues++
		}
	}

	// Calculate all revenue stats in single pass
	var todayRevenue, weekRevenue, totalRevenue float64
	venueRevenue := make(map[string]float64, len(venues))

	for _, tx := range allTx {
		fee := services.GetFloat64(tx, "platform_fee_amount")
		venueID := services.GetString(tx, "venue_id")
		createdAt := services.GetTime(tx, "created_at")

		// Total (all transactions)
		totalRevenue += fee
		venueRevenue[venueID] += fee

		if createdAt != nil {
			// Week check
			if !createdAt.Before(weekStart) {
				weekRevenue += fee
				// Today check (nested for efficiency)
				if !createdAt.Before(todayStart) {
					todayRevenue += fee
				}
			}
		}
	}

	// Add venue names to recent transactions
	for _, tx := range recentTx {
		venueID := services.GetString(tx, "venue_id")
		tx["venue_name"] = venueMap[venueID]
	}

	// Build top venues with efficient sorting
	topVenues := make([]map[string]interface{}, 0, len(venueRevenue))
	for venueID, revenue := range venueRevenue {
		if revenue > 0 {
			topVenues = append(topVenues, map[string]interface{}{
				"venue_id":   venueID,
				"venue_name": venueMap[venueID],
				"revenue":    revenue,
			})
		}
	}

	// Sort using Go's optimized sort (O(n log n))
	sort.Slice(topVenues, func(i, j int) bool {
		return services.GetFloat64(topVenues[i], "revenue") > services.GetFloat64(topVenues[j], "revenue")
	})

	// Limit to top 5
	if len(topVenues) > 5 {
		topVenues = topVenues[:5]
	}

	resp := PlatformDashboardResponse{
		TotalVenues:        totalVenues,
		ActiveVenues:       activeVenues,
		TotalOrganizations: len(orgs),
		TodayRevenue:       todayRevenue,
		WeekRevenue:        weekRevenue,
		MonthRevenue:       weekRevenue, // Month = week for now (could add separate calc)
		TotalRevenue:       totalRevenue,
		RecentTransactions: recentTx,
		TopVenues:          topVenues,
	}

	services.AppCache.Set(cacheKey, resp, platformDashboardTTL)
	c.JSON(http.StatusOK, resp)
}

// =============================================
// PLATFORM REVENUE (OPTIMIZED)
// - Parallel venue query
// - Pre-allocated maps and slices
// =============================================

// GetPlatformRevenue returns revenue analytics for platform
// GET /api/v1/admin/revenue
func GetPlatformRevenue(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	if !requirePlatformRole(c, platformRoleAdmin, platformRoleAnalyst) {
		return
	}
	role := c.GetString("role")

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Central database not available"})
		return
	}

	// Parse date range
	startDateStr := c.Query("start_date")
	endDateStr := c.Query("end_date")
	groupBy := c.DefaultQuery("group_by", "day")

	// Cache: la clave lleva el rol y TODOS los parámetros que cambian el
	// resultado. Omitir uno haría que dos rangos distintos compartieran
	// snapshot.
	cacheKey := platformCacheKey("platform:revenue", role, startDateStr, endDateStr, groupBy)
	if cached, ok := services.AppCache.Get(cacheKey); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

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

	// Parallel queries
	var (
		transactions []map[string]interface{}
		venues       []map[string]interface{}
		wg           sync.WaitGroup
		mu           sync.Mutex
	)

	wg.Add(2)

	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "transactions", map[string]interface{}{
			"select": "venue_id,gross_amount,platform_fee_amount,payment_gateway,transaction_type,created_at",
			"where": map[string]interface{}{
				"status":     "completed",
				"created_at": "gte." + startDate.Format(time.RFC3339),
			},
			"limit": maxPlatformScanRows,
		})
		mu.Lock()
		transactions = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "venues", map[string]interface{}{
			"select": "id,name",
			"limit":  maxPlatformScanRows,
		})
		mu.Lock()
		venues = result
		mu.Unlock()
	}()

	wg.Wait()

	// Pre-allocate maps with estimated capacity
	days := int(endDate.Sub(startDate).Hours()/24) + 1
	revenueByPeriod := make(map[string]float64, days)
	volumeByPeriod := make(map[string]float64, days)
	countByPeriod := make(map[string]int, days)
	revenueByVenue := make(map[string]float64, len(venues))
	revenueByGateway := make(map[string]float64, 5)
	revenueByType := make(map[string]float64, 10)

	var totalRevenue, totalVolume float64
	var totalCount int

	// Single pass aggregation
	for _, tx := range transactions {
		createdAt := services.GetTime(tx, "created_at")
		if createdAt == nil || createdAt.After(endDate) {
			continue
		}

		fee := services.GetFloat64(tx, "platform_fee_amount")
		amount := services.GetFloat64(tx, "gross_amount")

		// Calculate period key
		var period string
		switch groupBy {
		case "week":
			year, week := createdAt.ISOWeek()
			period = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, (week-1)*7).Format("2006-01-02")
		case "month":
			period = createdAt.Format("2006-01")
		default:
			period = createdAt.Format("2006-01-02")
		}

		revenueByPeriod[period] += fee
		volumeByPeriod[period] += amount
		countByPeriod[period]++

		revenueByVenue[services.GetString(tx, "venue_id")] += fee
		revenueByGateway[services.GetString(tx, "payment_gateway")] += fee
		revenueByType[services.GetString(tx, "transaction_type")] += fee

		totalRevenue += fee
		totalVolume += amount
		totalCount++
	}

	// Build chart data (pre-allocated)
	chartData := make([]map[string]interface{}, 0, days)
	for d := startDate; d.Before(endDate); {
		var period string
		var nextD time.Time
		switch groupBy {
		case "week":
			year, week := d.ISOWeek()
			period = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, (week-1)*7).Format("2006-01-02")
			nextD = d.AddDate(0, 0, 7)
		case "month":
			period = d.Format("2006-01")
			nextD = d.AddDate(0, 1, 0)
		default:
			period = d.Format("2006-01-02")
			nextD = d.AddDate(0, 0, 1)
		}

		chartData = append(chartData, map[string]interface{}{
			"period":       period,
			"revenue":      revenueByPeriod[period],
			"volume":       volumeByPeriod[period],
			"transactions": countByPeriod[period],
		})
		d = nextD
	}

	// Build venue name map
	venueMap := make(map[string]string, len(venues))
	for _, v := range venues {
		venueMap[services.GetString(v, "id")] = services.GetString(v, "name")
	}

	// Build response arrays (pre-allocated)
	venueData := make([]map[string]interface{}, 0, len(revenueByVenue))
	for venueID, revenue := range revenueByVenue {
		venueData = append(venueData, map[string]interface{}{
			"venue_id":   venueID,
			"venue_name": venueMap[venueID],
			"revenue":    revenue,
		})
	}

	gatewayData := make([]map[string]interface{}, 0, len(revenueByGateway))
	for gateway, revenue := range revenueByGateway {
		gatewayData = append(gatewayData, map[string]interface{}{
			"gateway": gateway,
			"revenue": revenue,
		})
	}

	typeData := make([]map[string]interface{}, 0, len(revenueByType))
	for txType, revenue := range revenueByType {
		typeData = append(typeData, map[string]interface{}{
			"type":    txType,
			"revenue": revenue,
		})
	}

	resp := gin.H{
		"period": gin.H{
			"start_date": startDate.Format("2006-01-02"),
			"end_date":   endDate.AddDate(0, 0, -1).Format("2006-01-02"),
			"group_by":   groupBy,
		},
		"summary": gin.H{
			"total_revenue":      totalRevenue,
			"total_volume":       totalVolume,
			"total_transactions": totalCount,
			"avg_fee":            safeDivFloat(totalRevenue, float64(totalCount)),
		},
		"chart_data":         chartData,
		"revenue_by_venue":   venueData,
		"revenue_by_gateway": gatewayData,
		"revenue_by_type":    typeData,
	}

	services.AppCache.Set(cacheKey, resp, platformRevenueTTL)
	c.JSON(http.StatusOK, resp)
}

// safeDivFloat performs safe division
func safeDivFloat(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// =============================================
// PLATFORM TRANSACTIONS (OPTIMIZED)
// - Parallel count and data queries
// - Efficient venue name lookup
// =============================================

// GetPlatformTransactions returns paginated transactions
// GET /api/v1/admin/transactions
func GetPlatformTransactions(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if !requirePlatformRole(c, platformRoleAdmin, platformRoleAnalyst, platformRoleSupport) {
		return
	}

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Central database not available"})
		return
	}

	// Parse filters
	venueID := c.Query("venue_id")
	gateway := c.Query("gateway")
	status := c.Query("status")
	txType := c.Query("type")
	startDateStr := c.Query("start_date")
	endDateStr := c.Query("end_date")

	// Pagination
	page := services.GetQueryInt(c, "page", 1)
	limit := services.GetQueryInt(c, "limit", 50)
	if limit > 100 {
		limit = 100
	}
	offset := (page - 1) * limit

	whereClause := make(map[string]interface{})

	if venueID != "" {
		whereClause["venue_id"] = venueID
	}
	if gateway != "" {
		whereClause["payment_gateway"] = gateway
	}
	if status != "" {
		whereClause["status"] = status
	}
	if txType != "" {
		whereClause["transaction_type"] = txType
	}
	if startDateStr != "" {
		if t, err := time.Parse("2006-01-02", startDateStr); err == nil {
			whereClause["created_at"] = "gte." + t.Format(time.RFC3339)
		}
	}
	if endDateStr != "" {
		if t, err := time.Parse("2006-01-02", endDateStr); err == nil {
			endT := t.Add(24 * time.Hour)
			if _, ok := whereClause["created_at"]; ok {
				whereClause["created_at"] = whereClause["created_at"].(string) + ",lte." + endT.Format(time.RFC3339)
			} else {
				whereClause["created_at"] = "lte." + endT.Format(time.RFC3339)
			}
		}
	}

	// Parallel queries: transactions, count, and venues
	var (
		transactions []map[string]interface{}
		countResult  []map[string]interface{}
		venues       []map[string]interface{}
		txErr        error
		wg           sync.WaitGroup
		mu           sync.Mutex
	)

	wg.Add(3)

	go func() {
		defer wg.Done()
		result, err := central.QueryCtx(ctx, "transactions", map[string]interface{}{
			"select": "*",
			"where":  whereClause,
			"order":  "created_at.desc",
			"limit":  limit,
			"offset": offset,
		})
		mu.Lock()
		transactions = result
		txErr = err
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		// Query de conteo: trae solo `id`, pero seguía siendo un scan sin
		// techo. El límite es una red de seguridad muy por encima del volumen
		// real; si alguna vez se alcanza, `total` se satura en ese número.
		result, _ := central.QueryCtx(ctx, "transactions", map[string]interface{}{
			"select": "id",
			"where":  whereClause,
			"limit":  maxPlatformScanRows,
		})
		mu.Lock()
		countResult = result
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		result, _ := central.QueryCtx(ctx, "venues", map[string]interface{}{
			"select": "id,name",
			"limit":  maxPlatformScanRows,
		})
		mu.Lock()
		venues = result
		mu.Unlock()
	}()

	wg.Wait()

	if txErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch transactions"})
		return
	}

	// Build venue map
	venueMap := make(map[string]string, len(venues))
	for _, v := range venues {
		venueMap[services.GetString(v, "id")] = services.GetString(v, "name")
	}

	// Add venue names
	for _, tx := range transactions {
		vID := services.GetString(tx, "venue_id")
		tx["venue_name"] = venueMap[vID]
	}

	totalCount := len(countResult)

	c.JSON(http.StatusOK, gin.H{
		"transactions": transactions,
		"pagination": gin.H{
			"page":        page,
			"limit":       limit,
			"total":       totalCount,
			"total_pages": (totalCount + limit - 1) / limit,
		},
	})
}

// GetTransactionDetails returns details of a specific transaction
// GET /api/v1/admin/transactions/:id
func GetTransactionDetails(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if !requirePlatformRole(c, platformRoleAdmin, platformRoleAnalyst, platformRoleSupport) {
		return
	}

	transactionID := c.Param("id")

	central := services.DB.Central()
	if central == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Central database not available"})
		return
	}

	// Get transaction
	tx, err := central.QueryOne(ctx, "transactions", map[string]interface{}{
		"select": "*",
		"where": map[string]interface{}{
			"id": transactionID,
		},
	})

	if err != nil || tx == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Transaction not found"})
		return
	}

	// Parallel lookup for venue and org
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		venueID := services.GetString(tx, "venue_id")
		if venue, _ := services.DB.GetVenue(ctx, venueID); venue != nil {
			tx["venue_name"] = venue.Name
			tx["venue_slug"] = venue.Slug
		}
	}()

	go func() {
		defer wg.Done()
		orgID := services.GetString(tx, "organization_id")
		if orgID != "" {
			if org, _ := central.QueryOne(ctx, "organizations", map[string]interface{}{
				"select": "name",
				"where":  map[string]interface{}{"id": orgID},
			}); org != nil {
				tx["organization_name"] = services.GetString(org, "name")
			}
		}
	}()

	wg.Wait()

	c.JSON(http.StatusOK, gin.H{
		"transaction": tx,
	})
}
