package config

import (
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all application configuration
type Config struct {
	// Server
	Port        string
	Environment string

	// =============================================
	// CENTRAL PULL DATABASE (Platform data)
	// =============================================
	CentralSupabaseURL    string
	CentralServiceKey     string
	CentralAnonKey        string

	// =============================================
	// DEFAULT VENUE DATABASE (Legacy/Fallback)
	// =============================================
	DefaultSupabaseURL    string
	DefaultServiceKey     string
	DefaultAnonKey        string

	// Security
	JWTSecret             string
	AppKey                string // 64 hex chars for AES-256 encryption

	// CORS & Cookies
	CookieDomain          string
	CookieSecure          bool
	CookieSameSite        string
	AllowedOrigins        []string

	// Stripe (Platform)
	StripeSecretKey       string
	StripePublishableKey  string
	StripeWebhookSecret   string

	// Email (Resend)
	ResendAPIKey          string
	ResendFromEmail       string

	// Email (Brevo, alternative provider — 300/day free without domain verification)
	BrevoAPIKey           string
	BrevoFromEmail        string

	// Frontend
	FrontendURL           string

	// Public base URL of this API (used to build absolute URLs, e.g. the demo
	// checkout page). When empty, falls back to http://localhost:{port}.
	APIBaseURL            string

	// Demo mode: when true, all payment processors are replaced by a mock
	// gateway that simulates checkout end-to-end without charging anything.
	DemoMode              bool

	// Redis (optional)
	RedisURL              string

	// Google OAuth (optional)
	GoogleClientID        string
	GoogleClientSecret    string
}

var App *Config

// Load initializes configuration from environment
func Load() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	env := getEnv("ENVIRONMENT", "development")
	frontendURL := getEnv("FRONTEND_URL", "https://web.pullevents.com")
	isProduction := env == "production"

	// Parse cookie domain from frontend URL
	// In development, don't use a domain (allows localhost cookies)
	var cookieDomain string
	if isProduction {
		cookieDomain = parseCookieDomain(frontendURL)
	} else {
		cookieDomain = "" // Empty domain works for localhost
	}

	// Parse allowed origins
	allowedOrigins := parseAllowedOrigins(getEnv("ALLOWED_ORIGINS", frontendURL))

	App = &Config{
		Port:        getEnv("PORT", "8080"),
		Environment: env,

		// Central Pull Platform Database
		CentralSupabaseURL: getEnv("CENTRAL_SUPABASE_URL", ""),
		CentralServiceKey:  getEnv("CENTRAL_SERVICE_KEY", ""),
		CentralAnonKey:     getEnv("CENTRAL_ANON_KEY", ""),

		// Default Venue Database (fallback)
		DefaultSupabaseURL: getEnv("DEFAULT_SUPABASE_URL", getEnv("SUPABASE_URL", "")),
		DefaultServiceKey:  getEnv("DEFAULT_SERVICE_KEY", getEnv("SUPABASE_SERVICE_KEY", "")),
		DefaultAnonKey:     getEnv("DEFAULT_ANON_KEY", getEnv("SUPABASE_ANON_KEY", "")),

		// Security
		JWTSecret: getEnv("JWT_SECRET", ""),
		AppKey:    getEnv("APP_KEY", ""),

		// CORS & Cookies
		CookieDomain:   cookieDomain,
		CookieSecure:   isProduction,
		CookieSameSite: getEnv("COOKIE_SAMESITE", "Lax"),
		AllowedOrigins: allowedOrigins,

		// Stripe
		StripeSecretKey:      getEnv("STRIPE_SECRET_KEY", ""),
		StripePublishableKey: getEnv("STRIPE_PUBLISHABLE_KEY", ""),
		StripeWebhookSecret:  getEnv("STRIPE_WEBHOOK_SECRET", ""),

		// Email
		ResendAPIKey:    getEnv("RESEND_API_KEY", ""),
		ResendFromEmail: getEnv("RESEND_FROM_EMAIL", "Pull Events <noreply@tickets.pullevents.com>"),

		// Brevo (alternative email provider)
		BrevoAPIKey:    getEnv("BREVO_API_KEY", ""),
		BrevoFromEmail: getEnv("BREVO_FROM_EMAIL", "Pull Events <noreply@pullevents.com>"),

		// Frontend
		FrontendURL: frontendURL,

		// Public API base URL + demo mode
		APIBaseURL: getEnv("API_BASE_URL", ""),
		DemoMode:   strings.EqualFold(getEnv("DEMO_MODE", "false"), "true"),

		// Redis
		RedisURL: getEnv("REDIS_URL", ""),

		// Google OAuth
		GoogleClientID:     getEnv("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: getEnv("GOOGLE_CLIENT_SECRET", ""),
	}

	validate()
	logConfig()
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func parseCookieDomain(frontendURL string) string {
	parsed, err := url.Parse(frontendURL)
	if err != nil {
		return ""
	}
	host := parsed.Hostname()
	if strings.Contains(host, "localhost") || strings.Contains(host, "127.0.0.1") {
		return ""
	}
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		return "." + parts[len(parts)-2] + "." + parts[len(parts)-1]
	}
	return ""
}

func parseAllowedOrigins(originsStr string) []string {
	origins := strings.Split(originsStr, ",")
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}
	return origins
}

func validate() {
	// Required in all environments
	required := map[string]string{
		"JWT_SECRET": App.JWTSecret,
		"APP_KEY":    App.AppKey,
	}

	for name, value := range required {
		if value == "" {
			log.Printf("Warning: %s is not set", name)
		}
	}

	// Must have at least one database configured
	if App.CentralSupabaseURL == "" && App.DefaultSupabaseURL == "" {
		log.Fatal("ERROR: At least one Supabase URL must be configured")
	}

	// Production security checks
	if App.Environment == "production" {
		if len(App.JWTSecret) < 32 {
			log.Fatal("SECURITY ERROR: JWT_SECRET must be at least 32 characters in production")
		}
		if len(App.AppKey) != 64 {
			log.Fatal("SECURITY ERROR: APP_KEY must be exactly 64 hex characters")
		}
		if App.StripeWebhookSecret == "" {
			log.Println("Warning: STRIPE_WEBHOOK_SECRET not set in production")
		}
	}
}

func logConfig() {
	log.Println("=============================================")
	log.Printf("Pull API v2 - Multi-Tenant Architecture")
	log.Println("=============================================")
	log.Printf("Environment: %s", App.Environment)
	log.Printf("Port: %s", App.Port)
	log.Printf("Frontend: %s", App.FrontendURL)

	// Database status
	if App.CentralSupabaseURL != "" {
		log.Printf("Central DB (Pull Platform): Configured")
	} else {
		log.Printf("Central DB: Not configured (single-tenant mode)")
	}

	if App.DefaultSupabaseURL != "" {
		log.Printf("Default Venue DB: Configured")
	}

	// Services status
	if App.StripeSecretKey != "" {
		log.Printf("Stripe: Configured")
	}
	if App.ResendAPIKey != "" {
		log.Printf("Resend Email: Configured")
	}
	if App.RedisURL != "" {
		log.Printf("Redis: Configured")
	}
	if App.GoogleClientID != "" {
		log.Printf("Google OAuth: Configured")
	}

	log.Println("=============================================")
}

// =============================================
// HELPER FUNCTIONS
// =============================================

func IsProduction() bool {
	return App != nil && App.Environment == "production"
}

func IsDevelopment() bool {
	return App != nil && App.Environment == "development"
}

func IsMultiTenantEnabled() bool {
	return App != nil && App.CentralSupabaseURL != ""
}

func IsOriginAllowed(origin string) bool {
	if App == nil {
		return false
	}
	// In development, allow localhost
	if !IsProduction() && (strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1")) {
		return true
	}
	for _, allowed := range App.AllowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}
