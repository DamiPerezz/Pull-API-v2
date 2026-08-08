package models

import "time"

// =============================================
// ANALYTICS & REVENUE MODELS
// =============================================

// DashboardStats represents venue dashboard overview
type DashboardStats struct {
	// Today's stats
	TodayRevenue      float64 `json:"today_revenue"`
	TodayOrders       int     `json:"today_orders"`
	TodayTicketsSold  int     `json:"today_tickets_sold"`
	TodayCheckIns     int     `json:"today_check_ins"`

	// Week stats
	WeekRevenue       float64 `json:"week_revenue"`
	WeekOrders        int     `json:"week_orders"`
	WeekTicketsSold   int     `json:"week_tickets_sold"`

	// Month stats
	MonthRevenue      float64 `json:"month_revenue"`
	MonthOrders       int     `json:"month_orders"`
	MonthTicketsSold  int     `json:"month_tickets_sold"`

	// Comparison (vs previous period)
	RevenueTrend      float64 `json:"revenue_trend"`      // Percentage change
	OrdersTrend       float64 `json:"orders_trend"`
	TicketsTrend      float64 `json:"tickets_trend"`

	// Active events
	ActiveEvents      int `json:"active_events"`
	UpcomingEvents    int `json:"upcoming_events"`

	// Pending actions
	PendingOrders        int `json:"pending_orders"`
	PendingReservations  int `json:"pending_reservations"`
	PendingGuestLists    int `json:"pending_guest_lists"`
	UnreadNotifications  int `json:"unread_notifications"`

	// Real-time (current event if any)
	CurrentEventID       *string `json:"current_event_id,omitempty"`
	CurrentEventName     *string `json:"current_event_name,omitempty"`
	CurrentEventCheckIns int     `json:"current_event_check_ins"`
	CurrentEventCapacity int     `json:"current_event_capacity"`
}

// EventAnalytics represents detailed analytics for an event
type EventAnalytics struct {
	EventID   string `json:"event_id"`
	EventName string `json:"event_name"`
	EventDate string `json:"event_date"`

	// Revenue breakdown
	TotalRevenue       float64 `json:"total_revenue"`
	TicketRevenue      float64 `json:"ticket_revenue"`
	ReservationRevenue float64 `json:"reservation_revenue"`
	VIPListRevenue     float64 `json:"vip_list_revenue"`
	BottleRevenue      float64 `json:"bottle_revenue"`

	// Fees
	PlatformFees  float64 `json:"platform_fees"`
	GatewayFees   float64 `json:"gateway_fees"`
	NetRevenue    float64 `json:"net_revenue"`

	// Tickets
	TotalTicketCapacity int     `json:"total_ticket_capacity"`
	TicketsSold         int     `json:"tickets_sold"`
	TicketsRemaining    int     `json:"tickets_remaining"`
	TicketSellThrough   float64 `json:"ticket_sell_through"` // Percentage

	// By ticket type
	TicketTypeStats []TicketTypeStats `json:"ticket_type_stats"`

	// Reservations
	TotalReservations      int `json:"total_reservations"`
	ConfirmedReservations  int `json:"confirmed_reservations"`
	PendingReservations    int `json:"pending_reservations"`
	CancelledReservations  int `json:"cancelled_reservations"`
	TotalReservationGuests int `json:"total_reservation_guests"`

	// VIP Lists
	TotalVIPLists     int `json:"total_vip_lists"`
	TotalVIPGuests    int `json:"total_vip_guests"`
	VIPGuestsPaid     int `json:"vip_guests_paid"`

	// Guest Lists
	TotalGuestListTypes   int `json:"total_guest_list_types"`
	TotalGuestListSignups int `json:"total_guest_list_signups"`
	ApprovedSignups       int `json:"approved_signups"`
	PendingSignups        int `json:"pending_signups"`

	// Check-ins
	TotalCapacity       int     `json:"total_capacity"`
	TotalCheckedIn      int     `json:"total_checked_in"`
	CheckInRate         float64 `json:"check_in_rate"` // Percentage
	TicketsCheckedIn    int     `json:"tickets_checked_in"`
	ReservationsCheckedIn int   `json:"reservations_checked_in"`
	VIPCheckedIn        int     `json:"vip_checked_in"`
	GuestListCheckedIn  int     `json:"guest_list_checked_in"`

	// Demographics
	GenderBreakdown    map[string]int `json:"gender_breakdown,omitempty"`
	AgeBreakdown       map[string]int `json:"age_breakdown,omitempty"`

	// Timeline
	SalesByDay    []DailySales    `json:"sales_by_day,omitempty"`
	SalesByHour   []HourlySales   `json:"sales_by_hour,omitempty"`
	CheckInsByHour []HourlyCheckIns `json:"check_ins_by_hour,omitempty"`
}

// TicketTypeStats for per-type analytics
type TicketTypeStats struct {
	TicketTypeID    string  `json:"ticket_type_id"`
	Name            string  `json:"name"`
	Price           float64 `json:"price"`
	InitialQuantity int     `json:"initial_quantity"`
	Sold            int     `json:"sold"`
	Remaining       int     `json:"remaining"`
	Revenue         float64 `json:"revenue"`
	SellThrough     float64 `json:"sell_through"`
	CheckedIn       int     `json:"checked_in"`
}

// DailySales for timeline charts
type DailySales struct {
	Date     string  `json:"date"`
	Orders   int     `json:"orders"`
	Tickets  int     `json:"tickets"`
	Revenue  float64 `json:"revenue"`
}

// HourlySales for peak analysis
type HourlySales struct {
	Hour    int     `json:"hour"`
	Orders  int     `json:"orders"`
	Tickets int     `json:"tickets"`
	Revenue float64 `json:"revenue"`
}

// HourlyCheckIns for event night analysis
type HourlyCheckIns struct {
	Hour     int `json:"hour"`
	CheckIns int `json:"check_ins"`
}

// =============================================
// VENUE ANALYTICS
// =============================================

// VenueAnalytics represents overall venue performance
type VenueAnalytics struct {
	VenueID        string `json:"venue_id"`
	VenueName      string `json:"venue_name"`
	Period         string `json:"period"` // day, week, month, year, custom
	StartDate      string `json:"start_date"`
	EndDate        string `json:"end_date"`

	// Revenue
	TotalRevenue      float64 `json:"total_revenue"`
	TicketRevenue     float64 `json:"ticket_revenue"`
	ReservationRevenue float64 `json:"reservation_revenue"`
	VIPListRevenue    float64 `json:"vip_list_revenue"`
	BottleRevenue     float64 `json:"bottle_revenue"`

	// Fees
	TotalPlatformFees float64 `json:"total_platform_fees"`
	TotalGatewayFees  float64 `json:"total_gateway_fees"`
	NetRevenue        float64 `json:"net_revenue"`

	// Counts
	TotalEvents       int `json:"total_events"`
	TotalOrders       int `json:"total_orders"`
	TotalTicketsSold  int `json:"total_tickets_sold"`
	TotalReservations int `json:"total_reservations"`
	TotalVIPLists     int `json:"total_vip_lists"`
	TotalGuestListSignups int `json:"total_guest_list_signups"`
	TotalCheckIns     int `json:"total_check_ins"`

	// Averages
	AvgOrderValue     float64 `json:"avg_order_value"`
	AvgTicketsPerOrder float64 `json:"avg_tickets_per_order"`
	AvgRevenuePerEvent float64 `json:"avg_revenue_per_event"`
	AvgCheckInRate    float64 `json:"avg_check_in_rate"`

	// Top performers
	TopEvents       []EventRevenueSummary `json:"top_events,omitempty"`
	TopTicketTypes  []TicketTypeRevenue   `json:"top_ticket_types,omitempty"`

	// Trends
	RevenueTrend    []DailySales `json:"revenue_trend,omitempty"`
	OrdersTrend     []DailyOrders `json:"orders_trend,omitempty"`
}

// EventRevenueSummary for top events
type EventRevenueSummary struct {
	EventID     string    `json:"event_id"`
	EventName   string    `json:"event_name"`
	EventDate   string    `json:"event_date"`
	TotalRevenue float64  `json:"total_revenue"`
	TicketsSold int       `json:"tickets_sold"`
	CheckIns    int       `json:"check_ins"`
}

// TicketTypeRevenue for top performing ticket types
type TicketTypeRevenue struct {
	TicketTypeID string  `json:"ticket_type_id"`
	Name         string  `json:"name"`
	EventName    string  `json:"event_name"`
	TotalRevenue float64 `json:"total_revenue"`
	TotalSold    int     `json:"total_sold"`
}

// DailyOrders for order trends
type DailyOrders struct {
	Date   string `json:"date"`
	Orders int    `json:"orders"`
}

// =============================================
// PLATFORM ANALYTICS (Central DB)
// =============================================

// PlatformDashboard for Pull platform admins
type PlatformDashboard struct {
	// Revenue
	TotalPlatformRevenue float64 `json:"total_platform_revenue"`
	TodayPlatformRevenue float64 `json:"today_platform_revenue"`
	WeekPlatformRevenue  float64 `json:"week_platform_revenue"`
	MonthPlatformRevenue float64 `json:"month_platform_revenue"`

	// Volume
	TotalTransactions    int     `json:"total_transactions"`
	TodayTransactions    int     `json:"today_transactions"`
	TotalGrossVolume     float64 `json:"total_gross_volume"`
	TodayGrossVolume     float64 `json:"today_gross_volume"`

	// Venues
	TotalVenues      int `json:"total_venues"`
	ActiveVenues     int `json:"active_venues"`
	VenuesWithEvents int `json:"venues_with_events"`

	// Users
	TotalUsers       int `json:"total_users"`
	ActiveUsers      int `json:"active_users"` // Last 30 days
	NewUsersToday    int `json:"new_users_today"`
	NewUsersWeek     int `json:"new_users_week"`

	// Events
	TotalEvents      int `json:"total_events"`
	ActiveEvents     int `json:"active_events"`
	EventsToday      int `json:"events_today"`
	EventsThisWeek   int `json:"events_this_week"`

	// Top venues
	TopVenues []VenueRevenueSummary `json:"top_venues,omitempty"`

	// Trends
	RevenueTrend      []DailyPlatformRevenue `json:"revenue_trend,omitempty"`
	TransactionsTrend []DailyTransactions    `json:"transactions_trend,omitempty"`
}

// VenueRevenueSummary for platform top venues
type VenueRevenueSummary struct {
	VenueID          string  `json:"venue_id"`
	VenueName        string  `json:"venue_name"`
	TotalGrossVolume float64 `json:"total_gross_volume"`
	PlatformFees     float64 `json:"platform_fees"`
	TransactionCount int     `json:"transaction_count"`
	EventCount       int     `json:"event_count"`
}

// DailyPlatformRevenue for trends
type DailyPlatformRevenue struct {
	Date            string  `json:"date"`
	GrossVolume     float64 `json:"gross_volume"`
	PlatformFees    float64 `json:"platform_fees"`
	Transactions    int     `json:"transactions"`
}

// DailyTransactions for trends
type DailyTransactions struct {
	Date         string `json:"date"`
	Transactions int    `json:"transactions"`
}

// =============================================
// REVENUE REPORTS
// =============================================

// RevenueReport detailed revenue breakdown
type RevenueReport struct {
	VenueID        string    `json:"venue_id"`
	Period         string    `json:"period"`
	StartDate      time.Time `json:"start_date"`
	EndDate        time.Time `json:"end_date"`
	GeneratedAt    time.Time `json:"generated_at"`

	// Summary
	GrossRevenue      float64 `json:"gross_revenue"`
	PlatformFees      float64 `json:"platform_fees"`
	GatewayFees       float64 `json:"gateway_fees"`
	NetRevenue        float64 `json:"net_revenue"`
	RefundedAmount    float64 `json:"refunded_amount"`

	// By source
	TicketRevenue      float64 `json:"ticket_revenue"`
	ReservationRevenue float64 `json:"reservation_revenue"`
	VIPListRevenue     float64 `json:"vip_list_revenue"`
	BottleRevenue      float64 `json:"bottle_revenue"`

	// By payment method
	// dLocal Go es la pasarela ACTIVA desde ago-2026; stripe/neonet solo
	// aparecen en transacciones históricas.
	DLocalRevenue       float64 `json:"dlocal_revenue"`
	StripeRevenue       float64 `json:"stripe_revenue"`
	NeoNetRevenue       float64 `json:"neonet_revenue"`
	MercadoPagoRevenue  float64 `json:"mercadopago_revenue"`
	CashRevenue         float64 `json:"cash_revenue"`
	TransferRevenue     float64 `json:"transfer_revenue"`

	// Detailed transactions
	Transactions []TransactionSummary `json:"transactions,omitempty"`
}

// TransactionSummary for reports
type TransactionSummary struct {
	ID              string    `json:"id"`
	Date            time.Time `json:"date"`
	Type            string    `json:"type"`
	EventName       string    `json:"event_name,omitempty"`
	CustomerName    string    `json:"customer_name"`
	CustomerEmail   string    `json:"customer_email"`
	GrossAmount     float64   `json:"gross_amount"`
	PlatformFee     float64   `json:"platform_fee"`
	GatewayFee      float64   `json:"gateway_fee"`
	NetAmount       float64   `json:"net_amount"`
	PaymentGateway  string    `json:"payment_gateway"`
	Status          string    `json:"status"`
}

// =============================================
// ANALYTICS REQUEST TYPES
// =============================================

// AnalyticsRequest for querying analytics
type AnalyticsRequest struct {
	StartDate  string `form:"start_date"`
	EndDate    string `form:"end_date"`
	Period     string `form:"period"` // day, week, month, year
	EventID    string `form:"event_id"`
	GroupBy    string `form:"group_by"` // day, week, month
	IncludeDetails bool `form:"include_details"`
}

// ExportFormat for report exports
type ExportFormat string

const (
	ExportCSV  ExportFormat = "csv"
	ExportXLSX ExportFormat = "xlsx"
	ExportPDF  ExportFormat = "pdf"
)

// ExportRequest for exporting reports
type ExportRequest struct {
	Format    ExportFormat `json:"format" binding:"required,oneof=csv xlsx pdf"`
	StartDate string       `json:"start_date" binding:"required"`
	EndDate   string       `json:"end_date" binding:"required"`
	Type      string       `json:"type" binding:"required"` // revenue, transactions, events
}
