package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// =============================================================================
// dLOCAL GO — cliente REST.
//
// OJO: dLocal tiene DOS productos distintos. Esto es **dLocal Go** (el de
// autoservicio, docs.dlocalgo.com), NO la API Payins de docs.dlocal.com.
// Diferencias que importan:
//   - Auth simple: `Authorization: Bearer <API_KEY>:<SECRET_KEY>` (sin HMAC).
//   - El cobro con tarjeta va por SmartFields: el navegador tokeniza la
//     tarjeta (nunca toca nuestro backend) y confirmamos con ese token.
//   - Split nativo del fee vía `split_code` — dLocal reparte solo.
//   - NO soporta pre-autorización/captura diferida (la retención de 48h de
//     los eventos privados NO es posible aquí; ver DLOCAL-MIGRACION.md §3.2).
//
// Docs: https://docs.dlocalgo.com/integration-api
// =============================================================================

const (
	dlocalGoProdHost    = "https://api.dlocalgo.com"
	dlocalGoSandboxHost = "https://api-sbx.dlocalgo.com"
)

// Estados de un pago en dLocal Go.
const (
	DLocalStatusPending   = "PENDING"
	DLocalStatusPaid      = "PAID"
	DLocalStatusRejected  = "REJECTED"
	DLocalStatusCancelled = "CANCELLED"
	DLocalStatusExpired   = "EXPIRED"
)

// dlocalGoClient habla con la API de dLocal Go. Reutiliza un http.Client
// compartido (mismo patrón que el cliente de Cybersource) para no abrir un
// pool de conexiones nuevo por cobro.
type dlocalGoClient struct {
	apiKey    string
	secretKey string
	baseURL   string
	client    *http.Client
}

// NewDLocalGoClient construye el cliente. environment "production"/"live" usa
// el host real; cualquier otro valor (test/sandbox/vacío) usa el sandbox —
// fail-safe: si el dato de la BD viene raro, NO se cobra dinero real.
func NewDLocalGoClient(apiKey, secretKey, environment string) *dlocalGoClient {
	host := dlocalGoSandboxHost
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "production", "live":
		host = dlocalGoProdHost
	}
	return &dlocalGoClient{
		apiKey:    apiKey,
		secretKey: secretKey,
		baseURL:   host,
		client:    &http.Client{Timeout: 25 * time.Second},
	}
}

// do ejecuta una petición autenticada. Devuelve el body crudo y el status.
// El Authorization NUNCA se loguea.
func (c *dlocalGoClient) do(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey+":"+c.secretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "PullEvents/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("dlocal request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}

// DLocalAccount es la respuesta de /v1/me — sirve para verificar credenciales.
type DLocalAccount struct {
	ID    interface{} `json:"id"`
	Email string      `json:"email"`
	Name  string      `json:"name"`
}

// Me verifica que las credenciales son válidas (GET /v1/me).
func (c *dlocalGoClient) Me(ctx context.Context) (*DLocalAccount, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("dlocal /v1/me HTTP %d: %s", status, truncate(string(body), 200))
	}
	var acc DLocalAccount
	if err := json.Unmarshal(body, &acc); err != nil {
		return nil, fmt.Errorf("decode /v1/me: %w", err)
	}
	return &acc, nil
}

// DLocalPaymentRequest es el cuerpo de POST /v1/payments.
type DLocalPaymentRequest struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Country  string  `json:"country,omitempty"`
	OrderID  string  `json:"order_id,omitempty"`

	// AllowTransparent=true devuelve merchant_checkout_token para SmartFields
	// (formulario de tarjeta embebido en NUESTRA web).
	AllowTransparent bool `json:"allow_transparent,omitempty"`

	// SplitCode reparte el cobro entre venue (seller) y Pull (collaborator).
	// Vacío = sin split (todo a la cuenta que cobra).
	SplitCode string `json:"split_code,omitempty"`

	Description     string `json:"description,omitempty"`
	NotificationURL string `json:"notification_url,omitempty"`
	SuccessURL      string `json:"success_url,omitempty"`
	BackURL         string `json:"back_url,omitempty"`
	PaymentType     string `json:"payment_type,omitempty"` // p.ej. "CREDIT_CARD"

	Payer *DLocalPayer `json:"payer,omitempty"`
}

// DLocalPayer son los datos del comprador.
type DLocalPayer struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	Document string `json:"document,omitempty"`
	Phone    string `json:"phone,omitempty"`
}

// DLocalPayment es la respuesta de crear/consultar un pago.
type DLocalPayment struct {
	ID       interface{} `json:"id"`
	Status   string      `json:"status"`
	Amount   float64     `json:"amount"`
	Currency string      `json:"currency"`
	OrderID  string      `json:"order_id"`

	// RedirectURL: checkout alojado de dLocal (si no usamos SmartFields).
	RedirectURL string `json:"redirect_url"`
	// MerchantCheckoutToken: necesario para SmartFields (allow_transparent).
	MerchantCheckoutToken string `json:"merchant_checkout_token"`

	RejectedReason string `json:"rejected_reason"`
	CreatedDate    string `json:"created_date"`
	ApprovedDate   string `json:"approved_date"`

	// Datos de la tarjeta que devuelve dLocal (nunca el PAN completo).
	BIN       string `json:"bin"`
	LastFour  string `json:"last_four"`
	Issuer    string `json:"issuer"`
	CardBrand string `json:"card_brand"`
}

// PaymentID devuelve el id del pago como string (dLocal lo manda como número
// o cadena según el endpoint).
func (p *DLocalPayment) PaymentID() string { return idToString(p.ID) }

// IsPaid indica si el pago está cobrado.
func (p *DLocalPayment) IsPaid() bool { return p.Status == DLocalStatusPaid }

// CreatePayment crea un pago (POST /v1/payments).
func (c *dlocalGoClient) CreatePayment(ctx context.Context, req DLocalPaymentRequest) (*DLocalPayment, error) {
	body, status, err := c.do(ctx, http.MethodPost, "/v1/payments", req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("crear pago dLocal HTTP %d: %s", status, truncate(string(body), 300))
	}
	var p DLocalPayment
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decode pago: %w", err)
	}
	return &p, nil
}

// DLocalConfirmRequest es el cuerpo de POST /v1/payments/confirm/{token}.
//
// OJO CON LOS NOMBRES: este endpoint usa camelCase (`cardToken`,
// `clientEmail`, `installmentsId`), a diferencia del resto de la API de dLocal
// Go, que usa snake_case. Mandar `token` o `installments_id` NO da un error
// claro: dLocal responde 400 con {"code":406,"message":"Missing payment
// method"}, que suena a un problema de configuración de la cuenta y manda a
// buscar en el sitio equivocado. Verificado contra producción el 2026-08-15.
// Fuente: ejemplo oficial bitbucket.org/directopago/dgo-smartfields-sample
// (server.js, /api/confirm-payment).
type DLocalConfirmRequest struct {
	CardToken string `json:"cardToken"`

	// Datos del comprador. dLocal los usa para el antifraude del emisor; sin
	// ellos aumentan los rechazos. Se mandan solo si los tenemos.
	ClientFirstName    string `json:"clientFirstName,omitempty"`
	ClientLastName     string `json:"clientLastName,omitempty"`
	ClientEmail        string `json:"clientEmail,omitempty"`
	ClientDocumentType string `json:"clientDocumentType,omitempty"`
	ClientDocument     string `json:"clientDocument,omitempty"`

	InstallmentsID string `json:"installmentsId,omitempty"`
}

// ConfirmPayment confirma un pago transparente con el token de tarjeta que
// generó SmartFields en el navegador
// (POST /v1/payments/confirm/{merchant_checkout_token}).
func (c *dlocalGoClient) ConfirmPayment(ctx context.Context, checkoutToken string, req DLocalConfirmRequest) (*DLocalPayment, error) {
	body, status, err := c.do(ctx, http.MethodPost, "/v1/payments/confirm/"+checkoutToken, req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("confirmar pago dLocal HTTP %d: %s", status, truncate(string(body), 300))
	}
	var p DLocalPayment
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decode confirmación: %w", err)
	}
	return &p, nil
}

// GetPayment consulta un pago (GET /v1/payments/{id}).
func (c *dlocalGoClient) GetPayment(ctx context.Context, paymentID string) (*DLocalPayment, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/v1/payments/"+paymentID, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("consultar pago dLocal HTTP %d: %s", status, truncate(string(body), 200))
	}
	var p DLocalPayment
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decode pago: %w", err)
	}
	return &p, nil
}

// DLocalRefund es la respuesta de un reembolso.
type DLocalRefund struct {
	ID        interface{} `json:"id"`
	PaymentID interface{} `json:"payment_id"`
	Amount    float64     `json:"amount"`
	Currency  string      `json:"currency"`
	Status    string      `json:"status"` // PENDING | SUCCESS | REJECTED | CANCELLED
}

// Refund reembolsa un pago (POST /v1/refunds). Amount 0 = reembolso total.
// ⚠️ ASÍNCRONO: la respuesta suele venir PENDING y el resultado final llega
// por webhook al notification_url. No asumir éxito por un 200.
func (c *dlocalGoClient) Refund(ctx context.Context, paymentID string, amount float64, notificationURL string) (*DLocalRefund, error) {
	payload := map[string]interface{}{"payment_id": paymentID}
	if amount > 0 {
		payload["amount"] = amount
	}
	if notificationURL != "" {
		payload["notification_url"] = notificationURL
	}
	body, status, err := c.do(ctx, http.MethodPost, "/v1/refunds", payload)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("reembolso dLocal HTTP %d: %s", status, truncate(string(body), 300))
	}
	var r DLocalRefund
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode reembolso: %w", err)
	}
	return &r, nil
}

// DLocalCollaboration es una colaboración de split payments.
type DLocalCollaboration struct {
	ID                     interface{} `json:"id"`
	Code                   string      `json:"code"` // el split_code que va en el pago
	Name                   string      `json:"name"`
	Role                   string      `json:"role"` // SELLER | COLLABORATOR
	CollaboratorCommission float64     `json:"collaborator_commission"`
	Category               string      `json:"category"`
	Status                 string      `json:"status"` // ACTIVE cuando está aceptada
}

// ListCollaborations lista las colaboraciones de split (GET /v1/collaborations).
// Sirve para obtener el `code` que se manda como split_code al cobrar.
func (c *dlocalGoClient) ListCollaborations(ctx context.Context) ([]DLocalCollaboration, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/v1/collaborations", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("colaboraciones dLocal HTTP %d: %s", status, truncate(string(body), 200))
	}
	// La API puede devolver un array plano o envuelto en {data:[...]}.
	var direct []DLocalCollaboration
	if err := json.Unmarshal(body, &direct); err == nil {
		return direct, nil
	}
	var wrapped struct {
		Data []DLocalCollaboration `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("decode colaboraciones: %w", err)
	}
	return wrapped.Data, nil
}

// idToString normaliza un id que dLocal puede mandar como número o cadena.
func idToString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
