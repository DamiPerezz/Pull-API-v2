package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"pull-api-v2/config"
	"pull-api-v2/models"
	"strings"
	"sync"
	"time"
)

// =============================================
// PAYMENT GATEWAY ROUTER
// Routes payments to the correct gateway per venue
// =============================================

// PaymentRouter manages payment gateways for venues
type PaymentRouter struct {
	// Venue payment configs (cached)
	configs   map[string]*models.VenuePaymentConfig
	configMu  sync.RWMutex
	cacheTTL  time.Duration
	cacheTime map[string]time.Time

	// Crypto service for decrypting credentials
	crypto *CryptoService
}

// Global payment router instance
var Payments *PaymentRouter

// InitPaymentRouter initializes the payment router
func InitPaymentRouter() error {
	crypto, err := NewCryptoService(config.App.AppKey)
	if err != nil {
		log.Printf("Warning: Crypto service not initialized for payments (APP_KEY may be missing): %v", err)
		// Continue without crypto - will use default config
	}

	Payments = &PaymentRouter{
		configs:   make(map[string]*models.VenuePaymentConfig),
		cacheTTL:  5 * time.Minute,
		cacheTime: make(map[string]time.Time),
		crypto:    crypto,
	}

	log.Println("Payment Gateway Router: Initialized")
	return nil
}

// =============================================
// GATEWAY SELECTION
// =============================================

// GetGatewayForVenue returns the primary payment gateway for a venue
func (r *PaymentRouter) GetGatewayForVenue(ctx context.Context, venueID string) (*models.VenuePaymentConfig, error) {
	// Check cache first (fast path)
	r.configMu.RLock()
	if cfg, ok := r.configs[venueID]; ok {
		if time.Since(r.cacheTime[venueID]) < r.cacheTTL {
			r.configMu.RUnlock()
			return cfg, nil
		}
	}
	r.configMu.RUnlock()

	// Load from central database with context
	cfg, err := r.loadPaymentConfig(ctx, venueID)
	if err != nil {
		return nil, err
	}

	// Cache config
	r.configMu.Lock()
	r.configs[venueID] = cfg
	r.cacheTime[venueID] = time.Now()
	r.configMu.Unlock()

	return cfg, nil
}

// loadPaymentConfig loads payment config from central database
func (r *PaymentRouter) loadPaymentConfig(ctx context.Context, venueID string) (*models.VenuePaymentConfig, error) {
	result, err := DB.Central().QueryCtx(ctx, "payment_gateway_credentials", map[string]interface{}{
		"select": "*",
		"where": map[string]interface{}{
			"venue_id":   venueID,
			"is_active":  true,
			"is_primary": true,
		},
		"limit": 1,
	})

	if err != nil {
		return nil, err
	}

	if len(result) == 0 {
		// Return default Stripe config if no custom config
		return r.getDefaultConfig(venueID), nil
	}

	cfg := &models.VenuePaymentConfig{
		ID:                 GetString(result[0], "id"),
		VenueID:            GetString(result[0], "venue_id"),
		Gateway:            models.PaymentGateway(GetString(result[0], "gateway")),
		GatewayName:        GetString(result[0], "gateway_name"),
		IsActive:           GetBool(result[0], "is_active"),
		IsPrimary:          GetBool(result[0], "is_primary"),
		Priority:           GetInt(result[0], "priority"),
		Environment:        GetString(result[0], "environment"),
		PlatformFeePercent: GetFloat64(result[0], "platform_fee_percent"),
		PlatformFeeFixed:   GetFloat64(result[0], "platform_fee_fixed"),
		GatewayFeePercent:  GetFloat64(result[0], "gateway_fee_percent"),
		GatewayFeeFixed:    GetFloat64(result[0], "gateway_fee_fixed"),
		DefaultCurrency:    GetString(result[0], "default_currency"),
	}

	// Build credentials from specific fields
	cfg.Credentials = &models.GatewayCredentials{
		// Stripe
		StripeAccountID:      GetString(result[0], "stripe_account_id"),
		StripePublishableKey: GetString(result[0], "stripe_publishable_key"),
		// NeoNet/Cybersource
		NeoNetProfileID:  GetString(result[0], "profile_id"),
		NeoNetAccessKey:  GetString(result[0], "access_key"),
		NeoNetMerchantID: GetString(result[0], "merchant_id"),
		NeoNetTerminalID: GetString(result[0], "terminal_id"),
		// MercadoPago
		MPPublicKey: GetString(result[0], "mercadopago_public_key"),
		// dLocal Go: comparte columnas con NeoNet — la API Key va en
		// `access_key` y la Secret Key en `secret_key_encrypted` (se descifra
		// abajo). `split_code` y `smartfields_key` son columnas OPCIONALES (hoy
		// NO existen en la tabla): si faltan, GetString devuelve "" y el
		// procesador cae a la env DLOCAL_SPLIT_CODE. `smartfields_key` es
		// pública y solo hará falta en la fase de checkout transparente.
		DLocalAPIKey:         GetString(result[0], "access_key"),
		DLocalSplitCode:      GetString(result[0], "split_code"),
		DLocalSmartFieldsKey: GetString(result[0], "smartfields_key"),
	}

	// Decrypt sensitive keys based on gateway
	if secretKey := GetString(result[0], "secret_key_encrypted"); secretKey != "" {
		if r.crypto == nil {
			// Sin APP_KEY no hay forma de leer la credencial. Antes esto era un
			// panic por deref de nil en plena ruta del dinero.
			log.Printf("[PaymentRouter] ALERT venue=%s tiene secret_key_encrypted pero el servicio de cifrado no está inicializado (¿falta APP_KEY?) — la pasarela quedará sin credenciales", venueID)
		} else if decrypted, err := r.crypto.Decrypt(secretKey); err != nil {
			log.Printf("[PaymentRouter] ALERT venue=%s no se pudo descifrar secret_key_encrypted: %v", venueID, err)
		} else {
			switch cfg.Gateway {
			case models.GatewayStripe:
				cfg.Credentials.StripeSecretKey = decrypted
			case models.GatewayNeoNet:
				cfg.Credentials.NeoNetSecretKey = decrypted
			case models.GatewayDLocal:
				cfg.Credentials.DLocalSecretKey = decrypted
			}
		}
	}

	// Decrypt MercadoPago access token (stored separately)
	if mpToken := GetString(result[0], "mercadopago_access_token_encrypted"); mpToken != "" && r.crypto != nil {
		if decrypted, err := r.crypto.Decrypt(mpToken); err == nil {
			cfg.Credentials.MPAccessToken = decrypted
		}
	}

	// Decrypt Stripe webhook secret
	if stripeWebhook := GetString(result[0], "stripe_webhook_secret_encrypted"); stripeWebhook != "" && r.crypto != nil {
		if decrypted, err := r.crypto.Decrypt(stripeWebhook); err == nil {
			cfg.Credentials.StripeWebhookSecret = decrypted
		}
	}

	return cfg, nil
}

// getDefaultConfig returns default Stripe configuration
func (r *PaymentRouter) getDefaultConfig(venueID string) *models.VenuePaymentConfig {
	return &models.VenuePaymentConfig{
		VenueID:     venueID,
		Gateway:     models.GatewayStripe,
		IsActive:    true,
		IsPrimary:   true,
		Environment: "production",
		Credentials: &models.GatewayCredentials{
			StripeSecretKey:      config.App.StripeSecretKey,
			StripePublishableKey: config.App.StripePublishableKey,
			StripeWebhookSecret:  config.App.StripeWebhookSecret,
		},
	}
}

// decryptCredentials decrypts gateway credentials
func (r *PaymentRouter) decryptCredentials(encrypted string) (*models.GatewayCredentials, error) {
	if r.crypto == nil {
		return nil, fmt.Errorf("crypto service not initialized (APP_KEY missing)")
	}
	decrypted, err := r.crypto.Decrypt(encrypted)
	if err != nil {
		return nil, err
	}

	var creds models.GatewayCredentials
	if err := json.Unmarshal([]byte(decrypted), &creds); err != nil {
		return nil, err
	}

	return &creds, nil
}

// =============================================
// PAYMENT PROCESSOR INTERFACE
// =============================================

// PaymentProcessor defines payment gateway operations
type PaymentProcessor interface {
	CreateCheckout(ctx context.Context, params models.CheckoutParams) (*models.CheckoutResult, error)
	ConfirmPayment(ctx context.Context, sessionID string) (*models.PaymentResult, error)
	ProcessRefund(ctx context.Context, transactionID string, amount float64) error
	ValidateWebhook(payload []byte, signature string) (bool, error)
	GetGateway() models.PaymentGateway
}

// GetProcessor returns the appropriate payment processor for a venue
func (r *PaymentRouter) GetProcessor(ctx context.Context, venueID string) (PaymentProcessor, error) {
	// Demo mode short-circuits all gateways with a mock that always succeeds.
	if config.App != nil && config.App.DemoMode {
		return NewMockProcessor(), nil
	}

	cfg, err := r.GetGatewayForVenue(ctx, venueID)
	if err != nil {
		return nil, err
	}

	return processorForGateway(cfg, venueID)
}

// processorForGateway mapea la pasarela configurada a su procesador. Vive
// separado de GetProcessor para poder probar el mapeo (sobre todo que lo
// desconocido FALLA) sin base de datos.
func processorForGateway(cfg *models.VenuePaymentConfig, venueID string) (PaymentProcessor, error) {
	switch cfg.Gateway {
	case models.GatewayStripe:
		return NewStripeProcessor(cfg), nil
	case models.GatewayNeoNet:
		return NewNeoNetProcessor(cfg), nil
	case models.GatewayMercadoPago:
		return NewMercadoPagoProcessor(cfg), nil
	case models.GatewayDLocal:
		return NewDLocalProcessor(cfg), nil
	default:
		// FAIL-CLOSED. Antes esto caía a Stripe (que no está implementado): una
		// pasarela mal escrita en la BD (typo, valor nuevo sin desplegar) salía
		// por un carril que NO es el que el venue tiene contratado. Es la ruta
		// del dinero: si no sabemos por dónde cobrar, no se cobra.
		return nil, fmt.Errorf("gateway de pago no soportado: %q (venue %s) — valores válidos: stripe, neonet, mercadopago, dlocal", cfg.Gateway, venueID)
	}
}

// =============================================
// STRIPE PROCESSOR
// =============================================

type StripeProcessor struct {
	config *models.VenuePaymentConfig
}

func NewStripeProcessor(cfg *models.VenuePaymentConfig) *StripeProcessor {
	return &StripeProcessor{config: cfg}
}

func (p *StripeProcessor) GetGateway() models.PaymentGateway {
	return models.GatewayStripe
}

func (p *StripeProcessor) CreateCheckout(ctx context.Context, params models.CheckoutParams) (*models.CheckoutResult, error) {
	// TODO: Implement Stripe checkout using stripe-go SDK
	// Use p.config.Credentials for account-specific keys
	return nil, fmt.Errorf("stripe checkout: implement with stripe-go SDK")
}

func (p *StripeProcessor) ConfirmPayment(ctx context.Context, sessionID string) (*models.PaymentResult, error) {
	// TODO: Implement payment confirmation
	return nil, fmt.Errorf("stripe confirm: implement with stripe-go SDK")
}

func (p *StripeProcessor) ProcessRefund(ctx context.Context, transactionID string, amount float64) error {
	// TODO: Implement refund
	return fmt.Errorf("stripe refund: not implemented")
}

func (p *StripeProcessor) ValidateWebhook(payload []byte, signature string) (bool, error) {
	// TODO: Implement webhook validation
	return false, fmt.Errorf("stripe webhook: implement with stripe-go SDK")
}

// =============================================
// NEONET PROCESSOR
// =============================================

type NeoNetProcessor struct {
	config *models.VenuePaymentConfig
}

func NewNeoNetProcessor(cfg *models.VenuePaymentConfig) *NeoNetProcessor {
	return &NeoNetProcessor{config: cfg}
}

func (p *NeoNetProcessor) GetGateway() models.PaymentGateway {
	return models.GatewayNeoNet
}

// CreateCheckout is intentionally unsupported: the NeoNet flow is
// direct-card via controllers.PayOrder (see neonet_processor_impl.go).
func (p *NeoNetProcessor) CreateCheckout(ctx context.Context, params models.CheckoutParams) (*models.CheckoutResult, error) {
	return nil, fmt.Errorf("neonet uses the direct card flow (/orders/pay), not hosted checkout")
}

func (p *NeoNetProcessor) ConfirmPayment(ctx context.Context, transactionID string) (*models.PaymentResult, error) {
	return p.neonetConfirmPayment(transactionID)
}

func (p *NeoNetProcessor) ProcessRefund(ctx context.Context, transactionID string, amount float64) error {
	return p.neonetProcessRefund(ctx, transactionID, amount)
}

func (p *NeoNetProcessor) ValidateWebhook(payload []byte, signature string) (bool, error) {
	return p.neonetValidateWebhook(payload, signature)
}

// =============================================
// MERCADOPAGO PROCESSOR
// =============================================

type MercadoPagoProcessor struct {
	config *models.VenuePaymentConfig
}

func NewMercadoPagoProcessor(cfg *models.VenuePaymentConfig) *MercadoPagoProcessor {
	return &MercadoPagoProcessor{config: cfg}
}

func (p *MercadoPagoProcessor) GetGateway() models.PaymentGateway {
	return models.GatewayMercadoPago
}

func (p *MercadoPagoProcessor) CreateCheckout(ctx context.Context, params models.CheckoutParams) (*models.CheckoutResult, error) {
	if p.config.Credentials == nil {
		return nil, fmt.Errorf("mercadopago credentials not configured")
	}
	// TODO: Implement MercadoPago checkout
	return nil, fmt.Errorf("mercadopago checkout: pending implementation")
}

func (p *MercadoPagoProcessor) ConfirmPayment(ctx context.Context, transactionID string) (*models.PaymentResult, error) {
	return nil, fmt.Errorf("mercadopago confirm: pending implementation")
}

func (p *MercadoPagoProcessor) ProcessRefund(ctx context.Context, transactionID string, amount float64) error {
	return fmt.Errorf("mercadopago refund: pending implementation")
}

func (p *MercadoPagoProcessor) ValidateWebhook(payload []byte, signature string) (bool, error) {
	return false, fmt.Errorf("mercadopago webhook: pending implementation")
}

// =============================================
// CACHE MANAGEMENT
// =============================================

// InvalidateCache removes venue from payment config cache
func (r *PaymentRouter) InvalidateCache(venueID string) {
	r.configMu.Lock()
	delete(r.configs, venueID)
	delete(r.cacheTime, venueID)
	r.configMu.Unlock()
}

// Stats returns payment router statistics
func (r *PaymentRouter) Stats() map[string]interface{} {
	r.configMu.RLock()
	configCount := len(r.configs)
	r.configMu.RUnlock()

	return map[string]interface{}{
		"cached_configs":    configCount,
		"cache_ttl_minutes": r.cacheTTL.Minutes(),
	}
}

// =============================================
// FEE ROUTING — a qué cuenta merchant va el fee de Pull
// =============================================

// PlatformFeeVenueID: si está seteado (env PLATFORM_FEE_VENUE_ID), el fee de
// servicio se cobra contra las credenciales de ESA fila de
// payment_gateway_credentials (la cuenta Cybersource de PULL) en vez de la
// del venue. Vacío = modo una-sola-cuenta (todo por la cuenta del venue,
// como en sandbox). Así, cuando NeoNet habilite la cuenta del venue:
//   fila del venue  -> credenciales del venue  (recibe el precio de entrada)
//   fila de Pull    -> credenciales de Pull    (recibe el fee)
//   PLATFORM_FEE_VENUE_ID=<id de la fila de Pull> en fly.prod.toml
func PlatformFeeVenueID() string {
	return strings.TrimSpace(os.Getenv("PLATFORM_FEE_VENUE_ID"))
}

// GetFeeProcessor devuelve el procesador por el que se cobra el FEE de Pull:
// el de la plataforma si está configurado, o el del venue si no.
func (r *PaymentRouter) GetFeeProcessor(ctx context.Context, venueID string) (PaymentProcessor, string, error) {
	if plat := PlatformFeeVenueID(); plat != "" {
		p, err := r.GetProcessor(ctx, plat)
		if err != nil {
			// La config de plataforma existe pero falla → NO cobrar el fee por
			// el carril equivocado en silencio: que el caller decida.
			return nil, plat, err
		}
		return p, plat, nil
	}
	p, err := r.GetProcessor(ctx, venueID)
	return p, "", err
}
