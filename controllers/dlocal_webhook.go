package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"pull-api-v2/models"
	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// =============================================================================
// WEBHOOK dLOCAL GO + CONSULTA DE ESTADO DESDE LA WEB
//
// Con el CHECKOUT ALOJADO el comprador paga FUERA de nuestra web: creamos el
// pago (nace PENDING), lo mandamos a la página de dLocal y volvemos. El dinero
// se confirma DESPUÉS, por dos caminos:
//
//   1. dLocal llama a `notification_url` → POST /webhooks/dlocal/:venue_id
//   2. El comprador vuelve a nuestra web → GET /api/v1/orders/:code/payment-status
//      (reconciliación: si el webhook se perdió, aquí se recupera solo)
//
// REGLAS DE ORO de este fichero:
//
//   - NO nos fiamos del body de la notificación. Sea lo que sea que llegue, el
//     estado real se re-consulta con GET /v1/payments/{id} contra dLocal. Un
//     POST falsificado no puede emitir un ticket.
//   - NO se duplica la emisión de tickets: se delega en ConfirmPayment
//     (controllers/order_controller.go), el mismo carril que ya emite tickets,
//     manda el email con PDF+QR y notifica al staff.
//   - IDEMPOTENCIA: dLocal reintenta. La garantía real es el flip atómico
//     processing→confirmed que hace ConfirmPayment; aquí, además, se reclama
//     el estado con UPDATE condicional antes de tocar nada.
//   - El dinero manda: cualquier caso raro (pagado de menos, pagado sobre una
//     orden caducada, pagado sin aprobación) NO emite tickets a ciegas —
//     deja ALERT en logs y rastro en metadata para resolverlo a mano.
//
// Diseño: DLOCAL-MIGRACION.md y DLOCAL-FLUJO-PRIVADO.md (raíz del workspace).
// =============================================================================

// El `stripe_session_id` de una orden pagada con dLocal es
// "dlocal_<payment_id>" — se reutiliza esa columna (ya la usan Stripe y NeoNet)
// para no tocar el esquema, y es la clave con la que ConfirmPayment localiza la
// orden. El formato vive en UN solo sitio, services.DLocalSessionID /
// services.DLocalPaymentIDFromSession: quien crea el pago y quien lo confirma
// TIENEN que construirlo igual o la orden no se encuentra al cobrar.

// =============================================================================
// 1. PAGOS YA VERIFICADOS (puente con el carril compartido)
// =============================================================================

// verifiedPayment es un resultado ya confirmado contra la pasarela.
type verifiedPayment struct {
	result *models.PaymentResult
	at     time.Time
}

// verifiedPayments guarda pagos cuyo estado YA se verificó contra dLocal, para
// que ConfirmPayment no tenga que volver a preguntar (ni dependa de qué
// procesador esté configurado). Mismo patrón que `neonetVerified`: en memoria y
// por proceso, porque quien lo registra llama a ConfirmPayment acto seguido, en
// la misma request. Con TTL para que un fallo intermedio no lo deje creciendo.
var verifiedPayments sync.Map // sessionID -> verifiedPayment

const verifiedPaymentTTL = 15 * time.Minute

func registerVerifiedPayment(sessionID string, result *models.PaymentResult) {
	now := time.Now()
	// Poda oportunista: entradas que nadie consumió (ConfirmPayment abortó
	// antes de llegar a ellas) no se quedan para siempre.
	verifiedPayments.Range(func(k, v interface{}) bool {
		if vp, ok := v.(verifiedPayment); ok && now.Sub(vp.at) > verifiedPaymentTTL {
			verifiedPayments.Delete(k)
		}
		return true
	})
	verifiedPayments.Store(sessionID, verifiedPayment{result: result, at: now})
}

// takeVerifiedPayment lo consume ConfirmPayment. Devuelve (nil,false) si no hay
// nada registrado o si caducó — en ese caso se sigue por el procesador normal.
func takeVerifiedPayment(sessionID string) (*models.PaymentResult, bool) {
	v, ok := verifiedPayments.LoadAndDelete(sessionID)
	if !ok {
		return nil, false
	}
	vp, ok := v.(verifiedPayment)
	if !ok || vp.result == nil || time.Since(vp.at) > verifiedPaymentTTL {
		return nil, false
	}
	return vp.result, true
}

func dropVerifiedPayment(sessionID string) { verifiedPayments.Delete(sessionID) }

// =============================================================================
// 2. CLIENTE dLOCAL — de dónde salen las credenciales
// =============================================================================

// dlocalPaymentReader es lo ÚNICO que este fichero necesita del procesador de
// dLocal: volver a consultar un pago. Si `services/dlocal_processor_impl.go`
// implementa este método (credenciales por venue desde la BD central), se usa;
// si no, se cae a las credenciales de entorno. Ver "contrato" del handoff.
type dlocalPaymentReader interface {
	GetDLocalPayment(ctx context.Context, paymentID string) (*services.DLocalPayment, error)
}

// dlocalGetPayment consulta el estado REAL de un pago (GET /v1/payments/{id}).
// Este es el ancla anti-suplantación: nada de lo que llegue por el webhook se
// da por bueno sin pasar por aquí.
func dlocalGetPayment(ctx context.Context, venueID, paymentID string) (*services.DLocalPayment, error) {
	if paymentID == "" {
		return nil, fmt.Errorf("payment id vacío")
	}

	// 1) Credenciales por venue (multi-tenant), si el procesador las expone.
	if services.Payments != nil && venueID != "" {
		if proc, err := services.Payments.GetProcessor(ctx, venueID); err == nil {
			if reader, ok := proc.(dlocalPaymentReader); ok {
				return reader.GetDLocalPayment(ctx, paymentID)
			}
		}
	}

	// 2) Fallback: credenciales de entorno (despliegue de un solo comercio).
	apiKey := strings.TrimSpace(os.Getenv("DLOCAL_API_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("DLOCAL_SECRET_KEY"))
	if apiKey == "" || secretKey == "" {
		return nil, fmt.Errorf("credenciales dLocal no configuradas (DLOCAL_API_KEY/DLOCAL_SECRET_KEY)")
	}
	env := strings.TrimSpace(os.Getenv("DLOCAL_ENVIRONMENT"))
	if env == "" {
		env = strings.TrimSpace(os.Getenv("ENVIRONMENT"))
	}
	// NewDLocalGoClient es fail-safe: si `env` no es production/live usa el
	// sandbox, así que una variable mal puesta NUNCA consulta dinero real.
	client := services.NewDLocalGoClient(apiKey, secretKey, env)
	return client.GetPayment(ctx, paymentID)
}

// La URL que hay que registrar como `notification_url` al crear el pago la
// construye services.DLocalNotificationURL(venueID) — vive allí porque el
// procesador la necesita y `services` no puede importar `controllers` (ciclo).
// Formato: <API_BASE_URL>/webhooks/dlocal/<venue_id>[?t=<DLOCAL_WEBHOOK_TOKEN>].

// =============================================================================
// 3. EL WEBHOOK
// =============================================================================

// HandleDLocalWebhook recibe las notificaciones de dLocal Go.
// POST /webhooks/dlocal/:venue_id
//
// dLocal Go no firma las notificaciones, así que el body es solo una PISTA: de
// él únicamente se saca el id del pago, y el estado se re-consulta contra la
// API. Responde 200 siempre que la notificación quede procesada o
// conscientemente ignorada; 5xx solo cuando queremos que dLocal reintente.
func HandleDLocalWebhook(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	venueID := c.Param("venue_id")
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id is required"})
		return
	}

	// Defensa en profundidad opcional: si DLOCAL_WEBHOOK_TOKEN está seteado, la
	// URL registrada en dLocal lleva ?t=<token>. Apagado por defecto.
	if want := strings.TrimSpace(os.Getenv("DLOCAL_WEBHOOK_TOKEN")); want != "" {
		if c.Query("t") != want {
			log.Printf("[dLocalWebhook] SECURITY token inválido venue=%s ip=%s", venueID, c.ClientIP())
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
	}

	raw, _ := c.GetRawData()
	var body map[string]interface{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			log.Printf("[dLocalWebhook] body no-JSON venue=%s: %v", venueID, err)
		}
	}

	// Las notificaciones de REEMBOLSO también llegan aquí (mismo
	// notification_url). No tocan la orden: un reembolso no des-emite tickets.
	if isDLocalRefundNotification(body) {
		log.Printf("[dLocalWebhook] ALERT notificación de REEMBOLSO venue=%s payload=%s — revisar la orden a mano (los tickets NO se anulan solos)",
			venueID, truncateForLog(string(raw), 300))
		logDLocalWebhook(venueID, "refund", dlocalPaymentIDFromBody(body), "ignored")
		c.JSON(http.StatusOK, gin.H{"received": true, "ignored": "refund notification"})
		return
	}

	paymentID := dlocalPaymentIDFromBody(body)
	if paymentID == "" {
		// Algunas integraciones mandan el id por query en vez de en el body.
		paymentID = firstNonEmpty(c.Query("payment_id"), c.Query("id"))
	}
	if paymentID == "" {
		log.Printf("[dLocalWebhook] ALERT notificación SIN id de pago venue=%s payload=%s",
			venueID, truncateForLog(string(raw), 300))
		// 200: reintentar no va a añadir el id que falta.
		c.JSON(http.StatusOK, gin.H{"received": true, "ignored": "missing payment id"})
		return
	}

	// ---- ANTI-SUPLANTACIÓN: el estado lo dice dLocal, no el body ----
	payment, err := dlocalGetPayment(ctx, venueID, paymentID)
	if err != nil {
		log.Printf("[dLocalWebhook] ALERT no se pudo verificar el pago %s venue=%s: %v — se pide reintento",
			paymentID, venueID, err)
		// 502: que dLocal reintente. Si el fallo es nuestro (credenciales,
		// red), el reintento nos salva; si no, queda el ALERT.
		c.JSON(http.StatusBadGateway, gin.H{"error": "cannot verify payment"})
		return
	}

	outcome := processDLocalPayment(ctx, venueID, payment)
	logDLocalWebhook(venueID, "payment", paymentID, outcome)
	log.Printf("[dLocalWebhook] venue=%s payment=%s status=%s → %s",
		venueID, paymentID, payment.Status, outcome)

	c.JSON(http.StatusOK, gin.H{
		"received": true,
		"payment":  paymentID,
		"status":   payment.Status,
		"outcome":  outcome,
	})
}

// isDLocalRefundNotification detecta notificaciones de reembolso, que traen su
// propio id + el `payment_id` del pago original. Confundirlas con un pago sería
// grave: GET /v1/payments/{id} del pago reembolsado sigue diciendo PAID.
func isDLocalRefundNotification(body map[string]interface{}) bool {
	if body == nil {
		return false
	}
	if strings.EqualFold(services.GetString(body, "type"), "REFUND") ||
		strings.EqualFold(services.GetString(body, "notification_type"), "REFUND") {
		return true
	}
	if _, ok := body["refund_id"]; ok {
		return true
	}
	// {"id": <id del reembolso>, "payment_id": <id del pago>} → es un reembolso.
	_, hasID := body["id"]
	_, hasPaymentID := body["payment_id"]
	return hasID && hasPaymentID
}

// dlocalPaymentIDFromBody saca el id del pago tolerando las variantes de shape
// que puede mandar dLocal ({id}, {payment_id}, {data:{id}}, {payment:{id}}).
func dlocalPaymentIDFromBody(body map[string]interface{}) string {
	if body == nil {
		return ""
	}
	for _, key := range []string{"payment_id", "id"} {
		if v, ok := body[key]; ok {
			if s := dlocalIDToString(v); s != "" {
				return s
			}
		}
	}
	for _, nest := range []string{"data", "payment", "resource"} {
		if inner, ok := body[nest].(map[string]interface{}); ok {
			for _, key := range []string{"payment_id", "id"} {
				if v, ok := inner[key]; ok {
					if s := dlocalIDToString(v); s != "" {
						return s
					}
				}
			}
		}
	}
	return ""
}

// dlocalIDToString normaliza un id que puede venir como número o cadena, y lo
// sanea: solo alfanumérico/-/_ para que no se pueda inyectar en la URL de la
// API ni en un filtro de PostgREST.
func dlocalIDToString(v interface{}) string {
	var s string
	switch t := v.(type) {
	case string:
		s = strings.TrimSpace(t)
	case float64:
		s = fmt.Sprintf("%.0f", t)
	case json.Number:
		s = t.String()
	default:
		return ""
	}
	if s == "" || len(s) > 64 {
		return ""
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return ""
		}
	}
	return s
}

// logDLocalWebhook deja rastro en la tabla central `webhook_logs`, igual que
// hacen los webhooks de Stripe y NeoNet. Fire-and-forget.
func logDLocalWebhook(venueID, eventType, paymentID, outcome string) {
	services.RunBackground("dlocal-webhook-log", func(bgCtx context.Context) error {
		central := services.DB.Central()
		if central == nil {
			return nil
		}
		_, err := central.InsertCtx(bgCtx, "webhook_logs", map[string]interface{}{
			"gateway":        "dlocal",
			"venue_id":       venueID,
			"endpoint":       "/webhooks/dlocal/" + venueID,
			"method":         "POST",
			"event_type":     eventType + ":" + outcome,
			"transaction_id": paymentID,
			"validated":      true,
			"processed":      outcome == "confirmed" || outcome == "already_confirmed",
		})
		return err
	})
}

// =============================================================================
// 4. EL NÚCLEO: llevar la orden a donde diga el pago
// =============================================================================

// Resultados posibles de procesar un pago. Se devuelven al webhook (para el
// log) y al endpoint de estado (para responder a la web).
const (
	dlOutcomeConfirmed        = "confirmed"         // tickets emitidos AHORA
	dlOutcomeAlreadyConfirmed = "already_confirmed" // reintento: ya estaban
	dlOutcomeRace             = "in_progress"       // otro request lo está haciendo
	dlOutcomePending          = "pending"           // el pago aún no está resuelto
	dlOutcomeRetryable        = "retryable"         // fallo transitorio: volver a intentar
	dlOutcomeFailed           = "payment_failed"    // rechazado/cancelado/caducado
	dlOutcomeOrderNotFound    = "order_not_found"
	dlOutcomeNeedsReview      = "needs_review" // caso raro: intervención manual
)

// processDLocalPayment localiza la orden del pago y la mueve a donde toque.
// Es idempotente: todas las transiciones son claims atómicos.
func processDLocalPayment(ctx context.Context, venueID string, payment *services.DLocalPayment) string {
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		log.Printf("[dLocal] ALERT venue desconocido %s para el pago %s", venueID, payment.PaymentID())
		return dlOutcomeOrderNotFound
	}
	order := findOrderForDLocalPayment(ctx, venueDB, payment)
	if order == nil {
		log.Printf("[dLocal] ALERT pago %s (order_id=%s status=%s) SIN orden en venue %s — cobro sin entrada, revisar a mano",
			payment.PaymentID(), payment.OrderID, payment.Status, venueID)
		return dlOutcomeOrderNotFound
	}
	return applyDLocalPaymentToOrder(ctx, venueID, venueDB, order, payment)
}

// findOrderForDLocalPayment busca la orden por el `order_id` que le pusimos al
// pago (id o número de orden) y, si no aparece, por la sesión guardada.
func findOrderForDLocalPayment(ctx context.Context, venueDB *services.SupabaseClient, payment *services.DLocalPayment) map[string]interface{} {
	const cols = "id,order_number,event_id,ticket_type_id,quantity,total,currency,status,user_name,user_email,metadata,stripe_session_id,paid_at"

	lookup := func(column, value string) map[string]interface{} {
		if value == "" || !safeLookupCode(value) {
			return nil
		}
		row, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
			"select": cols,
			"where":  map[string]interface{}{column: value},
		})
		if err != nil {
			return nil
		}
		return row
	}

	if o := lookup("id", payment.OrderID); o != nil {
		return o
	}
	if o := lookup("order_number", payment.OrderID); o != nil {
		return o
	}
	if o := lookup("stripe_session_id", services.DLocalSessionID(payment.PaymentID())); o != nil {
		return o
	}
	return lookup("stripe_session_id", payment.PaymentID())
}

// applyDLocalPaymentToOrder es la máquina de estados: PAID emite tickets;
// REJECTED/CANCELLED devuelven la orden a pagable (el comprador puede
// reintentar); EXPIRED la cierra y libera el aforo.
func applyDLocalPaymentToOrder(ctx context.Context, venueID string, venueDB *services.SupabaseClient, order map[string]interface{}, payment *services.DLocalPayment) string {
	switch payment.Status {
	case services.DLocalStatusPaid:
		return confirmDLocalPaidOrder(ctx, venueID, venueDB, order, payment)
	case services.DLocalStatusRejected, services.DLocalStatusCancelled:
		return releaseDLocalAttempt(ctx, venueDB, order, payment, false)
	case services.DLocalStatusExpired:
		return releaseDLocalAttempt(ctx, venueDB, order, payment, true)
	case services.DLocalStatusPending, "":
		// Aún no hay resolución. No se toca la orden.
		return dlOutcomePending
	default:
		log.Printf("[dLocal] ALERT estado desconocido %q pago=%s order=%s — no se toca nada",
			payment.Status, payment.PaymentID(), services.GetString(order, "order_number"))
		return dlOutcomeNeedsReview
	}
}

// dlocalPayableStatuses: estados desde los que una orden puede pasar a
// confirmada. `awaiting_approval` NO está: en el flujo privado nadie debería
// poder pagar antes de que el staff apruebe.
var dlocalPayableStatuses = []string{"pending", "processing", "approved_unpaid"}

// confirmDLocalPaidOrder emite los tickets de una orden PAGADA, reutilizando
// ConfirmPayment (tickets + email con PDF/QR + push). No duplica nada de eso.
func confirmDLocalPaidOrder(ctx context.Context, venueID string, venueDB *services.SupabaseClient, order map[string]interface{}, payment *services.DLocalPayment) string {
	orderID := services.GetString(order, "id")
	orderNumber := services.GetString(order, "order_number")
	status := services.GetString(order, "status")

	// Idempotencia barata: reintento de un webhook ya procesado.
	if status == "confirmed" {
		return dlOutcomeAlreadyConfirmed
	}

	// El dinero tiene que cuadrar. Un pago por MENOS del total (importe
	// manipulado al crear el pago, o moneda distinta) no emite tickets.
	total := services.GetFloat64(order, "total")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	if payment.Amount > 0 && payment.Amount < total-0.01 {
		log.Printf("[dLocal] ALERT IMPORTE INSUFICIENTE order=%s pagado=%.2f %s total=%.2f %s pago=%s — NO se emiten tickets",
			orderNumber, payment.Amount, payment.Currency, total, currency, payment.PaymentID())
		stampDLocalMetadata(ctx, venueDB, order, map[string]interface{}{
			"payment_id":  payment.PaymentID(),
			"status":      payment.Status,
			"amount_paid": payment.Amount,
			"review":      "underpaid",
			"reviewed_at": time.Now().Format(time.RFC3339),
		})
		return dlOutcomeNeedsReview
	}
	if payment.Currency != "" && !strings.EqualFold(payment.Currency, currency) {
		log.Printf("[dLocal] ALERT MONEDA DISTINTA order=%s pagado en %s, orden en %s pago=%s — NO se emiten tickets",
			orderNumber, payment.Currency, currency, payment.PaymentID())
		stampDLocalMetadata(ctx, venueDB, order, map[string]interface{}{
			"payment_id":  payment.PaymentID(),
			"status":      payment.Status,
			"review":      "currency_mismatch",
			"reviewed_at": time.Now().Format(time.RFC3339),
		})
		return dlOutcomeNeedsReview
	}

	// Orden en un estado que NO admite pago (caducada, cancelada, o privada sin
	// aprobar). El dinero ya entró: NO emitir tickets a ciegas (el aforo pudo
	// liberarse y provocaría sobreventa), pero dejarlo escrito para resolverlo.
	if !containsString(dlocalPayableStatuses, status) {
		log.Printf("[dLocal] ALERT PAGO SOBRE ORDEN NO PAGABLE order=%s status=%s pago=%s %.2f %s — revisar (reembolso o emisión manual)",
			orderNumber, status, payment.PaymentID(), payment.Amount, payment.Currency)
		stampDLocalMetadata(ctx, venueDB, order, map[string]interface{}{
			"payment_id":   payment.PaymentID(),
			"status":       payment.Status,
			"amount_paid":  payment.Amount,
			"review":       "paid_on_" + status,
			"order_status": status,
			"reviewed_at":  time.Now().Format(time.RFC3339),
		})
		return dlOutcomeNeedsReview
	}

	sessionID := services.DLocalSessionID(payment.PaymentID())

	// CLAIM ATÓMICO: pagable → processing, y de paso se graba la sesión con la
	// que ConfirmPayment encontrará la orden. `where` sin operadores: se prueba
	// estado por estado (UpdateCtx solo sabe hacer igualdad).
	claimed := false
	for _, from := range dlocalPayableStatuses {
		res, err := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
			"status":            "processing",
			"stripe_session_id": sessionID,
		}, map[string]interface{}{"id": orderID, "status": from})
		if err == nil && len(res) > 0 {
			claimed = true
			break
		}
	}
	if !claimed {
		// Otro request (webhook reintentado, o el retorno del comprador) ganó.
		cur, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
			"select": "status", "where": map[string]interface{}{"id": orderID},
		})
		if services.GetString(cur, "status") == "confirmed" {
			return dlOutcomeAlreadyConfirmed
		}
		log.Printf("[dLocal] claim perdido order=%s (status=%s) — otro proceso lo está confirmando",
			orderNumber, services.GetString(cur, "status"))
		return dlOutcomeRace
	}

	// El estado real YA está verificado contra dLocal: se lo damos hecho al
	// carril compartido para que no vuelva a preguntar a la pasarela.
	registerVerifiedPayment(sessionID, &models.PaymentResult{
		Success:       true,
		TransactionID: payment.PaymentID(),
		// models.GatewayDLocal lo añade otro agente en models/payment.go; aquí
		// se construye por valor para no depender del orden de los merges.
		Gateway:   models.PaymentGateway("dlocal"),
		CardLast4: payment.LastFour,
		CardBrand: payment.CardBrand,
	})
	defer dropVerifiedPayment(sessionID)

	code := runSharedConfirmRail(ctx, venueID, sessionID)
	switch {
	case code == http.StatusOK:
		// Rastro del cobro (en UPDATE aparte: `payment_gateway` es un enum y en
		// una BD sin la migración fase0 el valor 'dlocal' tumbaría el UPDATE
		// entero — jamás en el mismo que emite los tickets).
		stampDLocalMetadata(ctx, venueDB, order, map[string]interface{}{
			"payment_id":  payment.PaymentID(),
			"status":      payment.Status,
			"amount_paid": payment.Amount,
			"currency":    payment.Currency,
			"last_four":   payment.LastFour,
			"paid_at":     time.Now().Format(time.RFC3339),
		})
		markOrderGatewayDLocal(ctx, venueDB, orderID, orderNumber)
		log.Printf("[dLocal] CONFIRMADA order=%s pago=%s %.2f %s — tickets emitidos",
			orderNumber, payment.PaymentID(), payment.Amount, payment.Currency)
		return dlOutcomeConfirmed
	case code == http.StatusConflict:
		return dlOutcomeRace
	default:
		// ConfirmPayment ya devolvió la orden a 'processing'; el reintento del
		// webhook (o el retorno del comprador) volverá a pasar por aquí.
		log.Printf("[dLocal] ALERT COBRADO PERO SIN TICKETS order=%s pago=%s (ConfirmPayment HTTP %d) — se reintentará; si persiste, emitir a mano",
			orderNumber, payment.PaymentID(), code)
		return dlOutcomeRetryable
	}
}

// runSharedConfirmRail invoca ConfirmPayment fuera de una request HTTP real: la
// respuesta se descarta y solo se mira el código, para que cada llamador
// (webhook o endpoint de estado) escriba SU propia respuesta.
func runSharedConfirmRail(ctx context.Context, venueID, sessionID string) int {
	rec := &discardResponseWriter{header: make(http.Header), status: http.StatusOK}
	inner, _ := gin.CreateTestContext(rec)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/internal/dlocal-confirm", nil)
	if err != nil {
		return http.StatusInternalServerError
	}
	q := req.URL.Query()
	q.Set("session_id", sessionID)
	q.Set("venue_id", venueID)
	req.URL.RawQuery = q.Encode()
	inner.Request = req

	ConfirmPayment(inner)
	return rec.status
}

// discardResponseWriter recoge el código de estado de ConfirmPayment y tira el
// cuerpo: esa respuesta era para el navegador del comprador, no para dLocal.
type discardResponseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
}

func (w *discardResponseWriter) Header() http.Header { return w.header }

func (w *discardResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return len(b), nil
}

func (w *discardResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
}

// releaseDLocalAttempt trata un intento de pago que NO cuajó.
//
//	terminal=false (REJECTED / CANCELLED): el comprador puede reintentar con
//	  otra tarjeta, así que la orden vuelve a estado pagable y el aforo SIGUE
//	  reservado (lo liberan los jobs de caducidad si nunca vuelve). Matar la
//	  orden en la primera tarjeta declinada le quitaría el sitio a alguien que
//	  solo tecleó mal el CVV.
//	terminal=true (EXPIRED) o demasiados rechazos: se cierra la orden y se
//	  libera el aforo con la RPC `release_ticket_type`.
func releaseDLocalAttempt(ctx context.Context, venueDB *services.SupabaseClient, order map[string]interface{}, payment *services.DLocalPayment, terminal bool) string {
	orderID := services.GetString(order, "id")
	orderNumber := services.GetString(order, "order_number")
	status := services.GetString(order, "status")

	if status == "confirmed" {
		// Pagó por otra vía (o un intento anterior sí cuajó). No tocar.
		log.Printf("[dLocal] intento %s sobre orden YA confirmada order=%s pago=%s — ignorado",
			payment.Status, orderNumber, payment.PaymentID())
		return dlOutcomeAlreadyConfirmed
	}

	// IDEMPOTENCIA: dLocal reintenta la MISMA notificación. Sin esto, cinco
	// reintentos de un único CVV mal tecleado agotarían el contador de intentos
	// y cerrarían la orden de un comprador que solo falló una vez.
	if dlocalAlreadySeen(order, payment) {
		log.Printf("[dLocal] notificación repetida (%s) order=%s pago=%s — ignorada",
			payment.Status, orderNumber, payment.PaymentID())
		return dlOutcomeFailed
	}

	rejections := dlocalRejectionCount(order) + 1
	giveUp := terminal || rejections >= maxAttemptsPerOrder

	if !giveUp {
		// Devolver la orden a pagable SOLO si la habíamos puesto en processing.
		resume := resumableStatusFor(order)
		if _, err := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
			"status": resume,
		}, map[string]interface{}{"id": orderID, "status": "processing"}); err != nil {
			log.Printf("[dLocal] no se pudo devolver a %s order=%s: %v", resume, orderNumber, err)
		}
		stampDLocalMetadata(ctx, venueDB, order, map[string]interface{}{
			"payment_id":      payment.PaymentID(),
			"status":          payment.Status,
			"rejected_reason": payment.RejectedReason,
			"rejections":      rejections,
			"last_attempt_at": time.Now().Format(time.RFC3339),
		})
		log.Printf("[dLocal] intento %s order=%s pago=%s motivo=%q (intentos=%d) — la orden sigue pagable",
			payment.Status, orderNumber, payment.PaymentID(), payment.RejectedReason, rejections)
		return dlOutcomeFailed
	}

	// Cierre definitivo. `expired` y `failed` existen en el enum en los tres
	// entornos; se evita a propósito `payment_failed` (solo tras fase0).
	finalStatus := "failed"
	reason := fmt.Sprintf("Pago dLocal %s (%s)", payment.Status, payment.RejectedReason)
	if payment.Status == services.DLocalStatusExpired {
		finalStatus = "expired"
		reason = "Enlace de pago caducado en dLocal"
	}

	// CLAIM ATÓMICO: solo quien gana el flip libera el aforo — así un webhook
	// reintentado no devuelve la plaza dos veces.
	claimed := false
	for _, from := range dlocalPayableStatuses {
		res, err := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
			"status":              finalStatus,
			"cancelled_at":        time.Now().Format(time.RFC3339),
			"cancellation_reason": reason,
		}, map[string]interface{}{"id": orderID, "status": from})
		if err == nil && len(res) > 0 {
			claimed = true
			break
		}
	}
	if !claimed {
		log.Printf("[dLocal] cierre no aplicado order=%s (status actual=%s) — ya lo procesó otro",
			orderNumber, status)
		return dlOutcomeFailed
	}

	releaseOrderCapacity(ctx, venueDB, order)
	stampDLocalMetadata(ctx, venueDB, order, map[string]interface{}{
		"payment_id":      payment.PaymentID(),
		"status":          payment.Status,
		"rejected_reason": payment.RejectedReason,
		"rejections":      rejections,
		"closed_at":       time.Now().Format(time.RFC3339),
	})
	log.Printf("[dLocal] CERRADA order=%s pago=%s status=%s → %s (aforo liberado)",
		orderNumber, payment.PaymentID(), payment.Status, finalStatus)
	return dlOutcomeFailed
}

// resumableStatusFor decide a qué estado pagable vuelve una orden cuyo intento
// de pago falló: `approved_unpaid` si es una solicitud privada aprobada con
// plazo aún vigente, `pending` en cualquier otro caso.
func resumableStatusFor(order map[string]interface{}) string {
	metadata, _ := order["metadata"].(map[string]interface{})
	if metadata == nil {
		return "pending"
	}
	deadline := services.GetString(metadata, "payment_deadline")
	if deadline == "" {
		return "pending"
	}
	if t, ok := parseFlexTime(deadline); ok && time.Now().Before(t) {
		return "approved_unpaid"
	}
	return "pending"
}

// dlocalOrderTrace devuelve el rastro que dejó el último pago sobre la orden.
func dlocalOrderTrace(order map[string]interface{}) map[string]interface{} {
	metadata, _ := order["metadata"].(map[string]interface{})
	if metadata == nil {
		return nil
	}
	dl, _ := metadata["dlocal"].(map[string]interface{})
	return dl
}

// dlocalRejectionCount lee cuántos intentos fallidos lleva la orden.
func dlocalRejectionCount(order map[string]interface{}) int {
	dl := dlocalOrderTrace(order)
	if dl == nil {
		return 0
	}
	return services.GetInt(dl, "rejections")
}

// dlocalAlreadySeen indica si este MISMO pago, en este MISMO estado, ya se
// procesó sobre esta orden (reintento de la notificación).
func dlocalAlreadySeen(order map[string]interface{}, payment *services.DLocalPayment) bool {
	dl := dlocalOrderTrace(order)
	if dl == nil {
		return false
	}
	return services.GetString(dl, "payment_id") == payment.PaymentID() &&
		services.GetString(dl, "status") == payment.Status
}

// stampDLocalMetadata deja el rastro del pago en `orders.metadata.dlocal`.
// Best-effort: nunca debe tumbar el camino del dinero.
func stampDLocalMetadata(ctx context.Context, venueDB *services.SupabaseClient, order map[string]interface{}, fields map[string]interface{}) {
	orderID := services.GetString(order, "id")
	if orderID == "" || venueDB == nil {
		return
	}
	metadata, _ := order["metadata"].(map[string]interface{})
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	dl, _ := metadata["dlocal"].(map[string]interface{})
	if dl == nil {
		dl = map[string]interface{}{}
	}
	for k, v := range fields {
		dl[k] = v
	}
	metadata["dlocal"] = dl
	order["metadata"] = metadata
	if err := venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
		"metadata": metadata,
	}, map[string]interface{}{"id": orderID}); err != nil {
		log.Printf("[dLocal] no se pudo guardar metadata order=%s: %v",
			services.GetString(order, "order_number"), err)
	}
}

// markOrderGatewayDLocal marca la pasarela en la orden. Va SOLO, y después de
// emitir los tickets: `payment_gateway` es un enum y si la BD todavía no tiene
// el valor 'dlocal' (migración sql/dlocal_fase0.sql) el UPDATE falla con 22P02.
// Fallar aquí es cosmético; fallar en el UPDATE que confirma, no.
func markOrderGatewayDLocal(ctx context.Context, venueDB *services.SupabaseClient, orderID, orderNumber string) {
	if err := venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
		"payment_gateway": "dlocal",
	}, map[string]interface{}{"id": orderID}); err != nil {
		log.Printf("[dLocal] no se pudo marcar payment_gateway=dlocal order=%s: %v (¿falta sql/dlocal_fase0.sql?)",
			orderNumber, err)
	}
}

// =============================================================================
// 5. ESTADO DE LA ORDEN PARA LA WEB (vuelta del checkout)
// =============================================================================

// GetOrderPaymentStatus dice si una orden quedó pagada. Es lo que consulta la
// web cuando el comprador vuelve del checkout alojado de dLocal.
//
// GET /api/v1/orders/:code/payment-status?venue_id=<uuid>&payment_link_code=<code>
//
// Con `payment_link_code` correcto además RECONCILIA: consulta el pago en
// dLocal y, si está PAID pero la orden no está confirmada (webhook perdido,
// retrasado o mal configurado), emite los tickets en el acto. Así el comprador
// nunca depende de que la notificación llegue.
func GetOrderPaymentStatus(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	code := c.Param("code")
	if !safeLookupCode(code) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid order code"})
		return
	}

	venueID := c.Query("venue_id")
	if venueID == "" {
		if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id",
			"where":  map[string]interface{}{"is_active": true, "deleted_at": "is.null"},
			"limit":  1,
		}); v != nil {
			venueID = services.GetString(v, "id")
		}
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	const cols = "id,order_number,event_id,ticket_type_id,quantity,total,currency,status,user_name,user_email,metadata,stripe_session_id,paid_at"
	order, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": cols, "where": map[string]interface{}{"id": code},
	})
	if order == nil {
		order, _ = venueDB.QueryOne(ctx, "orders", map[string]interface{}{
			"select": cols, "where": map[string]interface{}{"order_number": code},
		})
	}
	if order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	metadata, _ := order["metadata"].(map[string]interface{})
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	// Igual que /orders/pay: solo quien tiene el código de ESTA orden puede
	// disparar la reconciliación (que habla con la pasarela y emite tickets).
	authorized := matchPaymentLinkCode(metadata, c.Query("payment_link_code"))

	outcome := ""
	if authorized && services.GetString(order, "status") != "confirmed" {
		if paymentID := dlocalPaymentIDForOrder(order); paymentID != "" {
			// Contexto DESPEGADO de la request: la reconciliación puede emitir
			// tickets, y si el comprador cierra la pestaña a mitad, cancelar el
			// contexto dejaría la orden en 'processing' con el dinero cobrado.
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer bgCancel()
			if payment, err := dlocalGetPayment(bgCtx, venueID, paymentID); err != nil {
				log.Printf("[dLocal] estado: no se pudo consultar el pago %s order=%s: %v",
					paymentID, services.GetString(order, "order_number"), err)
			} else {
				outcome = applyDLocalPaymentToOrder(bgCtx, venueID, venueDB, order, payment)
				// Releer: la reconciliación pudo cambiar el estado.
				if fresh, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
					"select": cols, "where": map[string]interface{}{"id": services.GetString(order, "id")},
				}); fresh != nil {
					order = fresh
				}
			}
		}
	}

	status := services.GetString(order, "status")
	paid := status == "confirmed"

	ticketCount := 0
	if paid {
		if n, err := venueDB.CountCtx(ctx, "tickets", map[string]interface{}{
			"order_id": services.GetString(order, "id"),
		}); err == nil {
			ticketCount = n
		}
	}

	resp := gin.H{
		"order_id":     services.GetString(order, "id"),
		"order_number": services.GetString(order, "order_number"),
		"status":       status,
		"paid":         paid,
		"tickets":      ticketCount,
		"total":        services.GetFloat64(order, "total"),
		"currency":     services.GetString(order, "currency"),
		"paid_at":      services.GetString(order, "paid_at"),
		"message":      dlocalStatusMessage(status, outcome),
	}
	if outcome != "" {
		resp["reconciliation"] = outcome
	}
	c.JSON(http.StatusOK, resp)
}

// dlocalPaymentIDForOrder recupera el id del pago de dLocal asociado a la
// orden: primero de metadata, si no de la sesión guardada.
func dlocalPaymentIDForOrder(order map[string]interface{}) string {
	if metadata, ok := order["metadata"].(map[string]interface{}); ok {
		if dl, ok := metadata["dlocal"].(map[string]interface{}); ok {
			if id := services.GetString(dl, "payment_id"); id != "" {
				return id
			}
		}
	}
	session := services.GetString(order, "stripe_session_id")
	if strings.HasPrefix(session, services.DLocalSessionPrefix) {
		return services.DLocalPaymentIDFromSession(session)
	}
	return ""
}

// dlocalStatusMessage traduce el estado a algo que la web pueda enseñar tal cual.
func dlocalStatusMessage(status, outcome string) string {
	switch status {
	case "confirmed":
		return "Pago confirmado. Tus entradas van en camino por correo."
	case "processing":
		return "Estamos confirmando tu pago. No vuelvas a pagar; en unos segundos lo tendrás."
	case "approved_unpaid":
		return "Tu solicitud está aprobada. Completa el pago para recibir tus entradas."
	case "awaiting_approval":
		return "Tu solicitud está pendiente de aprobación. Aún no se te ha cobrado nada."
	case "expired":
		return "El plazo de pago venció. Vuelve a intentarlo desde el evento."
	case "cancelled", "failed", "payment_failed":
		if outcome == dlOutcomeFailed {
			return "El pago no se completó. Puedes intentarlo de nuevo con otra tarjeta."
		}
		return "Esta orden ya no está activa."
	default:
		return "Tu orden está pendiente de pago."
	}
}

// =============================================================================
// helpers
// =============================================================================

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
