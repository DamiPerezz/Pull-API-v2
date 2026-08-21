package services

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"pull-api-v2/config"
	"pull-api-v2/models"
	"sync"
	"time"
)

// venueConfigMisses recuerda por que venues ya avisamos de que no tienen
// configuracion de BD, para que el aviso salga UNA vez por venue y no una por
// peticion: ForVenue esta en el camino caliente de todo el trafico del staff.
var venueConfigMisses sync.Map

// =============================================
// OPTIMIZED DATABASE ROUTER
// Ultra-fast routing with connection pooling
// =============================================

// =============================================
// SHARED HTTP TRANSPORT (CRITICAL FOR PERFORMANCE)
// ALL Supabase clients MUST use this to enable
// TCP connection reuse across all venue databases
// =============================================

var sharedTransport = &http.Transport{
	// Optimized connection pooling for multi-tenant
	MaxIdleConns:        256, // Reduced from 500 (memory optimization)
	MaxIdleConnsPerHost: 64,  // Reduced from 100 (per-host balance)
	MaxConnsPerHost:     128, // Doubled for burst capacity
	IdleConnTimeout:     90 * time.Second, // Reduced from 120s (optimal)

	// Optimized TCP settings
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 60 * time.Second, // Increased from 30s (less TCP overhead)
	}).DialContext,

	// TLS optimization
	TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
	},
	TLSHandshakeTimeout: 5 * time.Second,

	// Fast response handling
	ResponseHeaderTimeout: 10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	DisableCompression:    false,
	ForceAttemptHTTP2:     true,
}

// Shared HTTP client - used by ALL Supabase clients
var sharedHTTPClient = &http.Client{
	Transport: sharedTransport,
	Timeout:   30 * time.Second, // Increased from 15s for large result sets
}

// DatabaseRouter manages connections to venue databases
type DatabaseRouter struct {
	// Central Pull platform database
	central *SupabaseClient

	// Default venue database (legacy/fallback)
	defaultDB *SupabaseClient

	// Venue database connections (cached)
	venueDBs map[string]*SupabaseClient
	dbMu     sync.RWMutex

	// Venue configs (cached with expiry tracking)
	configs      map[string]*cachedConfig
	configMu     sync.RWMutex
	cacheTTL     time.Duration
	cleanupDone  chan struct{}

	// Crypto service for decrypting database credentials
	crypto *CryptoService
}

// cachedConfig wraps config with expiry time
type cachedConfig struct {
	config    *models.VenueDatabaseConfig
	expiresAt time.Time
}

// Global router instance
var DB *DatabaseRouter

// InitDatabaseRouter initializes the database router
func InitDatabaseRouter() error {
	// CRITICAL: Set shared transport BEFORE creating any Supabase clients
	// This enables connection pooling across ALL venue databases
	SetSharedTransport(sharedTransport, sharedHTTPClient)
	log.Println("Shared HTTP transport initialized for connection pooling")

	// Initialize crypto service for decrypting venue credentials
	crypto, err := NewCryptoService(config.App.AppKey)
	if err != nil {
		// SECURITY: In production, APP_KEY is REQUIRED for credential encryption
		if config.IsProduction() {
			log.Fatalf("CRITICAL SECURITY ERROR: APP_KEY is required in production for credential encryption. Error: %v", err)
		}
		log.Printf("WARNING: Crypto service not initialized (APP_KEY may be missing): %v", err)
		log.Printf("WARNING: Payment credentials will NOT be encrypted. This is only acceptable in development!")
	}

	DB = &DatabaseRouter{
		venueDBs:    make(map[string]*SupabaseClient),
		configs:     make(map[string]*cachedConfig),
		cacheTTL:    30 * time.Minute, // Increased from 5 min (venue configs rarely change)
		cleanupDone: make(chan struct{}),
		crypto:      crypto,
	}

	// Set global crypto for use by other packages
	if crypto != nil {
		SetGlobalCrypto(crypto)
	}

	// Initialize central database if configured
	if config.App.CentralSupabaseURL != "" {
		DB.central = newOptimizedClient(
			config.App.CentralSupabaseURL,
			config.App.CentralServiceKey,
			config.App.CentralAnonKey,
		)
		log.Println("Central database (Pull Platform): Connected")
	}

	// Initialize default venue database
	if config.App.DefaultSupabaseURL != "" {
		DB.defaultDB = newOptimizedClient(
			config.App.DefaultSupabaseURL,
			config.App.DefaultServiceKey,
			config.App.DefaultAnonKey,
		)
		DB.venueDBs["default"] = DB.defaultDB
		log.Println("Default venue database: Connected")
	}

	// Validate at least one database is configured
	if DB.central == nil && DB.defaultDB == nil {
		return fmt.Errorf("no database configured")
	}

	// Start background cache cleanup
	go DB.startCacheCleanup()

	return nil
}

// newOptimizedClient creates a client using the shared transport
func newOptimizedClient(baseURL, serviceKey, anonKey string) *SupabaseClient {
	return &SupabaseClient{
		baseURL:    baseURL,
		serviceKey: serviceKey,
		anonKey:    anonKey,
		client:     sharedHTTPClient,
	}
}

// startCacheCleanup periodically cleans expired cache entries
func (r *DatabaseRouter) startCacheCleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.cleanExpiredCache()
		case <-r.cleanupDone:
			return
		}
	}
}

// cleanExpiredCache removes expired cache entries
func (r *DatabaseRouter) cleanExpiredCache() {
	now := time.Now()

	r.configMu.Lock()
	for venueID, cached := range r.configs {
		if now.After(cached.expiresAt) {
			delete(r.configs, venueID)
		}
	}
	r.configMu.Unlock()
}

// Shutdown gracefully stops the router
func (r *DatabaseRouter) Shutdown() {
	close(r.cleanupDone)
}

// =============================================
// ROUTING METHODS (Optimized)
// =============================================

// Central returns the central Pull platform database
func (r *DatabaseRouter) Central() *SupabaseClient {
	if r.central != nil {
		return r.central
	}
	return r.defaultDB
}

// ForVenue returns the database client for a specific venue
// Uses double-checked locking for thread-safe lazy initialization
func (r *DatabaseRouter) ForVenue(venueID string) *SupabaseClient {
	// Fast path: multi-tenant disabled
	if !config.IsMultiTenantEnabled() {
		return r.defaultDB
	}

	// Fast path: check cache with read lock
	r.dbMu.RLock()
	if client, ok := r.venueDBs[venueID]; ok {
		r.dbMu.RUnlock()
		return client
	}
	r.dbMu.RUnlock()

	// Slow path: load config and create connection
	cfg := r.getVenueConfig(venueID)
	if cfg == nil || !cfg.IsActive {
		// ⚠️ CAER AQUI ES PELIGROSO EN CUANTO HAYA MAS DE UN CLIENTE.
		//
		// `defaultDB` es la BD que apuntan las variables DEFAULT_*, que en
		// produccion es la del venue de 511. Si a este venue no se le encuentra
		// configuracion, sus eventos y pedidos acaban DENTRO de los datos de
		// 511, sin error visible para nadie.
		//
		// Hoy es inofensivo porque solo hay un cliente y ese cliente ES el
		// default. Por eso NO se convierte en error todavia: varios handlers
		// dan por hecho que esto nunca devuelve nil, y romperlos a dos semanas
		// del evento del 6-sep es peor que el riesgo que evita.
		//
		// ANTES DE DAR DE ALTA AL CLIENTE NUMERO 2: cambiar esto por
		// `return nil` y revisar que TODOS los llamantes comprueban el nil
		// (el patron correcto ya existe, p.ej. en MobileResetEmployeePassword).
		// Mientras tanto, al menos que se vea en los logs.
		if _, avisado := venueConfigMisses.LoadOrStore(venueID, struct{}{}); !avisado {
			log.Printf("[DatabaseRouter] ⚠️ venue=%s SIN configuracion de BD "+
				"utilizable (existe=%v) — se devuelve la BD POR DEFECTO. Si este "+
				"venue NO es el venue por defecto, sus datos se estan escribiendo "+
				"en la BD de otro cliente. Revisa venue_database_configs. "+
				"(este aviso sale una vez por venue)", venueID, cfg != nil)
		}
		return r.defaultDB
	}

	// Double-check with write lock
	r.dbMu.Lock()
	defer r.dbMu.Unlock()

	// Check again in case another goroutine created it
	if client, ok := r.venueDBs[venueID]; ok {
		return client
	}

	// Create new connection using shared transport
	client := newOptimizedClient(cfg.SupabaseURL, cfg.ServiceKey, cfg.AnonKey)
	r.venueDBs[venueID] = client

	log.Printf("Created database connection for venue %s", venueID)
	return client
}

// Default returns the default venue database
func (r *DatabaseRouter) Default() *SupabaseClient {
	return r.defaultDB
}

// =============================================
// VENUE CONFIG MANAGEMENT (Optimized caching)
// =============================================

// getVenueConfig retrieves venue database config with caching
func (r *DatabaseRouter) getVenueConfig(venueID string) *models.VenueDatabaseConfig {
	now := time.Now()

	// Check cache with read lock
	r.configMu.RLock()
	if cached, ok := r.configs[venueID]; ok && now.Before(cached.expiresAt) {
		r.configMu.RUnlock()
		return cached.config
	}
	r.configMu.RUnlock()

	// Query central database
	if r.central == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := r.central.QueryOne(ctx, "venue_database_configs", map[string]interface{}{
		"select": "id,venue_id,supabase_url,supabase_service_key_encrypted,supabase_anon_key,is_active",
		"where": map[string]interface{}{
			"venue_id":  venueID,
			"is_active": true,
		},
	})

	if err != nil || result == nil {
		return nil
	}

	// Get keys (service key is encrypted, anon key may or may not be)
	serviceKey := GetString(result, "supabase_service_key_encrypted")
	anonKey := GetString(result, "supabase_anon_key")

	// Decrypt service key (always encrypted)
	if r.crypto != nil && serviceKey != "" {
		if decrypted, err := r.crypto.Decrypt(serviceKey); err == nil {
			serviceKey = decrypted
		}
	}

	// Try to decrypt anon key (may or may not be encrypted)
	if r.crypto != nil && anonKey != "" {
		if decrypted, err := r.crypto.Decrypt(anonKey); err == nil {
			anonKey = decrypted
		}
		// If decryption fails, use as-is (not encrypted)
	}

	cfg := &models.VenueDatabaseConfig{
		ID:          GetString(result, "id"),
		VenueID:     GetString(result, "venue_id"),
		SupabaseURL: GetString(result, "supabase_url"),
		ServiceKey:  serviceKey,
		AnonKey:     anonKey,
		IsActive:    GetBool(result, "is_active"),
	}

	// Cache config
	r.configMu.Lock()
	r.configs[venueID] = &cachedConfig{
		config:    cfg,
		expiresAt: now.Add(r.cacheTTL),
	}
	r.configMu.Unlock()

	return cfg
}

// InvalidateCache removes a venue from cache
func (r *DatabaseRouter) InvalidateCache(venueID string) {
	r.dbMu.Lock()
	delete(r.venueDBs, venueID)
	r.dbMu.Unlock()

	r.configMu.Lock()
	delete(r.configs, venueID)
	r.configMu.Unlock()
}

// RemoveVenue removes a venue from cache (alias for InvalidateCache)
// Call this after deleting a venue to clean up resources
func (r *DatabaseRouter) RemoveVenue(venueID string) {
	r.InvalidateCache(venueID)
	log.Printf("Venue %s removed from database router", venueID)
}

// RefreshVenue invalidates cache and establishes a new connection for a venue
// Call this after creating a new venue to make it immediately accessible
func (r *DatabaseRouter) RefreshVenue(venueID string) error {
	// First invalidate any existing cache
	r.InvalidateCache(venueID)

	// Multi-tenant disabled, nothing more to do
	if !config.IsMultiTenantEnabled() {
		return nil
	}

	// Try to establish connection by calling ForVenue
	// This will load the config and create the connection
	client := r.ForVenue(venueID)
	if client == nil {
		return fmt.Errorf("failed to create connection for venue %s", venueID)
	}

	// Verify connection works with a simple health check
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try a simple query to verify connection
	_, err := client.QueryCtx(ctx, "events", map[string]interface{}{
		"select": "id",
		"limit":  1,
	})
	if err != nil {
		// This is ok - the table might not exist yet or be empty
		// The important thing is the connection was established
		log.Printf("Venue %s connection established (query check returned: %v)", venueID, err)
	} else {
		log.Printf("Venue %s connection verified", venueID)
	}

	return nil
}

// =============================================
// CONVENIENCE METHODS (with context)
// =============================================

// GetVenue retrieves venue info from central database
func (r *DatabaseRouter) GetVenue(ctx context.Context, venueID string) (*models.Venue, error) {
	result, err := r.Central().QueryOne(ctx, "venues", map[string]interface{}{
		"select": "*",
		"where": map[string]interface{}{
			"id":         venueID,
			"deleted_at": "is.null",
		},
	})

	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("venue not found")
	}

	return parseVenue(result), nil
}

// GetVenueFees retrieves platform fees for a venue (cached via GetVenue)
func (r *DatabaseRouter) GetVenueFees(ctx context.Context, venueID string) (feePercent, feeFixed float64, err error) {
	venue, err := r.GetVenue(ctx, venueID)
	if err != nil {
		return 0, 0, err
	}
	return venue.PlatformFeePercent, venue.PlatformFeeFixed, nil
}

// RecordTransaction records a transaction in central database
func (r *DatabaseRouter) RecordTransaction(ctx context.Context, tx *models.Transaction) error {
	data := map[string]interface{}{
		"transaction_type":     tx.TransactionType,
		"status":               tx.Status,
		"gross_amount":         tx.GrossAmount,
		"currency":             tx.Currency,
		"platform_fee_percent": tx.PlatformFeePercent,
		"platform_fee_amount":  tx.PlatformFeeAmount,
		"gateway_fee_percent":  tx.GatewayFeePercent,
		"gateway_fee_fixed":    tx.GatewayFeeFixed,
		"gateway_fee_amount":   tx.GatewayFeeAmount,
		"net_to_venue":         tx.NetToVenue,
		"venue_id":             tx.VenueID,
		"organization_id":      tx.OrganizationID,
		"user_id":              tx.UserID,
		"payment_gateway":      tx.PaymentGateway,
	}

	// Optional fields
	if tx.EventID != nil {
		data["event_id"] = *tx.EventID
	}
	if tx.OrderID != nil {
		data["order_id"] = *tx.OrderID
	}
	if tx.GroupReservationID != nil {
		data["group_reservation_id"] = *tx.GroupReservationID
	}
	if tx.VipListID != nil {
		data["vip_list_id"] = *tx.VipListID
	}
	if tx.StripePaymentIntent != nil {
		data["stripe_payment_intent"] = *tx.StripePaymentIntent
	}
	if tx.StripeSessionID != nil {
		data["stripe_session_id"] = *tx.StripeSessionID
	}
	if tx.PayerName != nil {
		data["payer_name"] = *tx.PayerName
	}
	if tx.PayerEmail != nil {
		data["payer_email"] = *tx.PayerEmail
	}
	if tx.CardLast4 != nil {
		data["card_last4"] = *tx.CardLast4
	}
	if tx.CardBrand != nil {
		data["card_brand"] = *tx.CardBrand
	}

	_, err := r.Central().InsertCtx(ctx, "transactions", data)
	return err
}

// =============================================
// HEALTH CHECK (Parallel for speed)
// =============================================

// HealthCheck checks all database connections in parallel
func (r *DatabaseRouter) HealthCheck() map[string]bool {
	results := make(map[string]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Check central
	if r.central != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.central.QueryOne(ctx, "venues", map[string]interface{}{
				"select": "id",
			})
			mu.Lock()
			results["central"] = err == nil
			mu.Unlock()
		}()
	}

	// Check default
	if r.defaultDB != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.defaultDB.QueryOne(ctx, "events", map[string]interface{}{
				"select": "id",
			})
			mu.Lock()
			results["default"] = err == nil
			mu.Unlock()
		}()
	}

	wg.Wait()
	return results
}

// Stats returns router statistics
func (r *DatabaseRouter) Stats() map[string]interface{} {
	r.dbMu.RLock()
	connCount := len(r.venueDBs)
	r.dbMu.RUnlock()

	r.configMu.RLock()
	configCount := len(r.configs)
	r.configMu.RUnlock()

	return map[string]interface{}{
		"central_configured":   r.central != nil,
		"multi_tenant_enabled": config.IsMultiTenantEnabled(),
		"cached_connections":   connCount,
		"cached_configs":       configCount,
		"cache_ttl_minutes":    r.cacheTTL.Minutes(),
	}
}

// =============================================
// HELPER FUNCTIONS
// =============================================

func parseVenue(data map[string]interface{}) *models.Venue {
	venue := &models.Venue{
		ID:             GetString(data, "id"),
		OrganizationID: GetString(data, "organization_id"),
		Name:           GetString(data, "name"),
		Slug:           GetString(data, "slug"),
		Description:    GetString(data, "description"),
		Image:          GetString(data, "image"),
		CoverImage:     GetString(data, "cover_image"),
		Location:       GetString(data, "location"),
		Address:        GetString(data, "address"),
		City:           GetString(data, "city"),
		Country:        GetString(data, "country"),
		OpenTime:       GetString(data, "open_time"),
		CloseTime:      GetString(data, "close_time"),
		Timezone:       GetString(data, "timezone"),
		ContactEmail:   GetString(data, "contact_email"),
		ContactPhone:   GetString(data, "contact_phone"),
		WhatsappNumber: GetString(data, "whatsapp_number"),
		Currency:       GetString(data, "currency"),
		MinAge:         GetInt(data, "min_age"),
		// Legacy feature flags (for backwards compatibility)
		UseVipListFlow:           GetBool(data, "use_vip_list_flow"),
		UseGuestListFlow:         GetBool(data, "use_guest_list_flow"),
		UseIndividualTickets:     GetBool(data, "use_individual_tickets"),
		UseGroupReservations:     GetBool(data, "use_group_reservations"),
		RequireApprovalOrders:    GetBool(data, "require_approval_orders"),
		RequireApprovalGuestList: GetBool(data, "require_approval_guest_list"),
		// Payment
		PaymentGateway:     GetString(data, "payment_gateway"),
		PlatformFeePercent: GetFloat64(data, "platform_fee_percent"),
		PlatformFeeFixed:   GetFloat64(data, "platform_fee_fixed"),
		// Status
		IsActive: GetBool(data, "is_active"),
	}

	// Parse enhanced features from JSONB field if present
	if featuresData, ok := data["features"].(map[string]interface{}); ok {
		venue.Features = parseVenueFeatures(featuresData)
	} else {
		// Generate features from legacy flags for backwards compatibility
		venue.Features = generateFeaturesFromLegacy(venue)
	}

	// Sync legacy flags from features (ensures consistency)
	venue.SyncLegacyFlags()

	return venue
}

// parseVenueFeatures parses the features JSONB field into VenueFeatures struct
func parseVenueFeatures(data map[string]interface{}) models.VenueFeatures {
	features := models.DefaultVenueFeatures()

	if flowType, ok := data["flow_type"].(string); ok {
		features.FlowType = models.VenueFlowType(flowType)
	}

	if modules, ok := data["enabled_modules"].([]interface{}); ok {
		features.EnabledModules = make([]string, 0, len(modules))
		for _, m := range modules {
			if str, ok := m.(string); ok {
				features.EnabledModules = append(features.EnabledModules, str)
			}
		}
	}

	if v, ok := data["default_tab"].(string); ok {
		features.DefaultTab = v
	}
	if v, ok := data["show_all_tabs"].(bool); ok {
		features.ShowAllTabs = v
	}
	if v, ok := data["branding_color"].(string); ok {
		features.BrandingColor = v
	}

	// Booking behavior
	if v, ok := data["requires_approval"].(bool); ok {
		features.RequiresApproval = v
	}
	if v, ok := data["requires_approval_guest_list"].(bool); ok {
		features.RequiresApprovalGuestList = v
	}
	if v, ok := data["requires_approval_vip_list"].(bool); ok {
		features.RequiresApprovalVIPList = v
	}

	// Pricing
	if v, ok := data["gender_based_pricing"].(bool); ok {
		features.GenderBasedPricing = v
	}
	if v, ok := data["service_fee_percent"].(float64); ok {
		features.ServiceFeePercent = v
	}

	// VIP List specific
	if v, ok := data["bottle_service"].(bool); ok {
		features.BottleService = v
	}
	if v, ok := data["payment_deadline_hours"].(float64); ok {
		features.PaymentDeadlineHours = int(v)
	}
	if v, ok := data["allow_guest_self_payment"].(bool); ok {
		features.AllowGuestSelfPayment = v
	}
	if v, ok := data["allow_host_pay_all"].(bool); ok {
		features.AllowHostPayAll = v
	}
	if v, ok := data["min_guests_per_vip_list"].(float64); ok {
		features.MinGuestsPerVIPList = int(v)
	}
	if v, ok := data["max_guests_per_vip_list"].(float64); ok {
		features.MaxGuestsPerVIPList = int(v)
	}

	// Guest List specific
	if v, ok := data["guest_list_auto_approve"].(bool); ok {
		features.GuestListAutoApprove = v
	}
	if v, ok := data["guest_list_capacity_limit"].(float64); ok {
		features.GuestListCapacityLimit = int(v)
	}

	// Ticket features
	if v, ok := data["qr_code_required"].(bool); ok {
		features.QRCodeRequired = v
	}
	if v, ok := data["allow_walk_ins"].(bool); ok {
		features.AllowWalkIns = v
	}
	if v, ok := data["allow_transfers"].(bool); ok {
		features.AllowTransfers = v
	}
	if v, ok := data["allow_resale"].(bool); ok {
		features.AllowResale = v
	}

	// Check-in features
	if v, ok := data["multi_scan_allowed"].(bool); ok {
		features.MultiScanAllowed = v
	}
	if v, ok := data["check_in_window_hours"].(float64); ok {
		features.CheckInWindowHours = int(v)
	}

	// Notifications
	if v, ok := data["send_whatsapp"].(bool); ok {
		features.SendWhatsApp = v
	}
	if v, ok := data["send_email"].(bool); ok {
		features.SendEmail = v
	}
	if v, ok := data["send_push"].(bool); ok {
		features.SendPush = v
	}

	return features
}

// generateFeaturesFromLegacy creates VenueFeatures from legacy boolean flags
// Used when migrating from old format or when features JSONB is not set
func generateFeaturesFromLegacy(venue *models.Venue) models.VenueFeatures {
	features := models.DefaultVenueFeatures()

	// Determine flow type from legacy flags
	if venue.UseVipListFlow {
		features.FlowType = models.FlowTypeVIPList
		features.DefaultTab = "vip_lists"
	} else if venue.UseGuestListFlow && !venue.UseIndividualTickets {
		features.FlowType = models.FlowTypeGuestList
		features.DefaultTab = "guest_lists"
	} else if venue.UseVipListFlow && venue.UseIndividualTickets {
		features.FlowType = models.FlowTypeHybrid
		features.DefaultTab = "orders"
	} else {
		features.FlowType = models.FlowTypeStandard
		features.DefaultTab = "orders"
	}

	// Build enabled modules from legacy flags
	features.EnabledModules = []string{}
	if venue.UseVipListFlow {
		features.EnabledModules = append(features.EnabledModules, "vip_lists")
	}
	if venue.UseGuestListFlow {
		features.EnabledModules = append(features.EnabledModules, "guest_lists")
	}
	if venue.UseIndividualTickets {
		features.EnabledModules = append(features.EnabledModules, "individual_tickets")
	}
	if venue.UseGroupReservations {
		features.EnabledModules = append(features.EnabledModules, "group_reservations")
	}

	// Copy approval settings
	features.RequiresApproval = venue.RequireApprovalOrders
	features.RequiresApprovalGuestList = venue.RequireApprovalGuestList

	return features
}
