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

	// TransientTokenJWT (OPCIONAL) es el token de un solo uso que devuelve
	// Unified Checkout cuando el comprador paga con Apple Pay o Google Pay.
	// Cuando viene informado SUSTITUYE a Card: el PAN no existe en ese flujo.
	// Vacío = carril de tarjeta de siempre, sin ningún cambio de comportamiento.
	TransientTokenJWT string
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

	// CaptureState es lo que la PASARELA dice que pasó con el dinero, que no
	// tiene por qué coincidir con el Capture que se pidió:
	//
	//	CybsCaptureHeld    → autorizado y sin cobrar (se puede capturar/reversar)
	//	CybsCaptureSettled → ya cobrado (deshacerlo exige REEMBOLSO)
	//	CybsCaptureUnknown → la respuesta no lo dice; quédate con lo que pediste
	//
	// Existe porque en una retención de evento privado la diferencia decide si
	// al aprobar hay que capturar o no, y si al rechazar hay que reversar o
	// devolver. Ver captureStateFromResponse en cybersource.go.
	CaptureState CybsCaptureState
	// CaptureEvidence: la señal concreta que dio ese veredicto, para logs.
	CaptureEvidence string
}

// UnifiedCheckoutProvider lo implementan las pasarelas capaces de abrir una
// sesión de Unified Checkout (los botones de Apple Pay / Google Pay). Va
// aparte de DirectCardCharger a propósito: una pasarela puede saber cobrar una
// tarjeta y no ofrecer wallets, y el controlador tiene que poder distinguirlo
// para responder algo útil en vez de un 500.
type UnifiedCheckoutProvider interface {
	// CaptureContext devuelve el JWT de sesión que consume el SDK del
	// navegador. Es texto plano: se devuelve tal cual, sin reserializar.
	CaptureContext(ctx context.Context, params CaptureContextParams) (string, error)
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
