package controllers

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"pull-api-v2/models"
	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// =============================================
// DIRECT CARD PAYMENT (NeoNet/Cybersource)
// POST /api/v1/orders/pay
//
// Acordado con NeoNet: cada pago del comprador son DOS transacciones
// atómicas — la parte del venue (precio de las entradas) y el fee de
// servicio de Pull. Si la segunda falla, la primera se reversa y la orden
// queda sin cobrar. Solo con ambas aprobadas se emiten los tickets, por el
// mismo carril compartido de ConfirmPayment.
//
// En DEMO_MODE el MockProcessor implementa el mismo contrato, así que este
// flujo se puede probar end-to-end sin credenciales reales.
// =============================================

type payOrderRequest struct {
	OrderID string `json:"order_id"`
	// Anti-carding: el código que create-pending-order devolvió al crear la
	// orden. Sin él (o sin coincidir) no se toca la pasarela.
	PaymentLinkCode string `json:"payment_link_code"`
	TurnstileToken  string `json:"turnstile_token"`
	VenueID         string `json:"venue_id"`
	VenueSlug       string `json:"venue_slug"`
	Card            struct {
		Number   string `json:"number"`
		ExpMonth string `json:"exp_month"`
		ExpYear  string `json:"exp_year"`
		CVV      string `json:"cvv"`
	} `json:"card"`
	BillTo struct {
		Address1   string `json:"address1"`
		City       string `json:"city"`
		State      string `json:"state"`
		PostalCode string `json:"postal_code"`
		Country    string `json:"country"`
	} `json:"bill_to"`
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// PayOrder charges a pending order: venue share + service fee, atomically.
func PayOrder(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	var req payOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.OrderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id is required"})
		return
	}
	// SECURITY: block PostgREST operator injection via order_id.
	if !safeLookupCode(req.OrderID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid order_id"})
		return
	}

	// Never log or echo card data anywhere in this handler.
	card := services.CybsCard{
		Number:       strings.ReplaceAll(req.Card.Number, " ", ""),
		ExpMonth:     req.Card.ExpMonth,
		ExpYear:      req.Card.ExpYear,
		SecurityCode: req.Card.CVV,
	}
	if len(card.Number) < 12 || card.ExpMonth == "" || card.ExpYear == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Datos de tarjeta incompletos"})
		return
	}
	if len(card.ExpYear) == 2 {
		card.ExpYear = "20" + card.ExpYear
	}

	// Anti-carding: CAPTCHA (si está activado) y límite por tarjeta, antes de
	// gastar queries o tocar la pasarela.
	if err := verifyTurnstile(ctx, req.TurnstileToken, c.ClientIP()); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Verificación de seguridad fallida. Recarga la página."})
		return
	}
	if !allowCardAttempt(cardAttemptKey(card.Number)) {
		log.Printf("[PayOrder] card attempt limit hit cardkey=%s", cardAttemptKey(card.Number))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Demasiados intentos con esta tarjeta. Espera unos minutos."})
		return
	}

	// Resolve venue: explicit id > slug > first active (single-venue deploys).
	venueID := req.VenueID
	if venueID == "" && req.VenueSlug != "" {
		if id, err := resolveVenueIDFromSlug(ctx, req.VenueSlug); err == nil {
			venueID = id
		}
	}
	if venueID == "" {
		if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		}); v != nil {
			venueID = services.GetString(v, "id")
		}
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	order, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,status,total,currency,quantity,ticket_type_id,event_id,user_name,user_email,metadata",
		"where":  map[string]interface{}{"id": req.OrderID},
	})
	if err != nil || order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	// PUBLIC vs PRIVATE: a private event (is_private or require_approval on the
	// event) needs staff approval before charging. For those we AUTHORIZE only
	// (hold the funds) and settle on approval / reverse on rejection. Public
	// events capture immediately.
	needsApproval := false
	if eventID := services.GetString(order, "event_id"); eventID != "" {
		if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "is_private,require_approval",
			"where":  map[string]interface{}{"id": eventID},
		}); ev != nil {
			needsApproval = services.GetBool(ev, "is_private") || services.GetBool(ev, "require_approval")
		}
	}
	status := services.GetString(order, "status")
	if status == "confirmed" {
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Order already confirmed",
			"order_number": services.GetString(order, "order_number"), "order_id": req.OrderID})
		return
	}
	if status != "pending" && status != "processing" && status != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "Order is not payable", "status": status})
		return
	}

	// Anti-carding: solo quien tiene el payment_link_code de ESTA orden puede
	// intentar cobrarla, y cada orden aguanta un número finito de declinadas.
	orderMeta, _ := order["metadata"].(map[string]interface{})
	if orderMeta == nil {
		orderMeta = map[string]interface{}{}
	}
	if !matchPaymentLinkCode(orderMeta, req.PaymentLinkCode) {
		log.Printf("[PayOrder] payment_link_code mismatch order=%s", req.OrderID)
		c.JSON(http.StatusForbidden, gin.H{"error": "Código de pago inválido para esta orden."})
		return
	}
	priorAttempts := services.GetInt(orderMeta, "payment_attempts")
	if priorAttempts >= maxAttemptsPerOrder {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Demasiados intentos de pago para esta orden. Crea una nueva."})
		return
	}
	// recordDeclinedAttempt persiste el contador ANTES de responder al
	// cliente, para que el límite no se pueda esquivar con reintentos rápidos.
	recordDeclinedAttempt := func() {
		priorAttempts++
		orderMeta["payment_attempts"] = priorAttempts
		venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{"metadata": orderMeta},
			map[string]interface{}{"id": req.OrderID})
		log.Printf("[PayOrder] DECLINED order=%s attempts=%d cardkey=%s",
			services.GetString(order, "order_number"), priorAttempts, cardAttemptKey(card.Number))
	}

	total := services.GetFloat64(order, "total")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	if total <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Order total is invalid"})
		return
	}

	// Split: venue share vs Pull's service fee, from the venue's configured
	// fee percent (venues.platform_fee_percent — 8% for 511 Events). The
	// order total was created as subtotal * (1 + fee).
	feePercent := 0.0
	if venue, err := services.DB.GetVenue(ctx, venueID); err == nil && venue.PlatformFeePercent > 0 {
		feePercent = venue.PlatformFeePercent
	}
	venueShare := total
	feeShare := 0.0
	if feePercent > 0 {
		venueShare = round2(total / (1 + feePercent/100))
		feeShare = round2(total - venueShare)
	}

	processor, err := services.Payments.GetProcessor(ctx, venueID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway not configured"})
		return
	}
	charger, ok := processor.(services.DirectCardCharger)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gateway does not support direct card payments"})
		return
	}

	// Billing info: sensible Guatemala defaults when the form doesn't ask.
	userName := services.GetString(order, "user_name")
	firstName, lastName := userName, "."
	if parts := strings.SplitN(userName, " ", 2); len(parts) == 2 {
		firstName, lastName = parts[0], parts[1]
	}
	billTo := services.CybsBillTo{
		FirstName:  firstName,
		LastName:   lastName,
		Email:      services.GetString(order, "user_email"),
		Phone:      "50200000000",
		Address1:   orDefault(req.BillTo.Address1, "Ciudad de Guatemala"),
		Locality:   orDefault(req.BillTo.City, "Guatemala"),
		AdminArea:  orDefault(req.BillTo.State, "GT"),
		PostalCode: orDefault(req.BillTo.PostalCode, "01001"),
		Country:    orDefault(req.BillTo.Country, "GT"),
	}

	orderNumber := services.GetString(order, "order_number")
	// capture=true for public events (charge now); false for private events
	// (hold the funds until staff approves).
	capture := !needsApproval

	// --- Transacción 1: parte del venue ---
	charge1, err := charger.ChargeCard(ctx, services.ChargeParams{
		ReferenceCode: orderNumber + "-VENUE",
		Amount:        venueShare,
		Currency:      currency,
		Card:          card,
		BillTo:        billTo,
		Capture:       capture,
	})
	if err != nil {
		log.Printf("[PayOrder] charge1 error order=%s: %v", orderNumber, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "No se pudo procesar el pago. Intenta de nuevo."})
		return
	}
	// La parte del VENUE debe autorizarse ENTERA: una autorización parcial
	// (prepago sin saldo, o el trigger SDISCOUNT del sandbox) dejaría al
	// local cobrando de menos → se trata como rechazo y se libera lo retenido.
	venuePartial := charge1.Success && charge1.AuthorizedAmount > 0 && charge1.AuthorizedAmount < venueShare-0.005
	if !charge1.Success || venuePartial {
		if charge1.TransactionID != "" {
			rbCtx, rbCancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer rbCancel()
			if revErr := charger.ReverseCharge(rbCtx, charge1.TransactionID, orderNumber+"-VENUE-PARB", venueShare, currency); revErr != nil {
				log.Printf("[PayOrder] ALERT: venue partial/declined reversal failed order=%s tx=%s: %v", orderNumber, charge1.TransactionID, revErr)
			}
		}
		recordDeclinedAttempt()
		msg := charge1.ErrorMessage
		if venuePartial {
			msg = "Tu tarjeta no autorizó el importe completo. Usa otra tarjeta."
		}
		c.JSON(http.StatusPaymentRequired, gin.H{"error": msg, "declined": true})
		return
	}

	// --- Transacción 2: fee de servicio de Pull ---
	var charge2 *services.ChargeResult
	if feeShare > 0 {
		charge2, err = charger.ChargeCard(ctx, services.ChargeParams{
			ReferenceCode: orderNumber + "-FEE",
			Amount:        feeShare,
			Currency:      currency,
			Card:          card,
			BillTo:        billTo,
			Capture:       capture,
		})
		if err != nil || !charge2.Success {
			// Rollback: reversar la primera venta/autorización con un contexto
			// PROPIO (no el de la request, que puede estar casi agotado tras 2
			// llamadas lentas al gateway) para no dejar el cargo sin reversar.
			rbCtx, rbCancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer rbCancel()
			if revErr := charger.ReverseCharge(rbCtx, charge1.TransactionID, orderNumber+"-VENUE-RB", venueShare, currency); revErr != nil {
				log.Printf("[PayOrder] ALERT: fee charge failed AND reversal failed order=%s tx=%s: %v",
					orderNumber, charge1.TransactionID, revErr)
			} else {
				log.Printf("[PayOrder] fee charge failed, venue charge reversed order=%s", orderNumber)
			}
			// Si el fee quedó en autorización PARCIAL, también retiene dinero:
			// liberarlo igual.
			if charge2 != nil && charge2.TransactionID != "" {
				if revErr := charger.ReverseCharge(rbCtx, charge2.TransactionID, orderNumber+"-FEE-PARB", feeShare, currency); revErr != nil {
					log.Printf("[PayOrder] ALERT: fee partial-auth reversal failed order=%s tx=%s: %v", orderNumber, charge2.TransactionID, revErr)
				}
			}
			recordDeclinedAttempt()
			msg := "No se pudo completar el pago. No se ha realizado ningún cargo."
			if charge2 != nil && charge2.ErrorMessage != "" {
				msg = charge2.ErrorMessage
			}
			c.JSON(http.StatusPaymentRequired, gin.H{"error": msg, "declined": true})
			return
		}
	}

	feeTxID := ""
	if charge2 != nil {
		feeTxID = charge2.TransactionID
		// Fee con autorización PARCIAL: se acepta recortado — matar una venta
		// entera por el fee de Pull es peor negocio. Se registra el importe
		// REAL autorizado para que capturas y reversas posteriores usen esa
		// cifra (capturar más de lo autorizado = settlement Failed en el EBC).
		if charge2.AuthorizedAmount > 0 && charge2.AuthorizedAmount < feeShare-0.005 {
			log.Printf("[PayOrder] fee PARTIAL order=%s pedido=%.2f autorizado=%.2f — se captura lo autorizado",
				orderNumber, feeShare, charge2.AuthorizedAmount)
			feeShare = round2(charge2.AuthorizedAmount)
		}
	}

	// Persistir el desglose de las dos transacciones en la orden.
	metadata := orderMeta
	metadata["payment_split"] = map[string]interface{}{
		"venue_amount":      venueShare,
		"fee_amount":        feeShare,
		"fee_percent":       feePercent,
		"venue_transaction": charge1.TransactionID,
		"fee_transaction":   feeTxID,
		"gateway":           string(processor.GetGateway()),
		"captured":          capture, // false = funds held, awaiting approval
	}

	// --- PRIVATE: funds are HELD. Leave the order awaiting staff approval,
	// notify staff, and do NOT issue tickets yet. ---
	if needsApproval {
		// 48h staff-decision deadline; the expiry job auto-reverses after it.
		deadline := time.Now().Add(48 * time.Hour)
		metadata["approval_deadline"] = deadline.Format(time.RFC3339)
		metadata["authorized_at"] = time.Now().Format(time.RFC3339)

		// Ruta de dinero: si el hold no se puede PERSISTIR, la tarjeta quedó
		// autorizada pero la orden no lo sabría (ni staff, ni job de 48h, ni
		// nadie que pueda capturar o reversar). Liberar las autorizaciones y
		// devolver error limpio — el cliente reintenta y no queda dinero
		// retenido huérfano.
		if err := venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
			"status":          "payment_authorized",
			"payment_gateway": "neonet",
			"metadata":        metadata,
		}, map[string]interface{}{"id": req.OrderID}); err != nil {
			log.Printf("[PayOrder] ALERT: hold persist FAILED order=%s venueTx=%s feeTx=%s: %v — reversing",
				orderNumber, charge1.TransactionID, feeTxID, err)
			rbCtx, rbCancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer rbCancel()
			if revErr := charger.ReverseCharge(rbCtx, charge1.TransactionID, orderNumber+"-VENUE-RB", venueShare, currency); revErr != nil {
				log.Printf("[PayOrder] ALERT: hold reversal ALSO failed order=%s tx=%s: %v", orderNumber, charge1.TransactionID, revErr)
			}
			if feeTxID != "" {
				if revErr := charger.ReverseCharge(rbCtx, feeTxID, orderNumber+"-FEE-RB", feeShare, currency); revErr != nil {
					log.Printf("[PayOrder] ALERT: fee hold reversal ALSO failed order=%s tx=%s: %v", orderNumber, feeTxID, revErr)
				}
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "No se pudo registrar el pago. No se ha realizado ningún cargo — intenta de nuevo."})
			return
		}
		log.Printf("[PayOrder] HELD (awaiting approval) order=%s venue=%.2f fee=%.2f %s deadline=%s",
			orderNumber, venueShare, feeShare, currency, deadline.Format(time.RFC3339))

		// Notify staff (push) + email the buyer the "pending approval" notice
		// (NO ticket/QR yet — that only goes out on approval).
		go func() {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer bgCancel()
			if services.Push != nil {
				services.Push.NotifyVenueStaff(bgCtx, venueID, "Nueva solicitud de entrada",
					services.GetString(order, "user_name")+" solicita entrada — pendiente de aprobar",
					"reservations", map[string]interface{}{
						"type":     "order_pending_approval",
						"order_id": req.OrderID,
					})
			}
			if services.Email != nil {
				sendApprovalStatusEmail(bgCtx, venueID, order, total, currency, "pending", false)
			}
		}()

		c.JSON(http.StatusOK, gin.H{
			"success":          true,
			"pending_approval": true,
			"message":          "Payment authorized, awaiting staff approval",
			"order_number":     orderNumber,
			"order_id":         req.OrderID,
		})
		return
	}

	// --- PUBLIC: funds captured. Register for the shared confirmation rail
	// (issues tickets + email + push). ---
	sessionID := "neonet_" + charge1.TransactionID
	services.RegisterNeoNetPayment(sessionID, &models.PaymentResult{
		Success:           true,
		TransactionID:     charge1.TransactionID,
		AuthorizationCode: charge1.AuthCode,
		Gateway:           processor.GetGateway(),
		CardLast4:         charge1.CardLast4,
		CardBrand:         charge1.CardBrand,
	})
	// Ruta de dinero: el cargo YA se ejecutó (capture=true). Si no se puede
	// persistir la sesión, ConfirmPayment devolvería 404 con el cliente ya
	// cobrado — reintentar una vez y si no, gritar con los tx IDs para poder
	// reversar/conciliar a mano.
	writeCharge := func() error {
		return venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
			"status":            "processing",
			"stripe_session_id": sessionID,
			"payment_gateway":   "neonet",
			"metadata":          metadata,
		}, map[string]interface{}{"id": req.OrderID})
	}
	if err := writeCharge(); err != nil {
		log.Printf("[PayOrder] charge persist failed, retrying once order=%s: %v", orderNumber, err)
		if err = writeCharge(); err != nil {
			log.Printf("[PayOrder] ALERT: charge persist FAILED after retry order=%s venueTx=%s feeTx=%s: %v",
				orderNumber, charge1.TransactionID, feeTxID, err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "El pago se procesó pero hubo un error registrándolo. NO vuelvas a pagar — contacta al local con tu número de orden " + orderNumber + ".",
			})
			return
		}
	}

	log.Printf("[PayOrder] both charges OK order=%s venue=%.2f fee=%.2f %s", orderNumber, venueShare, feeShare, currency)

	// Delegar al carril compartido: emite tickets, email y push.
	q := c.Request.URL.Query()
	q.Set("session_id", sessionID)
	q.Set("venue_id", venueID)
	c.Request.URL.RawQuery = q.Encode()
	ConfirmPayment(c)
}

// heldCaptureOutcome distinguishes "not a held order" from "held but the
// gateway capture failed", so the approve handler can respond correctly.
type heldCaptureOutcome int

const (
	notHeld       heldCaptureOutcome = iota // no gateway authorization to settle
	captureOK                               // funds captured; proceed to issue tickets
	captureFailed                           // held, but the gateway rejected the capture
)

// captureHeldOrder settles the two held authorizations of a private-event
// order (on staff approval) and returns the sessionID to feed ConfirmPayment.
func captureHeldOrder(ctx context.Context, venueID string, order map[string]interface{}) (sessionID string, outcome heldCaptureOutcome) {
	metadata, _ := order["metadata"].(map[string]interface{})
	split, _ := metadata["payment_split"].(map[string]interface{})
	if split == nil || services.GetString(split, "gateway") != "neonet" {
		return "", notHeld
	}
	venueTx := services.GetString(split, "venue_transaction")
	if venueTx == "" {
		return "", notHeld
	}
	// Already captured (e.g. a retried approve) — nothing to settle, but the
	// session MUST re-registrarse en el mapa en memoria: es por-máquina y
	// por-proceso, y ConfirmPayment lo consume (LoadAndDelete). Sin esto un
	// retry (u otra máquina) da "Failed to confirm payment" con el dinero ya
	// capturado.
	if captured, _ := split["captured"].(bool); captured {
		sessionID = "neonet_" + venueTx
		services.RegisterNeoNetPayment(sessionID, &models.PaymentResult{
			Success:       true,
			TransactionID: venueTx,
			Gateway:       models.GatewayNeoNet,
		})
		return sessionID, captureOK
	}
	processor, err := services.Payments.GetProcessor(ctx, venueID)
	if err != nil {
		return "", notHeld
	}
	charger, isCharger := processor.(services.DirectCardCharger)
	if !isCharger {
		return "", notHeld
	}
	orderNumber := services.GetString(order, "order_number")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	// Capture the venue share first — if THIS fails, the money was never
	// taken, so report the failure and let staff retry (the auth may have
	// expired issuer-side near the 48h edge).
	if err := charger.CapturePayment(ctx, venueTx, orderNumber+"-VENUE-CAP", services.GetFloat64(split, "venue_amount"), currency); err != nil {
		log.Printf("[Approve] venue capture FAILED order=%s: %v", orderNumber, err)
		return "", captureFailed
	}
	if feeTx := services.GetString(split, "fee_transaction"); feeTx != "" {
		if err := charger.CapturePayment(ctx, feeTx, orderNumber+"-FEE-CAP", services.GetFloat64(split, "fee_amount"), currency); err != nil {
			// Venue already captured; log but continue so tickets still issue.
			log.Printf("[Approve] fee capture failed (venue already captured) order=%s: %v", orderNumber, err)
		}
	}
	split["captured"] = true

	sessionID = "neonet_" + venueTx
	services.RegisterNeoNetPayment(sessionID, &models.PaymentResult{
		Success:       true,
		TransactionID: venueTx,
		Gateway:       processor.GetGateway(),
	})
	return sessionID, captureOK
}

// reverseHeldOrder releases the two held authorizations of a private-event
// order (on staff rejection). Returns (held, released): held=false si no hay
// autorizaciones que liberar; released=false si ALGUNA reversa falló en la
// pasarela (queda released_ok=false en metadata y ALERT en logs — la
// autorización caducará sola del lado del emisor, pero no digas "liberada").
func reverseHeldOrder(ctx context.Context, venueID string, order map[string]interface{}) (held bool, released bool) {
	metadata, _ := order["metadata"].(map[string]interface{})
	split, _ := metadata["payment_split"].(map[string]interface{})
	if split == nil || services.GetString(split, "gateway") != "neonet" {
		return false, false
	}
	// If already captured, a reversal won't work — would need a refund.
	if captured, _ := split["captured"].(bool); captured {
		return false, false
	}
	venueTx := services.GetString(split, "venue_transaction")
	if venueTx == "" {
		return false, false
	}
	processor, err := services.Payments.GetProcessor(ctx, venueID)
	if err != nil {
		return false, false
	}
	charger, isCharger := processor.(services.DirectCardCharger)
	if !isCharger {
		return false, false
	}
	orderNumber := services.GetString(order, "order_number")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	released = true
	if err := charger.ReverseCharge(ctx, venueTx, orderNumber+"-VENUE-REL", services.GetFloat64(split, "venue_amount"), currency); err != nil {
		log.Printf("[Reject] venue auth reversal failed order=%s: %v", orderNumber, err)
		released = false
	}
	if feeTx := services.GetString(split, "fee_transaction"); feeTx != "" {
		if err := charger.ReverseCharge(ctx, feeTx, orderNumber+"-FEE-REL", services.GetFloat64(split, "fee_amount"), currency); err != nil {
			log.Printf("[Reject] fee auth reversal failed order=%s: %v", orderNumber, err)
			released = false
		}
	}
	// Traceability: stamp when the hold was released and whether it fully
	// succeeded, so a failed reversal can be spotted/retried later.
	split["released_at"] = time.Now().Format(time.RFC3339)
	split["released_ok"] = released
	venueDB := services.DB.ForVenue(venueID)
	if venueDB != nil {
		venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{"metadata": metadata},
			map[string]interface{}{"id": services.GetString(order, "id")})
	}
	if !released {
		log.Printf("[Reject] ALERT: REVERSAL INCOMPLETE order=%s — revisar en EBC y reversar a mano si sigue retenida",
			orderNumber)
	}
	return true, released
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// claimHeldOrder atomically claims a held (payment_authorized) order for
// settlement/release, flipping it to 'processing' only if it's still
// payment_authorized. Returns true for the single caller that wins — so
// approve, reject and the 48h expiry job can't act on the same hold twice.
func claimHeldOrder(ctx context.Context, venueDB *services.SupabaseClient, orderID string) bool {
	res, err := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
		"status": "processing",
	}, map[string]interface{}{
		"id":     orderID,
		"status": "payment_authorized",
	})
	return err == nil && len(res) > 0
}

// sendApprovalStatusEmail sends the private-event status email to the buyer.
// kind: "pending" (awaiting approval), "rejected" (staff declined),
// "expired" (48h passed). Resolves event + venue names for the copy.
func sendApprovalStatusEmail(ctx context.Context, venueID string, order map[string]interface{}, total float64, currency, kind string, expired bool) {
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil || services.Email == nil {
		return
	}
	eventName := ""
	if eid := services.GetString(order, "event_id"); eid != "" {
		if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "name", "where": map[string]interface{}{"id": eid},
		}); ev != nil {
			eventName = services.GetString(ev, "name")
		}
	}
	venueName := ""
	if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
		"select": "name", "where": map[string]interface{}{"id": venueID},
	}); v != nil {
		venueName = services.GetString(v, "name")
	}
	to := services.GetString(order, "user_email")
	if to == "" {
		return
	}
	data := services.ApprovalEmailData{
		CustomerName: services.GetString(order, "user_name"),
		EventName:    eventName,
		VenueName:    venueName,
		OrderNumber:  services.GetString(order, "order_number"),
		Total:        fmt.Sprintf("%.2f", total),
		Currency:     currency,
	}
	switch kind {
	case "pending":
		_ = services.Email.SendApprovalPending(ctx, to, data)
	case "rejected", "expired":
		_ = services.Email.SendApprovalRejected(ctx, to, data, expired)
	}
}

// safeLookupCode reports whether a user-supplied lookup value (order number,
// UUID, QR token) is safe to pass to PostgREST as an equality filter. Dots
// are excluded on purpose: they enable operator injection ("not.is.null").
func safeLookupCode(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for _, r := range v {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

var _ = fmt.Sprintf // keep fmt if unused paths change
