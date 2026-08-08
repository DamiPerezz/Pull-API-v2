package controllers

import (
	"context"
	"log"
	"time"

	"pull-api-v2/services"
)

// =============================================
// APPROVAL EXPIRY JOB (private events)
// Private-event orders sit in payment_authorized with a 48h approval
// deadline. If staff neither approves nor rejects in time, this job reverses
// the held authorizations (releasing the buyer's funds), marks the order
// expired, and emails the buyer. Runs every 15 minutes across all venues.
// =============================================

// StartApprovalExpiryJob launches the periodic sweep in a background goroutine.
func StartApprovalExpiryJob() {
	go func() {
		// Small initial delay so the app finishes booting first.
		time.Sleep(2 * time.Minute)
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		expireOverdueAuthorizations()
		expireAbandonedPendingOrders()
		expirePrivateFlowOrders()
		reconcileStuckDLocalPayments()
		for range ticker.C {
			expireOverdueAuthorizations()
			expireAbandonedPendingOrders()
			expirePrivateFlowOrders()
			reconcileStuckDLocalPayments()
		}
	}()
	log.Printf("[ApprovalExpiry] job started (sweeps every 15m)")
}

// parseFlexTime parsea un timestamp tolerando el formato de Postgres
// (con o sin fracción de segundo).
func parseFlexTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// expireAbandonedPendingOrders libera el aforo de carritos públicos abandonados:
// órdenes en 'pending' cuyo expires_at (30 min) ya pasó. Sin esto, cada carrito
// abandonado dejaría su quantity_reserved bloqueado para siempre → el evento
// marcaría "agotado" con asistencia real menor. El claim pending→expired es
// atómico (UpdateCtx con WHERE status=pending): con 2 máquinas barriendo a la
// vez, solo una gana y libera → nunca se libera dos veces.
func expireAbandonedPendingOrders() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	venues, err := services.DB.Central().QueryCtx(ctx, "venues", map[string]interface{}{
		"select": "id",
		"where":  map[string]interface{}{"is_active": true, "deleted_at": "is.null"},
	})
	if err != nil {
		return
	}
	now := time.Now()
	for _, v := range venues {
		venueID := services.GetString(v, "id")
		venueDB := services.DB.ForVenue(venueID)
		if venueDB == nil {
			continue
		}
		orders, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "id,order_number,ticket_type_id,quantity,expires_at",
			"where":  map[string]interface{}{"status": "pending"},
		})
		if err != nil {
			continue
		}
		for _, order := range orders {
			expStr := services.GetString(order, "expires_at")
			if expStr == "" {
				continue
			}
			exp, ok := parseFlexTime(expStr)
			if !ok || now.Before(exp) {
				continue // sin expires_at parseable, o aún no caducó
			}
			orderID := services.GetString(order, "id")
			// Claim atómico pending→expired: solo el que gana libera el aforo.
			res, uerr := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
				"status":              "expired",
				"cancelled_at":        now.Format(time.RFC3339),
				"cancellation_reason": "Carrito abandonado (30 min sin pago)",
			}, map[string]interface{}{"id": orderID, "status": "pending"})
			if uerr != nil || len(res) == 0 {
				continue // otra instancia lo cogió, o ya se pagó
			}
			ttID := services.GetString(order, "ticket_type_id")
			qty := services.GetInt(order, "quantity")
			if ttID != "" && qty > 0 {
				if _, e := venueDB.CallRPC(ctx, "release_ticket_type", map[string]interface{}{
					"p_id": ttID, "p_qty": qty,
				}); e != nil {
					log.Printf("[PendingExpiry] ALERT release falló order=%s tt=%s qty=%d: %v",
						services.GetString(order, "order_number"), ttID, qty, e)
				}
			}
			log.Printf("[PendingExpiry] carrito abandonado liberado order=%s venue=%s qty=%d",
				services.GetString(order, "order_number"), venueID, qty)
		}
	}
}

// Ventanas del reconciliador de pagos dLocal.
//
//	dlocalStuckAfter    : antes de esto no se toca nada — el comprador puede
//	                      estar tecleando la tarjeta en la página de dLocal.
//	dlocalGiveUpAfter   : si dLocal SIGUE diciendo PENDING pasado esto, se da
//	                      el checkout por abandonado y se devuelve la plaza.
const (
	dlocalStuckAfter  = 20 * time.Minute
	dlocalGiveUpAfter = 3 * time.Hour
)

// reconcileStuckDLocalPayments arregla las órdenes que se quedaron en
// `processing`, que es donde caen todas las que empiezan un checkout de dLocal.
// Hace DOS cosas, y las dos importan:
//
//  1. RESCATA COBROS SIN ENTRADA. Si el webhook de dLocal no llega (caída,
//     timeout, DNS), hoy nadie se entera: el cliente pagó y se queda sin
//     ticket. Aquí se vuelve a preguntar a dLocal por cada pago y, si dice
//     PAID, se emiten los tickets por el mismo carril que el webhook.
//
//  2. DEVUELVE PLAZAS DE CHECKOUTS ABANDONADOS. Quien abre el checkout y
//     cierra la pestaña deja su plaza retenida PARA SIEMPRE: ningún barrido
//     miraba `processing`. Con aforo real eso agota el evento con la sala
//     medio vacía.
//
// La verdad la tiene SIEMPRE dLocal: nunca se caduca una orden por el reloj
// sin preguntar antes. Si dLocal no contesta, no se toca nada — es preferible
// una plaza retenida de más que cancelar a alguien que sí pagó.
func reconcileStuckDLocalPayments() {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	venues, err := services.DB.Central().QueryCtx(ctx, "venues", map[string]interface{}{
		"select": "id",
		"where":  map[string]interface{}{"is_active": true, "deleted_at": "is.null"},
	})
	if err != nil {
		log.Printf("[dLocalRecon] no se pudieron listar los venues: %v", err)
		return
	}

	now := time.Now()
	for _, v := range venues {
		venueID := services.GetString(v, "id")
		venueDB := services.DB.ForVenue(venueID)
		if venueDB == nil {
			continue
		}
		orders, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "id,order_number,event_id,ticket_type_id,quantity,total,currency,status,user_name,user_email,metadata,stripe_session_id,paid_at,created_at,updated_at",
			"where":  map[string]interface{}{"status": "processing"},
			"limit":  1000,
		})
		if err != nil {
			continue
		}
		for _, order := range orders {
			paymentID := services.DLocalPaymentIDFromSession(services.GetString(order, "stripe_session_id"))
			if paymentID == "" {
				continue // no es un checkout de dLocal (o aún no tiene sesión)
			}
			started := dlocalCheckoutStartedAt(order)
			if started.IsZero() || now.Sub(started) < dlocalStuckAfter {
				continue // demasiado reciente: puede estar pagando ahora mismo
			}

			payment, err := dlocalGetPayment(ctx, venueID, paymentID)
			if err != nil || payment == nil {
				log.Printf("[dLocalRecon] no se pudo consultar el pago %s (order=%s): %v — se reintenta en el próximo barrido",
					paymentID, services.GetString(order, "order_number"), err)
				continue
			}

			outcome := applyDLocalPaymentToOrder(ctx, venueID, venueDB, order, payment)
			log.Printf("[dLocalRecon] order=%s pago=%s dlocal=%s → %s (lleva %s en processing)",
				services.GetString(order, "order_number"), paymentID, payment.Status, outcome,
				now.Sub(started).Truncate(time.Minute))

			// Sigue sin resolverse y ya pasó el límite duro: checkout abandonado.
			// Se cierra y se devuelve la plaza, con claim atómico por si el
			// comprador vuelve justo ahora.
			if outcome == dlOutcomePending && now.Sub(started) >= dlocalGiveUpAfter {
				res, uerr := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
					"status":              "expired",
					"cancelled_at":        now.Format(time.RFC3339),
					"cancellation_reason": "Checkout abandonado (sin resolución en dLocal)",
				}, map[string]interface{}{"id": services.GetString(order, "id"), "status": "processing"})
				if uerr != nil || len(res) == 0 {
					continue
				}
				releaseOrderCapacity(ctx, venueDB, order)
				log.Printf("[dLocalRecon] checkout abandonado → expired, aforo liberado order=%s qty=%d",
					services.GetString(order, "order_number"), services.GetInt(order, "quantity"))
			}
		}
	}
}

// dlocalCheckoutStartedAt devuelve cuándo empezó el INTENTO de pago. Con el
// flujo privado, `created_at` no vale: una solicitud puede crearse hoy y
// pagarse mañana, y usar la fecha de la orden caducaría un pago en curso.
func dlocalCheckoutStartedAt(order map[string]interface{}) time.Time {
	if md, ok := order["metadata"].(map[string]interface{}); ok {
		if t, ok2 := parseFlexTime(services.GetString(md, "checkout_started_at")); ok2 {
			return t
		}
	}
	// Órdenes anteriores a que se sellara checkout_started_at.
	if t, ok := parseFlexTime(services.GetString(order, "updated_at")); ok {
		return t
	}
	if t, ok := parseFlexTime(services.GetString(order, "created_at")); ok {
		return t
	}
	return time.Time{}
}

// expirePrivateFlowOrders barre los DOS estados del flujo privado con dLocal:
//
//   - awaiting_approval : el staff nunca decidió. Caduca con `expires_at` (48 h).
//   - approved_unpaid   : aprobada, pero el cliente no pagó su enlace. Caduca
//     con `metadata.payment_deadline` (24 h por defecto).
//
// En ambos casos NO hay dinero de por medio (con dLocal Go no se retiene
// nada), así que caducar es solo: marcar `expired` y DEVOLVER LA PLAZA. Sin
// esto, cada solicitud olvidada bloquea aforo para siempre y el evento se
// marca agotado con la sala medio vacía.
//
// El claim es atómico (UPDATE ... WHERE status=<el de partida>): si dos
// máquinas barren a la vez, o si el staff aprueba justo en ese instante, solo
// uno gana y el aforo se libera exactamente una vez.
func expirePrivateFlowOrders() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	venues, err := services.DB.Central().QueryCtx(ctx, "venues", map[string]interface{}{
		"select": "id",
		"where":  map[string]interface{}{"is_active": true, "deleted_at": "is.null"},
	})
	if err != nil {
		log.Printf("[PrivateExpiry] no se pudieron listar los venues: %v", err)
		return
	}

	now := time.Now()
	for _, v := range venues {
		venueID := services.GetString(v, "id")
		venueDB := services.DB.ForVenue(venueID)
		if venueDB == nil {
			continue
		}
		orders, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "id,order_number,status,ticket_type_id,quantity,total,currency,user_name,user_email,expires_at,metadata",
			"where":  map[string]interface{}{"status": "in.(awaiting_approval,approved_unpaid)"},
			"limit":  2000,
		})
		if err != nil {
			continue
		}
		for _, order := range orders {
			status := services.GetString(order, "status")

			// Cada estado mira su propio reloj.
			var deadlineStr, reason string
			if status == "approved_unpaid" {
				if md, ok := order["metadata"].(map[string]interface{}); ok {
					deadlineStr = services.GetString(md, "payment_deadline")
				}
				reason = "Plazo de pago agotado (solicitud aprobada sin pagar)"
			} else {
				deadlineStr = services.GetString(order, "expires_at")
				reason = "Solicitud caducada sin respuesta del staff"
			}
			if deadlineStr == "" {
				continue
			}
			deadline, ok := parseFlexTime(deadlineStr)
			if !ok || now.Before(deadline) {
				continue
			}

			orderID := services.GetString(order, "id")
			res, uerr := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
				"status":              "expired",
				"cancelled_at":        now.Format(time.RFC3339),
				"cancellation_reason": reason,
			}, map[string]interface{}{"id": orderID, "status": status})
			if uerr != nil || len(res) == 0 {
				continue // otro actor llegó antes (staff aprobó, cliente pagó…)
			}

			releaseOrderCapacity(ctx, venueDB, order)

			// Solo avisamos a quien YA le habíamos prometido algo: al aprobado
			// que no llegó a pagar. Al que nunca se le respondió no se le manda
			// un "caducó" sin contexto.
			if status == "approved_unpaid" && services.Email != nil {
				orderCopy := order
				total := services.GetFloat64(order, "total")
				currency := services.GetString(order, "currency")
				if currency == "" {
					currency = "GTQ"
				}
				services.RunBackground("payment-expired-notify", func(bgCtx context.Context) error {
					data, to := buildApprovalEmailData(bgCtx, venueID, orderCopy, total, currency)
					if to == "" {
						return nil
					}
					if e := services.Email.SendApprovalPaymentExpired(bgCtx, to, data, privatePaymentDeadlineHours()); e != nil {
						log.Printf("[PrivateExpiry] ALERT email de caducidad NO enviado order=%s: %v",
							services.GetString(orderCopy, "order_number"), e)
					}
					return nil
				})
			}

			log.Printf("[PrivateExpiry] %s → expired, aforo liberado order=%s venue=%s qty=%d",
				status, services.GetString(order, "order_number"), venueID, services.GetInt(order, "quantity"))
		}
	}
}

// expireOverdueAuthorizations scans every active venue for payment_authorized
// orders past their approval_deadline and releases them.
func expireOverdueAuthorizations() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	venues, err := services.DB.Central().QueryCtx(ctx, "venues", map[string]interface{}{
		"select": "id",
		"where":  map[string]interface{}{"is_active": true, "deleted_at": "is.null"},
	})
	if err != nil {
		log.Printf("[ApprovalExpiry] cannot list venues: %v", err)
		return
	}

	now := time.Now()
	for _, v := range venues {
		venueID := services.GetString(v, "id")
		venueDB := services.DB.ForVenue(venueID)
		if venueDB == nil {
			continue
		}
		orders, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "id,order_number,event_id,currency,total,user_name,user_email,metadata",
			"where":  map[string]interface{}{"status": "payment_authorized"},
		})
		if err != nil {
			continue
		}
		for _, order := range orders {
			metadata, _ := order["metadata"].(map[string]interface{})
			deadlineStr := services.GetString(metadata, "approval_deadline")
			if deadlineStr == "" {
				continue
			}
			deadline, perr := time.Parse(time.RFC3339, deadlineStr)
			if perr != nil || now.Before(deadline) {
				continue // not overdue yet
			}

			orderID := services.GetString(order, "id")
			// RACE GUARD: claim the hold atomically. If staff approved/rejected
			// it in this same window, the claim fails and we skip it.
			if !claimHeldOrder(ctx, venueDB, orderID) {
				continue
			}
			// Release the held authorizations (venue + fee).
			_, released := reverseHeldOrder(ctx, venueID, order)
			venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
				"status":              "expired",
				"cancelled_at":        now.Format(time.RFC3339),
				"cancellation_reason": "Autoexpirada: sin decisión del staff en 48h",
			}, map[string]interface{}{"id": orderID})
			log.Printf("[ApprovalExpiry] expired order=%s venue=%s released=%v",
				services.GetString(order, "order_number"), venueID, released)

			cur := services.GetString(order, "currency")
			if cur == "" {
				cur = "GTQ"
			}
			sendApprovalStatusEmail(ctx, venueID, order, services.GetFloat64(order, "total"), cur, "expired", true)
		}
	}
}
