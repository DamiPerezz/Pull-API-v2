package main

import (
	"log"
	"pull-api-v2/config"
	"pull-api-v2/controllers"
	"pull-api-v2/middleware"
	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

func main() {
	// Load configuration
	config.Load()

	// Set Gin mode
	if config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
		log.Println("Gin: Release mode")
	} else {
		log.Println("Gin: Debug mode")
	}

	// Initialize security FIRST
	middleware.InitSecurity()
	log.Println("Security: Initialized")

	// Initialize services
	services.InitCache()
	log.Println("Cache: Initialized")

	// Initialize background task queue (bounded worker pool)
	services.InitTaskQueue(0) // 0 = auto-detect workers based on CPU

	if err := services.InitDatabaseRouter(); err != nil {
		log.Fatal("Failed to initialize database router:", err)
	}

	if err := services.InitPaymentRouter(); err != nil {
		log.Printf("Warning: Payment router init failed: %v", err)
	}

	// Initialize email service
	if err := services.InitEmailService(); err != nil {
		log.Printf("Warning: Email service init failed: %v", err)
	}

	// Initialize PDF service (renders tickets with embedded QR codes that
	// the SendTickets email attaches).
	if err := services.InitPDFService(); err != nil {
		log.Printf("Warning: PDF service init failed: %v", err)
	}

	// Initialize Expo push service (notifies staff devices on new orders).
	services.InitPushService()

	// Start the 48h approval-expiry sweep for held private-event payments.
	controllers.StartApprovalExpiryJob()

	// Create router with custom recovery
	router := gin.New()
	router.Use(gin.Logger())
	router.Use(middleware.SafeRecovery())

	// Configure trusted proxies
	if config.IsProduction() {
		router.SetTrustedProxies([]string{
			"127.0.0.1",
			"10.0.0.0/8",
			"172.16.0.0/12",
			"192.168.0.0/16",
		})
	} else {
		router.SetTrustedProxies(nil)
	}

	// =============================================
	// SECURITY MIDDLEWARE CHAIN
	// Order matters: each layer adds protection
	// =============================================

	// 1. Request ID for tracing
	router.Use(middleware.RequestID())

	// 2. IP block check (before any processing)
	router.Use(middleware.IPBlockCheck())

	// 3. Body size limit (prevent large payload attacks)
	router.Use(middleware.BodySizeLimit(10 * 1024 * 1024)) // 10MB

	// 4. Security headers (HSTS, CSP, X-Frame-Options, etc.)
	router.Use(middleware.SecureHeaders())

	// 5. Secure CORS
	router.Use(middleware.SecureCORS())

	// 6. Input sanitization (detect injection patterns)
	router.Use(middleware.SanitizeInput())

	// 7. Gzip compression for responses (only in production for performance)
	if config.IsProduction() {
		router.Use(middleware.GzipCompressionFast())
	}

	// Health check
	router.GET("/health", healthCheck)

	// API v1 routes
	v1 := router.Group("/api/v1")
	{
		setupAuthRoutes(v1)
		setupVenueRoutes(v1)
		setupEventRoutes(v1)
		setupOrderRoutes(v1)
		setupTicketRoutes(v1)
		setupVIPListRoutes(v1)
		setupGuestListRoutes(v1)
		setupStaffRoutes(v1)
		setupPlatformRoutes(v1)
		setupLegacyRoutes(v1)
		setupMobileRoutes(v1)
	}

	// Webhooks (outside /api/v1)
	setupWebhookRoutes(router)

	// Start server
	port := config.App.Port
	log.Printf("Server starting on port %s", port)
	log.Printf("API: http://localhost:%s/api/v1", port)
	log.Printf("Health: http://localhost:%s/health", port)

	if err := router.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}

// =============================================
// NOTE: Security middleware moved to middleware/security.go
// =============================================

// =============================================
// HEALTH CHECK
// =============================================

func healthCheck(c *gin.Context) {
	dbHealth := services.DB.HealthCheck()

	status := "ok"
	for _, healthy := range dbHealth {
		if !healthy {
			status = "degraded"
			break
		}
	}

	stats := gin.H{
		"database": services.DB.Stats(),
		"payments": services.Payments.Stats(),
	}

	// Add task queue stats if available
	if services.BackgroundTasks != nil {
		stats["task_queue"] = services.BackgroundTasks.Stats()
	}

	c.JSON(200, gin.H{
		"status":    status,
		"service":   "Pull API v2",
		"databases": dbHealth,
		"stats":     stats,
	})
}

// =============================================
// ROUTE SETUP
// =============================================

func setupAuthRoutes(v1 *gin.RouterGroup) {
	// Staff authentication
	auth := v1.Group("/auth")
	{
		auth.POST("/login-staff", middleware.RateLimitAuth(), controllers.LoginStaff)
		auth.POST("/login-workers", middleware.RateLimitAuth(), controllers.LoginStaff) // Alias for mobile app
		auth.GET("/verify", middleware.AuthenticateStaff(), controllers.VerifyToken)
		auth.GET("/verify-token", middleware.AuthenticateStaff(), controllers.VerifyToken) // Alias for mobile app
		auth.POST("/refresh", middleware.AuthenticateStaff(), controllers.RefreshToken)
		auth.POST("/refresh-staff-token", middleware.AuthenticateStaff(), controllers.RefreshToken) // Alias for mobile app
	}

	// User authentication (email code-based)
	userAuth := v1.Group("/user-auth")
	{
		userAuth.POST("/request-code", middleware.RateLimitAuth(), controllers.RequestCode)
		userAuth.POST("/verify-code", middleware.RateLimitAuth(), controllers.VerifyCode)
		userAuth.GET("/profile", middleware.AuthenticateUser(), controllers.GetUserProfile)
		userAuth.PUT("/profile", middleware.AuthenticateUser(), controllers.UpdateUserProfile)
	}
}

func setupVenueRoutes(v1 *gin.RouterGroup) {
	// Public venue endpoints
	venues := v1.Group("/venues")
	{
		venues.GET("", middleware.RateLimitGeneral(), controllers.GetVenues)
		venues.GET("/:slug", middleware.RateLimitGeneral(), controllers.GetVenue)
		venues.GET("/:slug/events", middleware.RateLimitGeneral(), controllers.GetVenueEvents)
	}

	// Staff venue management
	venue := v1.Group("/venue")
	venue.Use(middleware.AuthenticateStaff())
	{
		venue.GET("/info", controllers.GetVenueInfo)
		venue.PUT("/update", controllers.UpdateVenue)
	}
}

func setupEventRoutes(v1 *gin.RouterGroup) {
	// Public event endpoints
	events := v1.Group("/events")
	{
		events.GET("", middleware.RateLimitGeneral(), controllers.GetEvents)
		events.GET("/:slug", middleware.RateLimitGeneral(), controllers.GetEvent)
		events.GET("/:slug/tickets", middleware.RateLimitGeneral(), controllers.GetEventTickets)
	}

	// Staff event management
	eventAdmin := v1.Group("/event")
	eventAdmin.Use(middleware.AuthenticateStaff())
	{
		eventAdmin.POST("", controllers.CreateEvent)
		eventAdmin.PUT("/:id", middleware.ValidateUUIDParam("id"), controllers.UpdateEvent)
		eventAdmin.DELETE("/:id", middleware.ValidateUUIDParam("id"), controllers.DeleteEvent)
		eventAdmin.POST("/:id/ticket-types", middleware.ValidateUUIDParam("id"), controllers.CreateTicketType)
	}

	// Staff events list
	staffEvents := v1.Group("/staff/events")
	staffEvents.Use(middleware.AuthenticateStaff())
	{
		staffEvents.GET("", controllers.GetStaffEvents)
		staffEvents.GET("/:id/tickets", middleware.ValidateUUIDParam("id"), controllers.GetStaffEventTickets)
	}
}

func setupOrderRoutes(v1 *gin.RouterGroup) {
	// Public order endpoints
	orders := v1.Group("/orders")
	{
		orders.POST("/create", middleware.RateLimitCreate(), controllers.CreateOrder)
		orders.POST("/checkout", middleware.RateLimitPayment(), controllers.CreateCheckout)
		orders.GET("/confirm", controllers.ConfirmPayment)
		// Demo checkout HTML page served by the API when DEMO_MODE is on.
		orders.GET("/demo-checkout", controllers.DemoCheckoutPage)
		orders.GET("/:code", controllers.GetOrder)
	}

	// Staff order management
	orderAdmin := v1.Group("/orders-admin")
	orderAdmin.Use(middleware.AuthenticateStaff())
	{
		orderAdmin.GET("/venue", controllers.GetVenueOrders)
		orderAdmin.POST("/:id/approve", middleware.ValidateUUIDParam("id"), controllers.ApproveOrder)
		orderAdmin.POST("/:id/reject", middleware.ValidateUUIDParam("id"), controllers.RejectOrder)
	}
}

func setupTicketRoutes(v1 *gin.RouterGroup) {
	// User ticket endpoints
	tickets := v1.Group("/tickets")
	tickets.Use(middleware.AuthenticateUser())
	{
		tickets.GET("/my", controllers.GetMyTickets)
		tickets.GET("/:id/pdf", middleware.ValidateUUIDParam("id"), controllers.GetTicketPDF)
	}

	// Public ticket lookup (by QR)
	v1.GET("/tickets/qr/:token", middleware.RateLimitGeneral(), controllers.GetTicketByQR)

	// Staff ticket validation
	validation := v1.Group("/validate")
	validation.Use(middleware.AuthenticateStaff())
	{
		validation.POST("/ticket", controllers.ValidateTicket)
	}

	// Staff ticket management
	staffTickets := v1.Group("/staff/tickets")
	staffTickets.Use(middleware.AuthenticateStaff())
	{
		staffTickets.POST("/:id/undo-checkin", middleware.ValidateUUIDParam("id"), controllers.UndoCheckIn)
	}

	// Manual check-in
	v1.POST("/staff/events/:id/manual-checkin", middleware.AuthenticateStaff(), middleware.ValidateUUIDParam("id"), controllers.ManualCheckIn)
}

func setupStaffRoutes(v1 *gin.RouterGroup) {
	staff := v1.Group("/staff")
	staff.Use(middleware.AuthenticateStaff())
	{
		staff.GET("/dashboard", controllers.GetDashboard)
		staff.GET("/dashboard/stats", controllers.GetDashboardStats)
		staff.GET("/analytics", controllers.GetAnalytics)
		staff.GET("/analytics/venue", controllers.GetVenueAnalytics)
		staff.GET("/analytics/sales", controllers.GetSalesByPeriod)
		staff.GET("/reports/revenue", controllers.GetRevenueReport)
	}

	// Staff event analytics
	staffEventAnalytics := v1.Group("/staff/events")
	staffEventAnalytics.Use(middleware.AuthenticateStaff())
	{
		staffEventAnalytics.GET("/:id/analytics", middleware.ValidateUUIDParam("id"), controllers.GetEventAnalytics)
		staffEventAnalytics.GET("/:id/check-ins/analytics", middleware.ValidateUUIDParam("id"), controllers.GetCheckInAnalytics)
	}

	// Staff notifications
	staffNotifications := v1.Group("/staff/notifications")
	staffNotifications.Use(middleware.AuthenticateStaff())
	{
		staffNotifications.GET("", controllers.GetStaffNotifications)
		staffNotifications.GET("/unread-count", controllers.GetUnreadCount)
		staffNotifications.PATCH("/:id/read", middleware.ValidateUUIDParam("id"), controllers.MarkNotificationRead)
		staffNotifications.POST("/read-all", controllers.MarkAllNotificationsRead)
		staffNotifications.PATCH("/:id/archive", middleware.ValidateUUIDParam("id"), controllers.ArchiveNotification)
		staffNotifications.GET("/preferences", controllers.GetNotificationPreferences)
		staffNotifications.PUT("/preferences", controllers.UpdateNotificationPreferences)
		staffNotifications.POST("/push-token", controllers.RegisterPushToken)
		staffNotifications.DELETE("/push-token/:device_id", controllers.UnregisterPushToken)
	}

	// User notifications
	userNotifications := v1.Group("/users/notifications")
	userNotifications.Use(middleware.AuthenticateUser())
	{
		userNotifications.GET("", controllers.GetUserNotifications)
		userNotifications.PATCH("/:id/read", middleware.ValidateUUIDParam("id"), controllers.MarkUserNotificationRead)
		userNotifications.POST("/push-token", controllers.RegisterUserPushToken)
	}

	// Staff notifications - Mobile app compatible paths
	staffNotificationsMobile := v1.Group("/staff-notifications")
	staffNotificationsMobile.Use(middleware.AuthenticateStaff())
	{
		staffNotificationsMobile.GET("/venue/:venueId", middleware.ValidateUUIDParam("venueId"), controllers.GetVenueStaffNotifications)
		staffNotificationsMobile.GET("/search/:venueId", middleware.ValidateUUIDParam("venueId"), controllers.SearchStaffNotifications)
	}

	employees := v1.Group("/employees")
	employees.Use(middleware.AuthenticateStaff())
	{
		employees.GET("", controllers.GetEmployees)
		employees.POST("", controllers.CreateEmployee)
		employees.PUT("/:id", middleware.ValidateUUIDParam("id"), controllers.UpdateEmployee)
		employees.DELETE("/:id", middleware.ValidateUUIDParam("id"), controllers.DeleteEmployee)
	}

	// Roles endpoint
	v1.GET("/roles", middleware.AuthenticateStaff(), controllers.GetRoles)
}

func setupVIPListRoutes(v1 *gin.RouterGroup) {
	// Public VIP list endpoints
	vipLists := v1.Group("/vip-lists")
	{
		// Create VIP list (can be authenticated or not)
		vipLists.POST("", middleware.RateLimitCreate(), controllers.CreateVIPList)

		// Get VIP list by QR token (public for check-in screens)
		vipLists.GET("/qr/:token", middleware.RateLimitGeneral(), controllers.GetVIPListByQR)

		// Guest confirms attendance (public with token in URL)
		vipLists.POST("/guests/:guest_id/confirm", middleware.RateLimitGeneral(), controllers.ConfirmGuestAttendance)
	}

	// Authenticated user VIP list endpoints
	userVipLists := v1.Group("/vip-lists")
	userVipLists.Use(middleware.AuthenticateUser())
	{
		userVipLists.GET("/my", controllers.GetMyVIPLists)
		userVipLists.GET("/:id", middleware.ValidateUUIDParam("id"), controllers.GetVIPList)
		userVipLists.POST("/:id/guests", middleware.ValidateUUIDParam("id"), controllers.AddGuestToVIPList)
		userVipLists.DELETE("/:id/guests/:guest_id", middleware.ValidateUUIDParam("id"), middleware.ValidateUUIDParam("guest_id"), controllers.RemoveGuestFromVIPList)
	}

	// Staff VIP list management (matches mobile app expectations)
	staffVipLists := v1.Group("/vip-lists")
	staffVipLists.Use(middleware.AuthenticateStaff())
	{
		// Create VIP list (staff version with full host info)
		staffVipLists.POST("/create", controllers.CreateVIPListStaff)

		// List VIP lists for a venue with filters and pagination
		staffVipLists.GET("/venue/:venueId", middleware.ValidateUUIDParam("venueId"), controllers.GetVenueVIPLists)

		// Get detailed VIP list info
		staffVipLists.GET("/detail/:id", middleware.ValidateUUIDParam("id"), controllers.GetVIPListDetail)

		// Get bottles selected for a VIP list
		staffVipLists.GET("/:id/bottles", middleware.ValidateUUIDParam("id"), controllers.GetVIPListBottles)

		// VIP list lifecycle management
		staffVipLists.POST("/:id/close", middleware.ValidateUUIDParam("id"), controllers.CloseVIPList)
		staffVipLists.POST("/:id/finalize", middleware.ValidateUUIDParam("id"), controllers.FinalizeVIPList)
	}

	// Staff VIP list actions (legacy paths kept for compatibility)
	staffVipListsLegacy := v1.Group("/staff/vip-lists")
	staffVipListsLegacy.Use(middleware.AuthenticateStaff())
	{
		staffVipListsLegacy.POST("/check-in", controllers.CheckInVIPGuest)
		staffVipListsLegacy.POST("/:id/approve", middleware.ValidateUUIDParam("id"), controllers.ApproveVIPList)
		staffVipListsLegacy.POST("/:id/reject", middleware.ValidateUUIDParam("id"), controllers.RejectVIPList)
		staffVipListsLegacy.POST("/guests/:id/undo-checkin", middleware.ValidateUUIDParam("id"), controllers.UndoVIPGuestCheckIn)
	}

	// Staff event VIP lists
	v1.GET("/staff/events/:id/vip-lists", middleware.AuthenticateStaff(), middleware.ValidateUUIDParam("id"), controllers.GetEventVIPLists)
}

func setupGuestListRoutes(v1 *gin.RouterGroup) {
	// Public guest list endpoints
	guestLists := v1.Group("/guest-lists")
	{
		// NOTE: POST /signup is registered in setupLegacyRoutes with the
		// PullWebApp-GL shape; we don't double-register here.

		// Get signup by QR token
		guestLists.GET("/qr/:token", middleware.RateLimitGeneral(), controllers.GetGuestListSignupByQR)

		// Get available guest lists for an event.
		// NOTE: the legacy compat route in setupLegacyRoutes also registers
		// /guest-lists/event/:eventSlug; we don't double-register here.
	}

	// Authenticated user guest list endpoints
	userGuestLists := v1.Group("/guest-lists")
	userGuestLists.Use(middleware.AuthenticateUser())
	{
		userGuestLists.GET("/my", controllers.GetMyGuestListSignups)
	}

	// Staff guest list management (mobile app paths)
	staffGuestListsMobile := v1.Group("/guest-lists")
	staffGuestListsMobile.Use(middleware.AuthenticateStaff())
	{
		// Pending signups for venue
		staffGuestListsMobile.GET("/venue/:venueId/pending", middleware.ValidateUUIDParam("venueId"), controllers.GetVenuePendingSignups)

		// Get specific signup
		staffGuestListsMobile.GET("/signup/:signupId", middleware.ValidateUUIDParam("signupId"), controllers.GetGuestListSignup)

		// Approve/Reject single signup (mobile app paths)
		staffGuestListsMobile.POST("/:signupId/approve", middleware.ValidateUUIDParam("signupId"), controllers.ApproveGuestListSignup)
		staffGuestListsMobile.POST("/:signupId/reject", middleware.ValidateUUIDParam("signupId"), controllers.RejectGuestListSignup)

		// Batch operations
		staffGuestListsMobile.POST("/batch/approve", controllers.BatchApproveGuestListSignups)
		staffGuestListsMobile.POST("/batch/reject", controllers.BatchRejectGuestListSignups)

		// Guest list types management
		staffGuestListsMobile.GET("/types/event/:eventId", middleware.ValidateUUIDParam("eventId"), controllers.GetEventGuestLists)
		staffGuestListsMobile.POST("/types", controllers.CreateGuestListType)
		staffGuestListsMobile.PUT("/types/:typeId", middleware.ValidateUUIDParam("typeId"), controllers.UpdateGuestListType)
		staffGuestListsMobile.DELETE("/types/:typeId", middleware.ValidateUUIDParam("typeId"), controllers.DeleteGuestListType)
	}

	// Staff guest list management (legacy paths)
	staffGuestLists := v1.Group("/staff/guest-lists")
	staffGuestLists.Use(middleware.AuthenticateStaff())
	{
		// Guest list types management
		staffGuestLists.POST("/types", controllers.CreateGuestListType)
		staffGuestLists.PUT("/types/:id", middleware.ValidateUUIDParam("id"), controllers.UpdateGuestListType)
		staffGuestLists.DELETE("/types/:id", middleware.ValidateUUIDParam("id"), controllers.DeleteGuestListType)

		// Signup management
		staffGuestLists.GET("/event/:event_id/signups", middleware.ValidateUUIDParam("event_id"), controllers.GetEventGuestListSignups)
		staffGuestLists.POST("/signups/:id/approve", middleware.ValidateUUIDParam("id"), controllers.ApproveGuestListSignup)
		staffGuestLists.POST("/signups/:id/reject", middleware.ValidateUUIDParam("id"), controllers.RejectGuestListSignup)
		staffGuestLists.POST("/check-in", controllers.CheckInGuestListSignup)
		staffGuestLists.POST("/signups/:id/undo-checkin", middleware.ValidateUUIDParam("id"), controllers.UndoGuestListCheckIn)
	}
}

func setupPlatformRoutes(v1 *gin.RouterGroup) {
	// Platform authentication (legacy - kept for compatibility)
	platform := v1.Group("/platform")
	{
		platform.POST("/login", middleware.RateLimitAuth(), controllers.PlatformLogin)
	}

	// =============================================
	// SECURE PLATFORM AUTH (Cookie-based with sessions)
	// Uses HTTP-only cookies with access/refresh tokens
	// =============================================
	platformAuth := v1.Group("/platform/auth")
	{
		// Public endpoints
		platformAuth.POST("/login", middleware.RateLimitAuth(), controllers.PlatformSecureLogin)
		platformAuth.POST("/refresh", middleware.RateLimitAuth(), controllers.PlatformRefreshToken)

		// Protected endpoints (require valid session)
		platformAuth.POST("/logout", middleware.AuthenticatePlatformSecure(), controllers.PlatformLogout)
		platformAuth.POST("/logout-all", middleware.AuthenticatePlatformSecure(), controllers.PlatformLogoutAll)
		platformAuth.GET("/sessions", middleware.AuthenticatePlatformSecure(), controllers.PlatformGetSessions)
		platformAuth.DELETE("/sessions/:session_id", middleware.AuthenticatePlatformSecure(), controllers.PlatformRevokeSession)
		platformAuth.GET("/me", middleware.AuthenticatePlatformSecure(), controllers.PlatformGetCurrentUser)
	}

	// Platform admin routes (legacy - uses Bearer token)
	admin := v1.Group("/admin")
	admin.Use(middleware.AuthenticatePlatform())
	{
		admin.GET("/dashboard", controllers.GetPlatformDashboard)
		admin.GET("/analytics", controllers.GetPlatformAnalytics)
		admin.GET("/venues", controllers.GetAllVenues)
		admin.POST("/venues", controllers.CreateVenue)
		admin.POST("/venues/full", controllers.CreateVenueFull) // Full venue creation with org + db config
		admin.PUT("/venues/:id/fees", middleware.ValidateUUIDParam("id"), controllers.UpdateVenueFees)
		admin.GET("/revenue", controllers.GetPlatformRevenue)
		admin.GET("/transactions", controllers.GetPlatformTransactions)
		admin.GET("/transactions/:id", middleware.ValidateUUIDParam("id"), controllers.GetTransactionDetails)
	}

	// =============================================
	// SECURE ADMIN ROUTES (Cookie-based auth)
	// New secure routes for admin dashboard
	// =============================================
	secureAdmin := v1.Group("/secure-admin")
	secureAdmin.Use(middleware.AuthenticatePlatformSecure())
	{
		secureAdmin.GET("/dashboard", controllers.GetPlatformDashboard)
		secureAdmin.GET("/analytics", controllers.GetPlatformAnalytics)
		secureAdmin.GET("/venues", controllers.GetAllVenues)
		secureAdmin.GET("/venues/:id", middleware.ValidateUUIDParam("id"), controllers.GetVenueById)
		secureAdmin.PUT("/venues/:id", middleware.ValidateUUIDParam("id"), controllers.UpdateVenueAdmin)
		secureAdmin.POST("/venues", controllers.CreateVenue)
		secureAdmin.POST("/venues/full", controllers.CreateVenueFull)
		secureAdmin.PUT("/venues/:id/fees", middleware.ValidateUUIDParam("id"), controllers.UpdateVenueFees)
		secureAdmin.DELETE("/venues/:id", middleware.ValidateUUIDParam("id"), controllers.DeleteVenue)
		secureAdmin.GET("/revenue", controllers.GetPlatformRevenue)
		secureAdmin.GET("/transactions", controllers.GetPlatformTransactions)
		secureAdmin.GET("/transactions/:id", middleware.ValidateUUIDParam("id"), controllers.GetTransactionDetails)

		// Storage/Images routes
		secureAdmin.POST("/venues/:id/images", middleware.ValidateUUIDParam("id"), controllers.UploadVenueImage)
		secureAdmin.GET("/venues/:id/images/sign", middleware.ValidateUUIDParam("id"), controllers.GetVenueImageURL)
		secureAdmin.DELETE("/venues/:id/images", middleware.ValidateUUIDParam("id"), controllers.DeleteVenueImage)
		secureAdmin.POST("/venues/:id/images/upload-url", middleware.ValidateUUIDParam("id"), controllers.GetSignedUploadURL)
		secureAdmin.POST("/venues/:id/images/batch-sign", middleware.ValidateUUIDParam("id"), controllers.BatchGetSignedURLs)
	}
}

// setupMobileRoutes wires the legacy (Pull-API-Go) URL paths that the
// PullMobileApp-GL staff app still calls. Public read-only ones are mounted
// unauthed; mutating ones get AuthenticateStaff().
func setupMobileRoutes(v1 *gin.RouterGroup) {
	// Public — used during onboarding before auth completes
	v1.GET("/event/upcoming-events/:venueId", middleware.RateLimitGeneral(), controllers.MobileGetUpcomingEvents)
	v1.GET("/event/get-event-details/:eventId", middleware.RateLimitGeneral(), controllers.MobileGetEventDetails)
	v1.GET("/venue/get-venue-info/:venueId", middleware.RateLimitGeneral(), controllers.MobileGetVenueInfo)

	// Authenticated — staff-only operations
	authed := v1.Group("")
	authed.Use(middleware.AuthenticateStaff())
	{
		// Events CRUD (mobile staff)
		authed.POST("/event/create-event", controllers.MobileCreateEvent)
		authed.POST("/event/create-event-with-tickets", controllers.MobileCreateEventWithTickets)
		authed.PUT("/event/update-event/:eventId", middleware.ValidateUUIDParam("eventId"), controllers.MobileUpdateEvent)
		authed.DELETE("/event/delete-event/:eventId", middleware.ValidateUUIDParam("eventId"), controllers.MobileDeleteEvent)

		authed.POST("/ticket-validation/validate-ticket", controllers.MobileValidateTicket)
		authed.GET("/orders/venue/:venueId", controllers.MobileGetVenueOrders)
		// /orders/details/:orderId is already registered in setupLegacyRoutes
		// (LegacyGetOrderDetails); we don't double-register here.
		authed.POST("/orders/:orderId/approve", middleware.ValidateUUIDParam("orderId"), controllers.MobileApproveOrder)
		authed.POST("/orders/:orderId/reject", middleware.ValidateUUIDParam("orderId"), controllers.MobileRejectOrder)
		authed.GET("/group-reservations/details/:reservationId", middleware.ValidateUUIDParam("reservationId"), controllers.MobileGetGroupReservationDetails)
		authed.POST("/group-reservations/:id/approve", middleware.ValidateUUIDParam("id"), controllers.MobileApproveGroupReservation)
		authed.POST("/group-reservations/:id/reject", middleware.ValidateUUIDParam("id"), controllers.MobileRejectGroupReservation)

		// Guest list staff endpoints are ALL registered in setupStaffGuestListRoutes
		// already (line ~441). The Mobile* compat versions in this file are unused
		// to avoid double-registration panics; the v2 handlers handle the same paths.

		// Bookings (VIP table reservations seen by staff)
		authed.GET("/bookings/get-bookings/:venueId", controllers.MobileGetBookings)
		authed.GET("/bookings/get-booking-details/:bookingId", middleware.ValidateUUIDParam("bookingId"), controllers.MobileGetBookingDetails)

		// Employees (admin role consumes these)
		authed.GET("/employees/employees", controllers.MobileGetEmployees)
		authed.GET("/employees/employees/:employeeId", middleware.ValidateUUIDParam("employeeId"), controllers.MobileGetEmployee)
		// Create uses a mobile-friendly shape (no password/role_id required).
		// Update/Delete reuse the v2 routes PUT/DELETE /employees/:id, which
		// the app already calls — adding /employees/:employeeId here would
		// panic gin (wildcard name conflict with :id).
		authed.POST("/employees/create", middleware.RateLimitCreate(), controllers.MobileCreateEmployee)

		// Upload (event image)
		authed.POST("/upload/event-image", controllers.MobileUploadEventImage)

		// Orders extras
		authed.GET("/orders/search/:venueId", controllers.MobileOrdersSearch)
		authed.POST("/orders/refresh-view", controllers.MobileOrdersRefreshView)

		// Bottle redemption stubs (demo has no vouchers)
		authed.POST("/bottle-redemption/preview", controllers.MobileBottleRedemptionPreview)
		authed.POST("/bottle-redemption/validate", controllers.MobileBottleRedemptionValidate)

		// Ticket types CRUD (mobile staff EventoDetalle)
		authed.GET("/ticket-types/event/:eventId", middleware.ValidateUUIDParam("eventId"), controllers.MobileGetTicketTypesByEvent)
		authed.POST("/ticket-types/event/:eventId", middleware.ValidateUUIDParam("eventId"), controllers.MobileCreateTicketType)
		authed.PUT("/ticket-types/:ticketTypeId", middleware.ValidateUUIDParam("ticketTypeId"), controllers.MobileUpdateTicketType)
		authed.DELETE("/ticket-types/:ticketTypeId", middleware.ValidateUUIDParam("ticketTypeId"), controllers.MobileDeleteTicketType)

		// Push token registration — MUST be authenticated (the app registers
		// right after login, so it has a token). Public registration let an
		// attacker subscribe their device to another venue's staff pushes.
		authed.POST("/notifications/register-token", controllers.MobileRegisterPushToken)
		authed.POST("/notifications/unregister-token", controllers.MobileUnregisterPushToken)
	}

	// Auth refresh alias the mobile interceptor uses on 403 INVALID_TOKEN.
	v1.POST("/auth/refresh-token", middleware.AuthenticateStaff(), controllers.RefreshToken)
}

// setupLegacyRoutes wires the legacy (Pull-API-Go) URL paths that the
// PullWebApp-GL frontend still calls. Each route delegates to a wrapper that
// resolves the venue and calls into v2 handlers / venue DB directly.
func setupLegacyRoutes(v1 *gin.RouterGroup) {
	// Venues
	v1.GET("/venues/get-all-venues", middleware.RateLimitGeneral(), controllers.LegacyGetAllVenues)
	v1.GET("/venues/events/get-venue-info/:slug", middleware.RateLimitGeneral(), controllers.LegacyGetVenueInfo)
	v1.GET("/venues/events/get-all-events/:slug", middleware.RateLimitGeneral(), controllers.LegacyGetAllVenueEvents)
	v1.GET("/venues/get-venue-description/:venueName", middleware.RateLimitGeneral(), controllers.LegacyGetVenueDescription)
	v1.GET("/venues/get-event-venue-info/:venueId", middleware.RateLimitGeneral(), controllers.LegacyGetEventVenueInfo)
	v1.GET("/venues/get-reservation-types/:venueId", middleware.RateLimitGeneral(), controllers.LegacyGetReservationTypes)

	// Events
	v1.GET("/event/get-all-events", middleware.RateLimitGeneral(), controllers.LegacyGetAllVenueEvents)
	v1.GET("/event/get-event-info/:slug", middleware.RateLimitGeneral(), controllers.LegacyGetEventInfo)
	v1.GET("/event/get-detailed-event-info/:slug", middleware.RateLimitGeneral(), controllers.LegacyGetDetailedEventInfo)
	v1.GET("/ticket-type/get-ticket-types/:eventSlug", middleware.RateLimitGeneral(), controllers.LegacyGetTicketTypes)
	v1.GET("/ticket-type/get-ticket-info/:eventSlug/:ticketTypeId", middleware.RateLimitGeneral(), controllers.LegacyGetTicketInfo)
	v1.GET("/guest-lists/event/:eventSlug", middleware.RateLimitGeneral(), controllers.LegacyGetGuestListsByEvent)
	v1.POST("/guest-lists/signup", middleware.RateLimitCreate(), controllers.LegacyGuestListSignup)
	v1.GET("/guest-lists/status/:code", middleware.RateLimitGeneral(), controllers.LegacyGetGuestListStatus)

	// Group reservations (mesa VIP) — legacy paths
	v1.GET("/group-reservations/bottles/:venueSlug", middleware.RateLimitGeneral(), controllers.LegacyGetBottles)
	v1.GET("/group-reservations/mixers/:venueSlug", middleware.RateLimitGeneral(), controllers.LegacyGetMixers)
	v1.POST("/group-reservations/create", middleware.RateLimitCreate(), controllers.LegacyCreateGroupReservation)
	v1.GET("/group-reservations/track/:code", middleware.RateLimitGeneral(), controllers.LegacyTrackGroupReservation)
	v1.GET("/group-reservations/manage/:code", middleware.RateLimitGeneral(), controllers.LegacyTrackGroupReservation)
	// Per-guest flow from the shared tracking link: each member completes
	// their data and pays their share (WebApp group-guest-complete page).
	v1.GET("/group-reservations/guest/:guestId", middleware.RateLimitGeneral(), middleware.ValidateUUIDParam("guestId"), controllers.LegacyGetGroupGuest)
	v1.POST("/group-reservations/guest/:guestId/complete", middleware.RateLimitCreate(), middleware.ValidateUUIDParam("guestId"), controllers.LegacyCompleteGroupGuest)
	v1.POST("/group-reservations/guest/:guestId/pay", middleware.RateLimitCreate(), middleware.ValidateUUIDParam("guestId"), controllers.LegacyPayGroupGuest)
	v1.POST("/group-reservations/guest/:guestId/verify-access-code", middleware.RateLimitGeneral(), middleware.ValidateUUIDParam("guestId"), controllers.LegacyVerifyGroupGuestAccessCode)

	// Orders (legacy paths)
	v1.POST("/orders/create-pending-order", middleware.RateLimitCreate(), controllers.LegacyCreatePendingOrder)
	v1.POST("/orders/create-checkout-session", middleware.RateLimitPayment(), controllers.LegacyCreateCheckoutSession)
	v1.GET("/orders/confirm-payment", controllers.LegacyConfirmPayment)
	v1.POST("/orders/simulate-payment", middleware.RateLimitPayment(), controllers.LegacySimulatePayment)
	// Direct-card payment (NeoNet/Cybersource): two atomic sales per purchase.
	v1.POST("/orders/pay", middleware.RateLimitPayment(), controllers.PayOrder)
	v1.GET("/orders/details/:orderId", controllers.LegacyGetOrderDetails)
}

func setupWebhookRoutes(router *gin.Engine) {
	webhooks := router.Group("/webhooks")
	webhooks.Use(middleware.WebhookRateLimit())
	{
		webhooks.POST("/stripe/:venue_id", middleware.ValidateUUIDParam("venue_id"), controllers.HandleStripeWebhook)
		webhooks.POST("/neonet/:venue_id", middleware.ValidateUUIDParam("venue_id"), controllers.HandleNeoNetWebhook)
	}
}
