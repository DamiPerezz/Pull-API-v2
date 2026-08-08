package models

import "time"

// =============================================
// PAYMENT GATEWAY TYPES
// =============================================

// PaymentGateway represents supported payment providers
type PaymentGateway string

const (
	GatewayStripe      PaymentGateway = "stripe"
	GatewayNeoNet      PaymentGateway = "neonet"
	GatewayMercadoPago PaymentGateway = "mercadopago"
	// GatewayDLocal es dLocal Go (docs.dlocalgo.com), la pasarela que sustituye
	// a NeoNet/Cybersource. Cobra por CHECKOUT ALOJADO: el pago nace PENDING y
	// se confirma por webhook — NO en la misma petición HTTP.
	// OJO: el valor 'dlocal' tiene que existir en el enum `payment_gateway_type`
	// de la BD (migración sql/dlocal_fase0.sql) antes de escribirlo en `orders`.
	GatewayDLocal   PaymentGateway = "dlocal"
	GatewayCash     PaymentGateway = "cash"
	GatewayTransfer PaymentGateway = "transfer"
)

func (g PaymentGateway) IsValid() bool {
	switch g {
	case GatewayStripe, GatewayNeoNet, GatewayMercadoPago, GatewayDLocal, GatewayCash, GatewayTransfer:
		return true
	}
	return false
}

func (g PaymentGateway) String() string {
	return string(g)
}

// TransactionType for central transactions
type TransactionType string

const (
	TxTypeIndividualTicket TransactionType = "individual_ticket"
	TxTypeGroupOrganizer   TransactionType = "group_organizer"
	TxTypeGroupGuest       TransactionType = "group_guest"
	TxTypeVipListOrganizer TransactionType = "vip_list_organizer"
	TxTypeVipListGuest     TransactionType = "vip_list_guest"
	TxTypeRefund           TransactionType = "refund"
)

// TransactionStatus for central transactions
type TransactionStatus string

const (
	TxStatusPending           TransactionStatus = "pending"
	TxStatusProcessing        TransactionStatus = "processing"
	TxStatusCompleted         TransactionStatus = "completed"
	TxStatusFailed            TransactionStatus = "failed"
	TxStatusRefunded          TransactionStatus = "refunded"
	TxStatusPartiallyRefunded TransactionStatus = "partially_refunded"
)

// =============================================
// VENUE PAYMENT CONFIG (Central DB)
// Matches pull-central.payment_gateway_credentials
// =============================================

// VenuePaymentConfig stores payment gateway credentials for a venue
type VenuePaymentConfig struct {
	ID          string         `json:"id"`
	VenueID     string         `json:"venue_id"`
	Gateway     PaymentGateway `json:"gateway"`
	GatewayName string         `json:"gateway_name,omitempty"`
	Environment string         `json:"environment"` // test, sandbox, production
	IsActive    bool           `json:"is_active"`
	IsPrimary   bool           `json:"is_primary"`
	Priority    int            `json:"priority"`

	// Fees
	PlatformFeePercent float64 `json:"platform_fee_percent"`
	PlatformFeeFixed   float64 `json:"platform_fee_fixed"`
	GatewayFeePercent  float64 `json:"gateway_fee_percent"`
	GatewayFeeFixed    float64 `json:"gateway_fee_fixed"`

	// Currency
	DefaultCurrency     string   `json:"default_currency"`
	SupportedCurrencies []string `json:"supported_currencies"`

	// Decrypted at runtime (never stored)
	Credentials *GatewayCredentials `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GatewayCredentials holds decrypted credentials
type GatewayCredentials struct {
	// Stripe
	StripeAccountID      string `json:"stripe_account_id,omitempty"`
	StripePublishableKey string `json:"stripe_publishable_key,omitempty"`
	StripeSecretKey      string `json:"stripe_secret_key,omitempty"`
	StripeWebhookSecret  string `json:"stripe_webhook_secret,omitempty"`

	// NeoNet/Cybersource
	NeoNetProfileID  string `json:"profile_id,omitempty"`
	NeoNetAccessKey  string `json:"access_key,omitempty"`
	NeoNetSecretKey  string `json:"secret_key,omitempty"`
	NeoNetMerchantID string `json:"merchant_id,omitempty"`
	NeoNetTerminalID string `json:"terminal_id,omitempty"`

	// MercadoPago
	MPPublicKey   string `json:"mercadopago_public_key,omitempty"`
	MPAccessToken string `json:"mercadopago_access_token,omitempty"`

	// dLocal Go. Mapeo con las columnas de `payment_gateway_credentials`:
	//   DLocalAPIKey    <- access_key            (mitad pública del Bearer)
	//   DLocalSecretKey <- secret_key_encrypted  (descifrado; NUNCA se loguea)
	//   DLocalSplitCode <- split_code            (columna opcional; si no existe
	//                      se cae a la env DLOCAL_SPLIT_CODE)
	// El Bearer se arma como "<api_key>:<secret_key>" — ver services/dlocalgo.go.
	DLocalAPIKey    string `json:"dlocal_api_key,omitempty"`
	DLocalSecretKey string `json:"dlocal_secret_key,omitempty"`
	DLocalSplitCode string `json:"dlocal_split_code,omitempty"`
	// DLocalSmartFieldsKey es PÚBLICA por diseño (va al navegador). Hoy no se
	// usa: el checkout es alojado. Queda para la fase SmartFields.
	DLocalSmartFieldsKey string `json:"dlocal_smartfields_key,omitempty"`
}

// =============================================
// TRANSACTIONS (Central DB)
// Matches pull-central.transactions table
// =============================================

// Transaction records all platform transactions
type Transaction struct {
	ID                string            `json:"id"`
	TransactionNumber string            `json:"transaction_number"`
	TransactionType   TransactionType   `json:"transaction_type"`
	Status            TransactionStatus `json:"status"`

	// Amounts
	GrossAmount        float64 `json:"gross_amount"`
	Currency           string  `json:"currency"`
	PlatformFeePercent float64 `json:"platform_fee_percent"`
	PlatformFeeAmount  float64 `json:"platform_fee_amount"`
	GatewayFeePercent  float64 `json:"gateway_fee_percent"`
	GatewayFeeFixed    float64 `json:"gateway_fee_fixed"`
	GatewayFeeAmount   float64 `json:"gateway_fee_amount"`
	NetToVenue         float64 `json:"net_to_venue"`

	// References
	VenueID            string  `json:"venue_id"`
	OrganizationID     string  `json:"organization_id"`
	EventID            *string `json:"event_id,omitempty"`
	UserID             string  `json:"user_id"`
	OrderID            *string `json:"order_id,omitempty"`
	GroupReservationID *string `json:"group_reservation_id,omitempty"`
	GroupGuestID       *string `json:"group_guest_id,omitempty"`
	VipListID          *string `json:"vip_list_id,omitempty"`
	VipListGuestID     *string `json:"vip_list_guest_id,omitempty"`
	TicketID           *string `json:"ticket_id,omitempty"`

	// Payment Gateway
	PaymentGateway PaymentGateway `json:"payment_gateway"`

	// Stripe specific
	StripePaymentIntent      *string `json:"stripe_payment_intent,omitempty"`
	StripeChargeID           *string `json:"stripe_charge_id,omitempty"`
	StripeSessionID          *string `json:"stripe_session_id,omitempty"`
	StripeTransferID         *string `json:"stripe_transfer_id,omitempty"`
	StripeBalanceTransaction *string `json:"stripe_balance_transaction,omitempty"`
	StripeRefundID           *string `json:"stripe_refund_id,omitempty"`

	// NeoNet specific
	NeoNetTransactionID     *string `json:"neonet_transaction_id,omitempty"`
	NeoNetAuthorizationCode *string `json:"neonet_authorization_code,omitempty"`
	NeoNetReference         *string `json:"neonet_reference,omitempty"`
	NeoNetReasonCode        *string `json:"neonet_reason_code,omitempty"`

	// MercadoPago specific
	MercadoPagoPaymentID    *string `json:"mercadopago_payment_id,omitempty"`
	MercadoPagoPreferenceID *string `json:"mercadopago_preference_id,omitempty"`

	// Payer info
	PayerName  *string `json:"payer_name,omitempty"`
	PayerEmail *string `json:"payer_email,omitempty"`
	PayerPhone *string `json:"payer_phone,omitempty"`

	// Card info
	CardLast4 *string `json:"card_last4,omitempty"`
	CardBrand *string `json:"card_brand,omitempty"`
	CardType  *string `json:"card_type,omitempty"`

	// Extra
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
	InternalNotes *string                `json:"internal_notes,omitempty"`

	// Refund
	OriginalTransactionID *string  `json:"original_transaction_id,omitempty"`
	RefundReason          *string  `json:"refund_reason,omitempty"`
	RefundedAmount        float64  `json:"refunded_amount"`

	// Timestamps
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
	RefundedAt *time.Time `json:"refunded_at,omitempty"`
	FailedAt   *time.Time `json:"failed_at,omitempty"`
}

// PlatformTransaction is an alias for creating transactions
// Used by RecordPlatformTransaction
type PlatformTransaction = Transaction

// =============================================
// CHECKOUT TYPES
// =============================================

// CheckoutParams for creating payment sessions
type CheckoutParams struct {
	VenueID        string
	OrganizationID string
	EventID        string
	UserID         string
	OrderID        string
	Amount         float64
	Currency       string
	ProductName    string
	ProductImage   string
	SuccessURL     string
	CancelURL     string
	CustomerEmail  string
	CustomerName   string
	Metadata       map[string]string
}

// CheckoutResult from payment gateway.
//
// SessionID es el identificador con el que la orden localiza después el pago:
// se guarda en `orders.stripe_session_id` (columna reutilizada por todas las
// pasarelas) y es lo que recibe PaymentProcessor.ConfirmPayment.
// CheckoutURL es la URL a la que hay que mandar al comprador (en dLocal Go, el
// `redirect_url` del checkout alojado).
type CheckoutResult struct {
	SessionID   string
	CheckoutURL string
	Gateway     PaymentGateway

	// PaymentID es el id NATIVO del pago en la pasarela, sin prefijar (en dLocal
	// Go, el `id` de POST /v1/payments). SessionID lo lleva prefijado; este campo
	// evita tener que deshacer el prefijo a mano.
	PaymentID string
	// Status es el estado con el que NACE el pago ("PENDING" en dLocal Go).
	// Recordatorio de que el checkout alojado NO confirma en la misma petición.
	Status string
}

// PaymentResult after payment confirmation
type PaymentResult struct {
	Success           bool
	TransactionID     string
	AuthorizationCode string
	Gateway           PaymentGateway
	ErrorMessage      string
	CardLast4         string
	CardBrand         string
}
