package controllers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"pull-api-v2/config"
	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// =============================================================================
// FLUJO PRIVADO CON dLOCAL GO (opción B): solicitar → aprobar → pagar.
//
// dLocal Go NO sabe retener dinero sin cobrarlo, así que el flujo de eventos
// privados cambió: el cliente solicita SIN pagar (`awaiting_approval`), y si
// el staff aprueba se le manda un ENLACE DE PAGO con plazo
// (`approved_unpaid`). El ticket se emite cuando paga.
// Diseño completo: DLOCAL-FLUJO-PRIVADO.md (raíz del workspace).
// =============================================================================

// privatePaymentDeadlineHours es el plazo que tiene el cliente para pagar tras
// la aprobación. Pasado ese plazo la solicitud caduca y se libera el aforo.
func privatePaymentDeadlineHours() int {
	if v := strings.TrimSpace(os.Getenv("PRIVATE_PAYMENT_DEADLINE_HOURS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 24
}

// buildPaymentLink arma el enlace donde el cliente paga su solicitud aprobada.
// La página de pago ya sabe retomar una orden existente con ?order_id=.
func buildPaymentLink(ctx context.Context, venueDB *services.SupabaseClient, order map[string]interface{}) string {
	eventSlug := ""
	if eid := services.GetString(order, "event_id"); eid != "" {
		if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "slug", "where": map[string]interface{}{"id": eid},
		}); ev != nil {
			eventSlug = services.GetString(ev, "slug")
		}
	}
	if eventSlug == "" {
		return ""
	}
	code := ""
	if md, ok := order["metadata"].(map[string]interface{}); ok {
		code = services.GetString(md, "payment_link_code")
	}
	qty := services.GetInt(order, "quantity")
	if qty <= 0 {
		qty = 1
	}
	return fmt.Sprintf("%s/es/event/%s/tickets/%s/%d?order_id=%s&code=%s",
		strings.TrimRight(config.App.FrontendURL, "/"),
		eventSlug,
		services.GetString(order, "ticket_type_id"),
		qty,
		services.GetString(order, "id"),
		code,
	)
}

// claimAwaitingOrder hace el flip atómico awaiting_approval → nextStatus. Solo
// un actor gana (staff que aprueba, staff que rechaza, o el job de caducidad),
// igual que el claim de las retenciones de NeoNet.
func claimAwaitingOrder(ctx context.Context, venueDB *services.SupabaseClient, orderID, nextStatus string, extra map[string]interface{}) bool {
	updates := map[string]interface{}{"status": nextStatus}
	for k, v := range extra {
		updates[k] = v
	}
	res, err := venueDB.UpdateCtx(ctx, "orders", updates, map[string]interface{}{
		"id":     orderID,
		"status": "awaiting_approval",
	})
	return err == nil && len(res) > 0
}

// releaseOrderCapacity devuelve al aforo las plazas que la solicitud tenía
// reservadas (rechazo o caducidad). Sin esto, una plaza rechazada quedaría
// bloqueada para siempre.
func releaseOrderCapacity(ctx context.Context, venueDB *services.SupabaseClient, order map[string]interface{}) {
	ttID := services.GetString(order, "ticket_type_id")
	qty := services.GetInt(order, "quantity")
	if ttID == "" || qty <= 0 {
		return
	}
	if _, err := venueDB.CallRPC(ctx, "release_ticket_type", map[string]interface{}{
		"p_id": ttID, "p_qty": qty,
	}); err != nil {
		log.Printf("[PrivateFlow] ALERT no se pudo liberar aforo order=%s tt=%s qty=%d: %v",
			services.GetString(order, "order_number"), ttID, qty, err)
	}
}

// sendApprovalApprovedEmail manda el email "aprobada — paga aquí" con el
// enlace y el plazo. Si falla, ALERT: el cliente aprobado se quedaría sin
// saber que tiene que pagar.
func sendApprovalApprovedEmail(ctx context.Context, venueID string, order map[string]interface{}, total float64, currency, payURL string, deadline time.Time) {
	if services.Email == nil {
		return
	}
	data, to := buildApprovalEmailData(ctx, venueID, order, total, currency)
	if to == "" {
		log.Printf("[PrivateFlow] ALERT orden aprobada SIN email de destino order=%s",
			services.GetString(order, "order_number"))
		return
	}
	hours := int(time.Until(deadline).Hours())
	if hours < 1 {
		hours = 1
	}
	if err := services.Email.SendApprovalApproved(ctx, to, data, payURL, hours); err != nil {
		log.Printf("[PrivateFlow] ALERT email de aprobación NO enviado order=%s to=%s: %v — avisar al cliente a mano (enlace: %s)",
			services.GetString(order, "order_number"), to, err, payURL)
	}
}

// approveAwaitingOrder aprueba una SOLICITUD sin pago: la pasa a
// approved_unpaid, fija el plazo de pago y manda el enlace por email.
// NO mueve dinero (no hay nada retenido).
func approveAwaitingOrder(c *gin.Context, ctx context.Context, venueDB *services.SupabaseClient, venueID, orderID string, order map[string]interface{}) {
	deadline := time.Now().Add(time.Duration(privatePaymentDeadlineHours()) * time.Hour)

	metadata, _ := order["metadata"].(map[string]interface{})
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	metadata["approved_by"] = c.GetString("staff_id")
	metadata["approved_at"] = time.Now().Format(time.RFC3339)
	metadata["payment_deadline"] = deadline.Format(time.RFC3339)

	// Claim atómico: si otro staff (o el job de caducidad) llegó antes, perdemos.
	if !claimAwaitingOrder(ctx, venueDB, orderID, "approved_unpaid", map[string]interface{}{
		"metadata": metadata,
	}) {
		c.JSON(http.StatusConflict, gin.H{"error": "Esta solicitud ya fue procesada por otra persona"})
		return
	}

	payURL := buildPaymentLink(ctx, venueDB, order)
	if payURL == "" {
		// Sin enlace el cliente no puede pagar: no dejarlo en silencio.
		log.Printf("[PrivateFlow] ALERT no pude construir el enlace de pago order=%s", orderID)
	}

	total := services.GetFloat64(order, "total")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	orderCopy := order
	services.RunBackground("approved-payment-link", func(bgCtx context.Context) error {
		sendApprovalApprovedEmail(bgCtx, venueID, orderCopy, total, currency, payURL, deadline)
		return nil
	})

	log.Printf("[PrivateFlow] APROBADA order=%s → esperando pago (plazo %s)",
		services.GetString(order, "order_number"), deadline.Format(time.RFC3339))

	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"awaiting_payment": true,
		"message":          "Solicitud aprobada. Se envió el enlace de pago al cliente.",
		"order_id":         orderID,
		"order_number":     services.GetString(order, "order_number"),
		"payment_url":      payURL,
		"payment_deadline": deadline.Format(time.RFC3339),
	})
}

// ResendPaymentLink reenvía el enlace de pago de una solicitud YA aprobada.
// POST /orders/:orderId/resend-payment-link  (staff)
// Idempotente: NO cambia el estado ni el plazo, solo vuelve a mandar el correo
// (y devuelve la URL para que el staff pueda compartirla a mano). Sin esto, un
// cliente que pierde el email se queda sin forma de pagar.
func ResendPaymentLink(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	if role := c.GetString("role"); role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Solo un admin o manager puede reenviar el enlace de pago"})
		return
	}
	venueID := c.GetString("venue_id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	orderID := c.Param("orderId")
	order, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,status,total,currency,event_id,ticket_type_id,quantity,user_name,user_email,metadata",
		"where":  map[string]interface{}{"id": orderID},
	})
	if order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}
	if st := services.GetString(order, "status"); st != "approved_unpaid" {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "Solo se puede reenviar el enlace de una solicitud aprobada pendiente de pago",
			"status": st,
		})
		return
	}

	payURL := buildPaymentLink(ctx, venueDB, order)
	if payURL == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "No se pudo construir el enlace de pago"})
		return
	}
	// Plazo: el que ya tenía; si no se puede leer, se informa sin inventar.
	deadline := time.Time{}
	if md, ok := order["metadata"].(map[string]interface{}); ok {
		if d, ok2 := parseFlexTime(services.GetString(md, "payment_deadline")); ok2 {
			deadline = d
		}
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(privatePaymentDeadlineHours()) * time.Hour)
	}

	total := services.GetFloat64(order, "total")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	orderCopy := order
	services.RunBackground("resend-payment-link", func(bgCtx context.Context) error {
		sendApprovalApprovedEmail(bgCtx, venueID, orderCopy, total, currency, payURL, deadline)
		return nil
	})
	log.Printf("[PrivateFlow] enlace de pago REENVIADO order=%s", services.GetString(order, "order_number"))

	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"message":          "Enlace de pago reenviado al cliente",
		"payment_url":      payURL,
		"payment_deadline": deadline.Format(time.RFC3339),
	})
}

// rejectAwaitingOrder rechaza una SOLICITUD sin pago: la cancela, libera el
// aforo y avisa al cliente. No hay dinero que devolver.
func rejectAwaitingOrder(c *gin.Context, ctx context.Context, venueDB *services.SupabaseClient, venueID, orderID, reason string, order map[string]interface{}) {
	if !claimAwaitingOrder(ctx, venueDB, orderID, "cancelled", map[string]interface{}{
		"cancelled_at":        time.Now().Format(time.RFC3339),
		"cancellation_reason": reason,
	}) {
		c.JSON(http.StatusConflict, gin.H{"error": "Esta solicitud ya fue procesada por otra persona"})
		return
	}

	releaseOrderCapacity(ctx, venueDB, order)

	total := services.GetFloat64(order, "total")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	orderCopy := order
	services.RunBackground("rejected-notify", func(bgCtx context.Context) error {
		sendApprovalStatusEmail(bgCtx, venueID, orderCopy, total, currency, "rejected", false)
		return nil
	})

	log.Printf("[PrivateFlow] RECHAZADA order=%s (aforo liberado)", services.GetString(order, "order_number"))

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      "Solicitud rechazada. No se ha cobrado nada al cliente.",
		"order_id":     orderID,
		"order_number": services.GetString(order, "order_number"),
	})
}
