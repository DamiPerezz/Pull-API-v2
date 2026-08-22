package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"pull-api-v2/models"
)

// =============================================
// NEONET (CYBERSOURCE) — real implementation
// The stub methods in payment_router.go were replaced by these. The flow is
// direct-card: controllers.PayOrder charges the two atomic sales (venue
// share + Pull fee) via ChargeCard, registers the result here, and then
// delegates to the shared ConfirmPayment path, which reads it back through
// NeoNetProcessor.ConfirmPayment.
// =============================================

// neonetVerified holds payment results for sessions charged in-request, so
// the shared ConfirmPayment path can pick them up without a second network
// round-trip. Same-process by design: PayOrder charges and confirms within
// one HTTP request.
var neonetVerified sync.Map // sessionID -> *models.PaymentResult

// RegisterNeoNetPayment stores a verified charge under its session id.
func RegisterNeoNetPayment(sessionID string, result *models.PaymentResult) {
	neonetVerified.Store(sessionID, result)
}

func (p *NeoNetProcessor) client() (*CybersourceClient, error) {
	c := p.config.Credentials
	if c == nil || c.NeoNetMerchantID == "" || c.NeoNetAccessKey == "" || c.NeoNetSecretKey == "" {
		return nil, fmt.Errorf("neonet credentials not configured (need merchant_id, access_key, secret_key)")
	}
	return NewCybersourceClient(c.NeoNetMerchantID, c.NeoNetAccessKey, c.NeoNetSecretKey, p.config.Environment), nil
}

func brandFor(number string) string {
	switch cardTypeFor(number) {
	case "001":
		return "visa"
	case "002":
		return "mastercard"
	case "003":
		return "amex"
	default:
		return "card"
	}
}

// ChargeCard performs one Cybersource sale (auth+capture).
//
// Si params.TransientTokenJWT viene informado (Apple/Google Pay vía Unified
// Checkout), el medio de pago es ese token y params.Card se ignora. Vacío =
// carril de tarjeta de siempre.
//
// params.Device (IP, User-Agent, huella) viaja SIEMPRE que venga informado, por
// los dos carriles: es lo que Decision Manager usa para no confundir a 200
// compradores con un solo dispositivo.
func (p *NeoNetProcessor) ChargeCard(ctx context.Context, params ChargeParams) (*ChargeResult, error) {
	cli, err := p.client()
	if err != nil {
		return nil, err
	}
	sale, err := cli.Sale(ctx, params.ReferenceCode, params.Amount, params.Currency, params.Card, params.BillTo, params.Capture, params.TransientTokenJWT, params.Device)
	if err != nil {
		return nil, err
	}
	// La marca sale del número cuando hay número. Con token no lo hay, así que
	// se usa la que devolvió la pasarela (brandFor("") daría "card").
	brand := brandFor(params.Card.Number)
	if sale.CardBrand != "" {
		brand = sale.CardBrand
	}

	// El mensaje de rechazo se TRADUCE aquí, en la frontera con la pasarela, y
	// no en cada sitio que lo enseña. Lo que sale de Cybersource viene en
	// inglés y nombrando sus sistemas internos ("The order has been rejected by
	// Decision Manager"), y `pay_controller` lo mandaba tal cual al navegador
	// del comprador. Traducir en el borde garantiza que ningún camino futuro se
	// olvide de hacerlo.
	//
	// El original NO se pierde: `Sale()` ya lo registra con su código en el log
	// (`[Cybersource] sale NOT approved ... reason=...`), que es donde sirve.
	humano := sale.ErrorMessage
	if !sale.Success {
		humano = DeclineMessage(sale.ErrorReason, sale.ErrorMessage)
	}

	return &ChargeResult{
		Success:          sale.Success,
		TransactionID:    sale.PaymentID,
		AuthCode:         sale.AuthCode,
		CardLast4:        sale.CardLast4,
		CardBrand:        brand,
		AuthorizedAmount: sale.AuthorizedAmount,
		ErrorMessage:     humano,
		// Lo que la pasarela dice que hizo con el dinero (retener o cobrar),
		// que el llamador contrasta con lo que pidió.
		CaptureState:    sale.CaptureState,
		CaptureEvidence: sale.CaptureEvidence,
	}, nil
}

// CaptureContext abre una sesión de Unified Checkout con las credenciales del
// venue. Implementa UnifiedCheckoutProvider.
func (p *NeoNetProcessor) CaptureContext(ctx context.Context, params CaptureContextParams) (string, error) {
	cli, err := p.client()
	if err != nil {
		return "", err
	}
	return cli.CaptureContext(ctx, params)
}

// CapturePayment settles a held authorization (private-event approval).
func (p *NeoNetProcessor) CapturePayment(ctx context.Context, transactionID, referenceCode string, amount float64, currency string) error {
	cli, err := p.client()
	if err != nil {
		return err
	}
	return cli.Capture(ctx, transactionID, referenceCode, amount, currency)
}

// ReverseCharge undoes an authorization (rollback of the atomic pair, or
// releasing a held authorization when a private-event order is rejected).
func (p *NeoNetProcessor) ReverseCharge(ctx context.Context, transactionID, referenceCode string, amount float64, currency string) error {
	cli, err := p.client()
	if err != nil {
		return err
	}
	return cli.Reverse(ctx, transactionID, referenceCode, amount, currency)
}

// RefundCharge deshace una venta ya capturada (rollback del par atómico en
// público cuando la 2ª tx falla tras capturarse la 1ª). Usa /refunds, no
// /reversals — una venta liquidada no se revierte con un auth-reversal.
func (p *NeoNetProcessor) RefundCharge(ctx context.Context, transactionID, referenceCode string, amount float64, currency string) error {
	cli, err := p.client()
	if err != nil {
		return err
	}
	return cli.Refund(ctx, transactionID, referenceCode, amount, currency)
}

// neonetConfirmPayment resolves a session charged by PayOrder.
func (p *NeoNetProcessor) neonetConfirmPayment(sessionID string) (*models.PaymentResult, error) {
	if v, ok := neonetVerified.LoadAndDelete(sessionID); ok {
		return v.(*models.PaymentResult), nil
	}
	return nil, fmt.Errorf("neonet session %s not found — payment must go through /orders/pay", sessionID)
}

// neonetProcessRefund issues a follow-on refund.
func (p *NeoNetProcessor) neonetProcessRefund(ctx context.Context, transactionID string, amount float64) error {
	cli, err := p.client()
	if err != nil {
		return err
	}
	currency := p.config.DefaultCurrency
	if currency == "" {
		currency = "GTQ"
	}
	return cli.Refund(ctx, transactionID, "refund-"+transactionID, amount, currency)
}

// neonetValidateWebhook verifies an HMAC-SHA256 hex signature over the payload.
func (p *NeoNetProcessor) neonetValidateWebhook(payload []byte, signature string) (bool, error) {
	c := p.config.Credentials
	if c == nil || c.NeoNetSecretKey == "" {
		return false, fmt.Errorf("neonet secret not configured")
	}
	mac := hmac.New(sha256.New, []byte(c.NeoNetSecretKey))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature)), nil
}
