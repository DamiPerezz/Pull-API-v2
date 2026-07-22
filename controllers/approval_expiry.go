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
		for range ticker.C {
			expireOverdueAuthorizations()
			expireAbandonedPendingOrders()
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
