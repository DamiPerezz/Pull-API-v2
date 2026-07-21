package controllers

import (
	"context"
	"net/http"
	"time"

	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// MobileGetEventStats — panel de control del evento para el staff.
// GET /event/stats/:eventId  (auth staff)
//
// Devuelve el dinero cobrado/retenido y el recuento de personas por estado:
// escaneadas (dentro), con entrada sin escanear, pendientes de aprobar,
// rechazadas y expiradas.
func MobileGetEventStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	venueID := c.GetString("venue_id")
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
	eventID := c.Param("eventId")
	if eventID == "" {
		eventID = c.Param("event_id")
	}
	if !safeLookupCode(eventID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid event id"})
		return
	}

	// Cache 20s: el panel refresca cada 30s en varios móviles del staff; sin
	// esto son 6 queries pesadas por móvil compitiendo con el checkout.
	cacheKey := venueID + ":" + eventID
	if cached, ok := getCachedStats(cacheKey); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	// --- Dinero: órdenes del evento por estado. Los totales YA incluyen el
	// fee; para el venue lo que importa es lo cobrado (confirmed) y lo
	// retenido a la espera (payment_authorized). Sumamos en Go: un evento
	// <1000 personas son pocas filas.
	currency := "GTQ"
	sumOrders := func(status string) (float64, int, int) {
		rows, _ := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
			"select": "total,quantity,currency",
			"where":  map[string]interface{}{"event_id": eventID, "status": status},
		})
		total, people := 0.0, 0
		for _, r := range rows {
			total += services.GetFloat64(r, "total")
			people += services.GetInt(r, "quantity")
			if cur := services.GetString(r, "currency"); cur != "" {
				currency = cur
			}
		}
		return total, people, len(rows)
	}
	capturedTotal, _, confirmedOrders := sumOrders("confirmed")
	heldTotal, waitingPeople, waitingOrders := sumOrders("payment_authorized")
	_, rejectedPeople, rejectedOrders := sumOrders("cancelled")
	_, expiredPeople, expiredOrders := sumOrders("expired")

	// --- Personas con entrada: tickets emitidos y su estado de escaneo.
	totalTickets, _ := venueDB.CountCtx(ctx, "tickets", map[string]interface{}{
		"event_id": eventID,
	})
	scanned, _ := venueDB.CountCtx(ctx, "tickets", map[string]interface{}{
		"event_id": eventID, "checked_in_at": "not.is.null",
	})
	pendingScan := totalTickets - scanned
	if pendingScan < 0 {
		pendingScan = 0
	}

	payload := map[string]interface{}{
		"revenue": gin.H{
			"captured": round2(capturedTotal), // cobrado de verdad
			"held":     round2(heldTotal),     // retenido, pendiente de decisión
			"currency": currency,
		},
		"people": gin.H{
			"scanned":           scanned,       // ya dentro (QR escaneado)
			"pending_scan":      pendingScan,   // con entrada, aún no han llegado
			"total_tickets":     totalTickets,  // entradas emitidas
			"awaiting_approval": waitingPeople, // esperando al staff (retenidos)
			"rejected":          rejectedPeople,
			"expired":           expiredPeople,
		},
		"orders": gin.H{
			"confirmed":         confirmedOrders,
			"awaiting_approval": waitingOrders,
			"rejected":          rejectedOrders,
			"expired":           expiredOrders,
		},
	}
	setCachedStats(cacheKey, payload)
	c.JSON(http.StatusOK, payload)
}
