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
	Success          bool
	TransactionID    string
	AuthCode         string
	CardLast4        string
	CardBrand        string
	AuthorizedAmount float64 // < Amount pedido si la autorización fue parcial
	ErrorMessage     string
}

// DirectCardCharger is implemented by processors that can charge a raw card.
type DirectCardCharger interface {
	ChargeCard(ctx context.Context, params ChargeParams) (*ChargeResult, error)
	// CapturePayment settles a held authorization (private-event approval).
	CapturePayment(ctx context.Context, transactionID, referenceCode string, amount float64, currency string) error
	// ReverseCharge releases a held authorization (private-event rejection,
	// or rollback of the atomic pair). SOLO válido sobre una autorización NO
	// capturada; una venta ya liquidada (capture=true) NO se deshace con esto.
	ReverseCharge(ctx context.Context, transactionID, referenceCode string, amount float64, currency string) error
	// RefundCharge deshace una venta YA CAPTURADA/liquidada (capture=true):
	// rollback del par atómico en eventos públicos cuando la 2ª tx falla tras
	// haberse capturado la 1ª. Un reversal no sirve para una venta liquidada.
	RefundCharge(ctx context.Context, transactionID, referenceCode string, amount float64, currency string) error
}
