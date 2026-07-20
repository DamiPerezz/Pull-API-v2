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
func (p *NeoNetProcessor) ChargeCard(ctx context.Context, params ChargeParams) (*ChargeResult, error) {
	cli, err := p.client()
	if err != nil {
		return nil, err
	}
	sale, err := cli.Sale(ctx, params.ReferenceCode, params.Amount, params.Currency, params.Card, params.BillTo, params.Capture)
	if err != nil {
		return nil, err
	}
	return &ChargeResult{
		Success:       sale.Success,
		TransactionID: sale.PaymentID,
		AuthCode:      sale.AuthCode,
		CardLast4:     sale.CardLast4,
		CardBrand:     brandFor(params.Card.Number),
		ErrorMessage:  sale.ErrorMessage,
	}, nil
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
