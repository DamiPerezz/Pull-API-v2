package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"

	"pull-api-v2/config"
	"pull-api-v2/models"
)

// =============================================================================
// dLOCAL GO — procesador de pagos (CHECKOUT ALOJADO)
//
// Cómo cobra esto, en una frase: creamos el pago en dLocal, dLocal nos devuelve
// una `redirect_url`, mandamos ahí al comprador, y el dinero se confirma
// DESPUÉS — por webhook o consultando el estado.
//
// LO QUE CAMBIA RESPECTO A NEONET (y hay que tener presente al leer esto):
//
//   - El pago NO se confirma en la misma petición HTTP. Nace PENDING.
//     CreateCheckout devolver "ok" NO significa que haya dinero: significa que
//     el comprador tiene a dónde ir a pagar.
//   - Los tickets se emiten cuando el pago pasa a PAID, por el carril
//     compartido (controllers.ConfirmPayment), disparado desde el webhook
//     (controllers/dlocal_webhook.go) o desde la vuelta del comprador.
//   - NO hay tarjeta cruda: dLocal Go no acepta PAN. Por eso este procesador
//     NO implementa DirectCardCharger a propósito (ver abajo).
//   - El fee del 8% de Pull lo reparte dLocal solo, con `split_code`. No hay
//     dos transacciones ni rollback asimétrico como con Cybersource.
//   - El reembolso es ASÍNCRONO: nace PENDING y lo confirma un webhook.
//
// El cliente HTTP vive en services/dlocalgo.go — aquí solo se traduce entre
// ese cliente y las interfaces de la plataforma.
// Diseño: DLOCAL-MIGRACION.md y DLOCAL-FLUJO-PRIVADO.md (raíz del workspace).
// =============================================================================

// Comprobación en tiempo de compilación de que cumplimos el contrato.
var _ PaymentProcessor = (*DLocalProcessor)(nil)

// DLocalSessionPrefix prefija el `stripe_session_id` de las órdenes pagadas por
// dLocal, igual que "mock_" y "neonet_" en las otras pasarelas.
//
// El formato vive AQUÍ y en ningún otro sitio: quien necesite construirlo o
// deshacerlo (el webhook, el endpoint de estado, la web) usa DLocalSessionID /
// DLocalPaymentIDFromSession. Un segundo sitio que "sepa" el prefijo es un
// desajuste esperando a pasar: la orden se guardaría con una forma y se
// buscaría con otra, y el pago quedaría cobrado sin ticket.
const DLocalSessionPrefix = "dlocal_"

// DLocalSessionID construye el session id canónico de un pago de dLocal.
// Normaliza (recorta espacios, no re-prefija) para que el valor que se GUARDA
// en la orden y el que se BUSCA después sean byte a byte el mismo.
func DLocalSessionID(paymentID string) string {
	id := strings.TrimSpace(paymentID)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, DLocalSessionPrefix) {
		return DLocalSessionPrefix + DLocalPaymentIDFromSession(id)
	}
	return DLocalSessionPrefix + id
}

// DLocalPaymentIDFromSession deshace DLocalSessionID. Tolera que le pasen el id
// desnudo (sin prefijo): así ConfirmPayment funciona con ambas formas y una
// orden antigua no se queda sin poder confirmarse.
func DLocalPaymentIDFromSession(sessionID string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sessionID), DLocalSessionPrefix))
}

// ErrDLocalRefundPending indica que dLocal ACEPTÓ el reembolso pero todavía no
// lo ha liquidado (nace PENDING y lo confirma por webhook).
//
// NO es un fallo: el dinero probablemente vuelva. Tampoco es un éxito: no se
// puede dar por devuelto. Quien lo reciba debe registrarlo y esperar la
// notificación — NO reintentar el reembolso a ciegas (se reembolsaría dos
// veces). Distínguelo con errors.Is(err, services.ErrDLocalRefundPending).
var ErrDLocalRefundPending = errors.New("reembolso dLocal aceptado pero PENDIENTE de confirmación por webhook")

// ErrDLocalNoRawCard es el error que se devuelve si algún flujo intenta cobrar
// una tarjeta cruda por dLocal Go.
var ErrDLocalNoRawCard = errors.New("dLocal Go no acepta tarjeta cruda; usar checkout alojado (CreateCheckout → redirect_url)")

// DLocalProcessor implementa PaymentProcessor sobre dLocal Go.
type DLocalProcessor struct {
	config *models.VenuePaymentConfig
}

// NewDLocalProcessor construye el procesador de un venue a partir de su fila de
// `payment_gateway_credentials` (ya descifrada por el PaymentRouter).
func NewDLocalProcessor(cfg *models.VenuePaymentConfig) *DLocalProcessor {
	return &DLocalProcessor{config: cfg}
}

func (p *DLocalProcessor) GetGateway() models.PaymentGateway {
	return models.GatewayDLocal
}

// =============================================================================
// CREDENCIALES
// =============================================================================

// client arma el cliente HTTP de dLocal con las credenciales del venue.
//
// Orden de resolución: primero la fila de la BD central (multi-tenant), y si
// está vacía se cae a las variables de entorno (despliegue de un solo
// comercio, que es como está 511 hoy). Si no hay nada, error claro — jamás se
// sigue adelante "a ver si suena la flauta" en la ruta del dinero.
func (p *DLocalProcessor) client() (*dlocalGoClient, error) {
	apiKey, secretKey := "", ""
	environment := ""

	if p.config != nil {
		environment = strings.TrimSpace(p.config.Environment)
		if c := p.config.Credentials; c != nil {
			apiKey = strings.TrimSpace(c.DLocalAPIKey)
			secretKey = strings.TrimSpace(c.DLocalSecretKey)
		}
	}

	if apiKey == "" || secretKey == "" {
		envKey := strings.TrimSpace(os.Getenv("DLOCAL_API_KEY"))
		envSecret := strings.TrimSpace(os.Getenv("DLOCAL_SECRET_KEY"))
		if envKey == "" || envSecret == "" {
			return nil, fmt.Errorf("credenciales dLocal no configuradas: faltan access_key/secret_key_encrypted en payment_gateway_credentials (venue %s) y tampoco hay DLOCAL_API_KEY/DLOCAL_SECRET_KEY", p.venueID())
		}
		if apiKey != "" || secretKey != "" {
			log.Printf("[dLocal] AVISO venue=%s tiene credenciales A MEDIAS en la BD — se usan las de entorno", p.venueID())
		}
		apiKey, secretKey = envKey, envSecret
	}

	if environment == "" {
		environment = strings.TrimSpace(os.Getenv("DLOCAL_ENVIRONMENT"))
	}
	if environment == "" && config.App != nil {
		environment = strings.TrimSpace(config.App.Environment)
	}

	// NewDLocalGoClient es fail-safe: cualquier valor que no sea
	// production/live apunta al sandbox, así que un dato raro en la BD NO
	// mueve dinero real.
	return NewDLocalGoClient(apiKey, secretKey, environment), nil
}

func (p *DLocalProcessor) venueID() string {
	if p.config == nil {
		return ""
	}
	return p.config.VenueID
}

// splitCode devuelve el código de colaboración con el que dLocal reparte el fee
// de Pull. Vacío = sin split (todo el dinero a la cuenta que cobra), que es un
// estado válido mientras 511 no tenga su cuenta aprobada.
func (p *DLocalProcessor) splitCode() string {
	if p.config != nil && p.config.Credentials != nil {
		if code := strings.TrimSpace(p.config.Credentials.DLocalSplitCode); code != "" {
			return code
		}
	}
	return strings.TrimSpace(os.Getenv("DLOCAL_SPLIT_CODE"))
}

// defaultCurrency devuelve la moneda del venue, GTQ si no hay nada configurado.
func (p *DLocalProcessor) defaultCurrency() string {
	if p.config != nil && strings.TrimSpace(p.config.DefaultCurrency) != "" {
		return strings.ToUpper(strings.TrimSpace(p.config.DefaultCurrency))
	}
	return "GTQ"
}

// DLocalCountry es el país que se manda a dLocal (código ISO de 2 letras).
// Guatemala por defecto; se puede sobreescribir con DLOCAL_COUNTRY.
func DLocalCountry() string {
	if v := strings.ToUpper(strings.TrimSpace(os.Getenv("DLOCAL_COUNTRY"))); len(v) == 2 {
		return v
	}
	return "GT"
}

// DLocalNotificationURL devuelve la URL absoluta que se manda como
// `notification_url` al crear un pago o un reembolso.
//
// OJO: el grupo /webhooks cuelga de la RAÍZ del backend (NO de /api/v1), así
// que no pasa por el proxy de Cloudflare Pages y tiene que apuntar al backend
// directamente (API_BASE_URL, o DLOCAL_NOTIFICATION_BASE_URL si se quiere
// separar). Devuelve "" si no hay forma de construirla — en ese caso el pago se
// crea igual, pero solo se confirmará cuando el comprador vuelva a la web, y se
// deja un ALERT en los logs.
//
// Vive aquí (y no en controllers) para que la URL que se REGISTRA al crear el
// pago y la que ATIENDE la notificación se construyan con la misma función:
// si divergen, dLocal notifica a un sitio que no existe y los cobros se quedan
// sin confirmar.
func DLocalNotificationURL(venueID string) string {
	base := strings.TrimSpace(os.Getenv("DLOCAL_NOTIFICATION_BASE_URL"))
	if base == "" && config.App != nil {
		base = strings.TrimSpace(config.App.APIBaseURL)
	}
	if base == "" || venueID == "" {
		return ""
	}
	u := strings.TrimRight(base, "/") + "/webhooks/dlocal/" + venueID
	if token := strings.TrimSpace(os.Getenv("DLOCAL_WEBHOOK_TOKEN")); token != "" {
		u += "?t=" + token
	}
	return u
}

// =============================================================================
// CREATE CHECKOUT — crear el pago y devolver a dónde mandar al comprador
// =============================================================================

// CreateCheckout crea el pago en dLocal (POST /v1/payments) y devuelve el
// `redirect_url` del checkout alojado.
//
// IMPORTANTE para quien llame: al volver, el pago está PENDING, no cobrado.
// Guarda `CheckoutResult.SessionID` en `orders.stripe_session_id` — es lo que
// usan el webhook y ConfirmPayment para encontrar la orden.
func (p *DLocalProcessor) CreateCheckout(ctx context.Context, params models.CheckoutParams) (*models.CheckoutResult, error) {
	cli, err := p.client()
	if err != nil {
		return nil, err
	}

	amount := dlocalRound2(params.Amount)
	if amount <= 0 {
		return nil, fmt.Errorf("importe inválido para dLocal: %.2f", params.Amount)
	}

	currency := strings.ToUpper(strings.TrimSpace(params.Currency))
	if currency == "" {
		currency = p.defaultCurrency()
	}

	// order_id: el webhook busca la orden por este valor (por `id` y por
	// `order_number`), así que mandar el UUID de la orden es lo que cierra el
	// círculo si la notificación llega antes que nada más.
	orderID := strings.TrimSpace(params.OrderID)
	if orderID == "" && params.Metadata != nil {
		orderID = strings.TrimSpace(params.Metadata["order_id"])
	}
	if orderID == "" && params.Metadata != nil {
		orderID = strings.TrimSpace(params.Metadata["order_number"])
	}
	if orderID == "" {
		return nil, fmt.Errorf("dLocal necesita order_id para poder conciliar el pago con la orden")
	}

	venueID := strings.TrimSpace(params.VenueID)
	if venueID == "" && params.Metadata != nil {
		venueID = strings.TrimSpace(params.Metadata["venue_id"])
	}
	if venueID == "" {
		venueID = p.venueID()
	}

	// El llamador puede imponer la URL de notificación (p.ej. la que construye
	// controllers.DLocalNotificationURL); si no, se arma aquí.
	notificationURL := ""
	if params.Metadata != nil {
		notificationURL = strings.TrimSpace(params.Metadata["notification_url"])
	}
	if notificationURL == "" {
		notificationURL = DLocalNotificationURL(venueID)
	}
	if notificationURL == "" {
		log.Printf("[dLocal] ALERT order=%s se crea el pago SIN notification_url (falta API_BASE_URL o venue_id) — el cobro solo se confirmará cuando el comprador vuelva a la web",
			orderID)
	}

	description := strings.TrimSpace(params.ProductName)
	if description == "" {
		description = "Entradas"
	}
	// Recorte por RUNAS, no por bytes: "Preventa Ñ" cortado a media letra deja
	// UTF-8 inválido y dLocal rechaza el pago entero.
	description = truncateRunes(description, 100)

	successURL := strings.TrimSpace(params.SuccessURL)
	backURL := strings.TrimSpace(params.CancelURL)
	if successURL == "" && config.App != nil {
		successURL = config.App.FrontendURL
	}
	if backURL == "" {
		backURL = successURL
	}

	req := DLocalPaymentRequest{
		Amount:          amount,
		Currency:        currency,
		Country:         DLocalCountry(),
		OrderID:         orderID,
		Description:     description,
		NotificationURL: notificationURL,
		SuccessURL:      successURL,
		BackURL:         backURL,
		PaymentType:     "CREDIT_CARD",
		SplitCode:       p.splitCode(),
	}
	if params.CustomerEmail != "" || params.CustomerName != "" {
		req.Payer = &DLocalPayer{
			Name:  strings.TrimSpace(params.CustomerName),
			Email: strings.TrimSpace(params.CustomerEmail),
		}
	}
	// SmartFields (checkout transparente). Se pide el mismo pago pero con
	// allow_transparent, que hace que dLocal devuelva merchant_checkout_token
	// para tokenizar la tarjeta en NUESTRA página.
	//
	// En Guatemala esto NO es una preferencia estética: el checkout alojado
	// solo ofrece efectivo (la cobertura de la cuenta no lista tarjeta), y
	// SmartFields es la única vía por la que sale el formulario de tarjeta.
	// Verificado contra producción el 2026-08-13.
	if params.Transparent {
		req.AllowTransparent = true
	}

	payment, err := cli.CreatePayment(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("dLocal: no se pudo crear el pago de la orden %s: %w", orderID, err)
	}

	paymentID := payment.PaymentID()
	if paymentID == "" {
		// Sin id no hay forma de consultar ni de conciliar: mejor fallar aquí
		// que dejar un pago vivo en dLocal que nadie sabe seguir.
		return nil, fmt.Errorf("dLocal creó el pago de la orden %s SIN id — no se puede conciliar", orderID)
	}
	if payment.RedirectURL == "" {
		// El pago EXISTE en dLocal pero no hay a dónde mandar al comprador. Se
		// falla, pero dejando el id escrito: si alguien acabara pagándolo por
		// otra vía, este log es lo único que permite conciliarlo.
		log.Printf("[dLocal] ALERT pago %s creado para la orden %s SIN redirect_url (estado %s) — pago huérfano en dLocal, revisar",
			paymentID, orderID, payment.Status)
		return nil, fmt.Errorf("dLocal no devolvió redirect_url para la orden %s (pago %s, estado %s)", orderID, paymentID, payment.Status)
	}

	log.Printf("[dLocal] checkout creado order=%s pago=%s %.2f %s estado=%s split=%t",
		orderID, paymentID, amount, currency, payment.Status, req.SplitCode != "")

	// SmartFields sin token es un callejón sin salida: el navegador no puede
	// tokenizar y el comprador se queda mirando un formulario muerto. Se falla
	// aquí, dejando el id en el log para poder conciliar si acabara pagándose.
	if params.Transparent && payment.MerchantCheckoutToken == "" {
		log.Printf("[dLocal] ALERT pago %s (orden %s) creado SIN merchant_checkout_token pese a allow_transparent — revisar",
			paymentID, orderID)
		return nil, fmt.Errorf("dLocal no devolvió merchant_checkout_token para la orden %s (pago %s)", orderID, paymentID)
	}

	return &models.CheckoutResult{
		SessionID:             DLocalSessionID(paymentID),
		CheckoutURL:           payment.RedirectURL,
		Gateway:               models.GatewayDLocal,
		PaymentID:             paymentID,
		Status:                payment.Status,
		MerchantCheckoutToken: payment.MerchantCheckoutToken,
	}, nil
}

// SmartFieldsKey devuelve la clave PÚBLICA de SmartFields (la que va al
// navegador para inicializar el SDK). Sale de la fila cifrada del venue y, si
// ahí no está, del entorno. NO es un secreto — la secret key jamás sale del
// backend.
func (p *DLocalProcessor) SmartFieldsKey() string {
	if p != nil && p.config != nil && p.config.Credentials != nil {
		if k := strings.TrimSpace(p.config.Credentials.DLocalSmartFieldsKey); k != "" {
			return k
		}
	}
	return strings.TrimSpace(os.Getenv("DLOCAL_SMARTFIELDS_KEY"))
}

// ConfirmCardToken cierra un pago transparente con el `cardToken` que generó
// SmartFields en el navegador (POST /v1/payments/confirm/{checkout_token}).
//
// Devuelve el pago tal y como queda en dLocal. OJO: que la llamada no falle NO
// significa que se haya cobrado — hay que mirar el estado. Un rechazo del banco
// es una respuesta válida con status REJECTED, no un error.
func (p *DLocalProcessor) ConfirmCardToken(ctx context.Context, checkoutToken string, req DLocalConfirmRequest) (*DLocalPayment, error) {
	cli, err := p.client()
	if err != nil {
		return nil, err
	}
	checkoutToken = strings.TrimSpace(checkoutToken)
	req.CardToken = strings.TrimSpace(req.CardToken)
	if checkoutToken == "" || req.CardToken == "" {
		return nil, fmt.Errorf("dLocal: faltan checkout_token o card_token para confirmar")
	}
	return cli.ConfirmPayment(ctx, checkoutToken, req)
}

// =============================================================================
// CONFIRM PAYMENT — ¿hay dinero de verdad?
// =============================================================================

// ConfirmPayment consulta el estado REAL del pago en dLocal
// (GET /v1/payments/{id}) y lo traduce a models.PaymentResult.
//
//	PAID                          → Success=true  (se pueden emitir tickets)
//	PENDING (o vacío)             → Success=false (aún no hay dinero; NO es un
//	                                fallo: el comprador puede estar pagando)
//	REJECTED/CANCELLED/EXPIRED    → Success=false con el motivo
//
// Solo devuelve error si NO se pudo averiguar el estado (red, credenciales):
// un error significa "no lo sé", nunca "no pagó".
func (p *DLocalProcessor) ConfirmPayment(ctx context.Context, sessionID string) (*models.PaymentResult, error) {
	paymentID := DLocalPaymentIDFromSession(sessionID)
	if paymentID == "" {
		return nil, fmt.Errorf("dLocal: session id vacío")
	}

	payment, err := p.GetDLocalPayment(ctx, paymentID)
	if err != nil {
		return nil, err
	}

	result := &models.PaymentResult{
		TransactionID: payment.PaymentID(),
		Gateway:       models.GatewayDLocal,
		CardLast4:     payment.LastFour,
		CardBrand:     payment.CardBrand,
	}

	switch strings.ToUpper(strings.TrimSpace(payment.Status)) {
	case DLocalStatusPaid:
		result.Success = true
		// dLocal Go no expone código de autorización; el id del pago es la
		// referencia con la que se concilia en su panel.
		result.AuthorizationCode = payment.PaymentID()

	case DLocalStatusPending, "":
		result.ErrorMessage = "El pago todavía no está confirmado (dLocal: PENDING). No se emiten entradas hasta que el pago se complete."

	case DLocalStatusRejected:
		result.ErrorMessage = dlocalFailureMessage("El pago fue rechazado", payment.RejectedReason)

	case DLocalStatusCancelled:
		result.ErrorMessage = dlocalFailureMessage("El pago fue cancelado", payment.RejectedReason)

	case DLocalStatusExpired:
		result.ErrorMessage = dlocalFailureMessage("El enlace de pago caducó", payment.RejectedReason)

	default:
		// Estado que dLocal añadió y nosotros no conocemos: NO se da por bueno.
		log.Printf("[dLocal] ALERT estado desconocido %q en el pago %s — se trata como NO pagado",
			payment.Status, payment.PaymentID())
		result.ErrorMessage = fmt.Sprintf("Estado de pago no reconocido (%s). No se emiten entradas.", payment.Status)
	}

	return result, nil
}

// GetDLocalPayment consulta un pago con las credenciales de ESTE venue. Es lo
// que usa controllers/dlocal_webhook.go (interfaz `dlocalPaymentReader`) para
// verificar contra dLocal el estado que dice una notificación.
func (p *DLocalProcessor) GetDLocalPayment(ctx context.Context, paymentID string) (*DLocalPayment, error) {
	id := DLocalPaymentIDFromSession(paymentID)
	if id == "" {
		return nil, fmt.Errorf("dLocal: payment id vacío")
	}
	cli, err := p.client()
	if err != nil {
		return nil, err
	}
	return cli.GetPayment(ctx, id)
}

func dlocalFailureMessage(base, reason string) string {
	if r := strings.TrimSpace(reason); r != "" {
		return base + " (" + r + ")"
	}
	return base + "."
}

// =============================================================================
// REEMBOLSOS — asíncronos
// =============================================================================

// ProcessRefund pide el reembolso (POST /v1/refunds). amount<=0 = total.
//
// ⚠️ dLocal liquida los reembolsos de forma ASÍNCRONA: la respuesta normal es
// PENDING y la confirmación llega por webhook. Por eso, cuando el reembolso
// queda PENDIENTE, esto devuelve ErrDLocalRefundPending en vez de nil: devolver
// nil sería mentir ("dinero devuelto") sobre algo que aún no ha pasado.
//
// Quien lo llame debe distinguirlo con errors.Is y NO reintentar el reembolso
// —se reembolsaría dos veces—, solo registrarlo y esperar la notificación.
func (p *DLocalProcessor) ProcessRefund(ctx context.Context, transactionID string, amount float64) error {
	_, err := p.RefundPayment(ctx, transactionID, amount)
	return err
}

// RefundPayment es ProcessRefund pero devolviendo también el reembolso creado
// (id y estado), para quien necesite dejar rastro del trámite.
// El error sigue las mismas reglas: ErrDLocalRefundPending = aceptado pero sin
// liquidar todavía.
func (p *DLocalProcessor) RefundPayment(ctx context.Context, transactionID string, amount float64) (*DLocalRefund, error) {
	paymentID := DLocalPaymentIDFromSession(transactionID)
	if paymentID == "" {
		return nil, fmt.Errorf("dLocal: transaction id vacío para el reembolso")
	}
	cli, err := p.client()
	if err != nil {
		return nil, err
	}

	refund, err := cli.Refund(ctx, paymentID, dlocalRound2(amount), DLocalNotificationURL(p.venueID()))
	if err != nil {
		return nil, fmt.Errorf("dLocal: reembolso del pago %s rechazado por la API: %w", paymentID, err)
	}

	refundID := idToString(refund.ID)
	switch strings.ToUpper(strings.TrimSpace(refund.Status)) {
	case "SUCCESS", "PAID", "COMPLETED", "APPROVED":
		log.Printf("[dLocal] reembolso COMPLETADO pago=%s refund=%s importe=%.2f", paymentID, refundID, amount)
		return refund, nil

	case "REJECTED", "CANCELLED", "FAILED":
		return refund, fmt.Errorf("dLocal rechazó el reembolso del pago %s (refund=%s estado=%s)", paymentID, refundID, refund.Status)

	default: // PENDING o vacío
		log.Printf("[dLocal] reembolso PENDIENTE pago=%s refund=%s importe=%.2f — se confirma por webhook, NO reintentar",
			paymentID, refundID, amount)
		return refund, fmt.Errorf("%w (pago=%s refund=%s estado=%s)", ErrDLocalRefundPending, paymentID, refundID, refund.Status)
	}
}

// =============================================================================
// WEBHOOKS
// =============================================================================

// ValidateWebhook: dLocal Go NO firma sus notificaciones, así que no hay firma
// que verificar y esto siempre dice "no validado".
//
// No es un agujero: controllers/dlocal_webhook.go no se fía del body — saca de
// él solo el id del pago y re-consulta el estado real con GET /v1/payments/{id}
// (ver GetDLocalPayment). Un POST falsificado no puede emitir un ticket.
func (p *DLocalProcessor) ValidateWebhook(payload []byte, signature string) (bool, error) {
	return false, fmt.Errorf("dLocal Go no firma las notificaciones: el estado se verifica re-consultando GET /v1/payments/{id}, no con la firma")
}

// =============================================================================
// TARJETA CRUDA — NO SOPORTADO A PROPÓSITO
// =============================================================================
//
// DLocalProcessor NO implementa DirectCardCharger (services/direct_charge.go):
// dLocal Go no acepta el PAN, y fingir que sí lo hace obligaría a que la
// tarjeta pasara por nuestro servidor para acabar fallando igual.
//
// Consecuencia práctica: en controllers.PayOrder el type assertion
// `processor.(services.DirectCardCharger)` falla y el flujo responde con un
// error claro. Los cobros de dLocal van por CreateCheckout → redirect_url.
//
// DLocalRawCardError es el mensaje único para ese caso, para que el motivo
// aparezca igual en cualquier sitio que lo necesite.
func DLocalRawCardError() error { return ErrDLocalNoRawCard }

// =============================================================================
// helpers
// =============================================================================

// dlocalRound2 redondea a 2 decimales: dLocal rechaza importes con más
// precisión de la que admite la moneda, y un céntimo de más descuadra la
// conciliación.
func dlocalRound2(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return math.Round(v*100) / 100
}

// truncateRunes recorta a n caracteres (no bytes), para no partir un carácter
// acentuado por la mitad y mandar UTF-8 roto.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
