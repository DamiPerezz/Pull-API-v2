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
		for range ticker.C {
			expireOverdueAuthorizations()
		}
	}()
	log.Printf("[ApprovalExpiry] job started (sweeps every 15m)")
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
