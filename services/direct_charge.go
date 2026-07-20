package services

import "context"

// =============================================
// DIRECT CARD CHARGING
// The NeoNet/Cybersource flow collects the card on our own payment page and
// charges server-side (no hosted redirect). Processors that support this
// implement DirectCardCharger in addition to PaymentProcessor.
// =============================================

// ChargeParams describes one sale. Capture=true charges immediately (public
// events); Capture=false authorizes/holds the funds without charging, to be
// settled later via CapturePayment or released via ReverseCharge (private
// events with staff approval).
type ChargeParams struct {
	ReferenceCode string
	Amount        float64
	Currency      string
	Card          CybsCard
	BillTo        CybsBillTo
	Capture       bool
}

// ChargeResult is the outcome of one sale.
type ChargeResult struct {
	Success       bool
	TransactionID string
	AuthCode      string
	CardLast4     string
	CardBrand     string
	ErrorMessage  string
}

// DirectCardCharger is implemented by processors that can charge a raw card.
type DirectCardCharger interface {
	ChargeCard(ctx context.Context, params ChargeParams) (*ChargeResult, error)
	// CapturePayment settles a held authorization (private-event approval).
	CapturePayment(ctx context.Context, transactionID, referenceCode string, amount float64, currency string) error
	// ReverseCharge releases a held authorization (private-event rejection,
	// or rollback of the atomic pair).
	ReverseCharge(ctx context.Context, transactionID, referenceCode string, amount float64, currency string) error
}
