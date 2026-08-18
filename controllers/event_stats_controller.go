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
// Devuelve el dinero y el recuento de personas por estado: escaneadas
// (dentro), con entrada sin escanear, pendientes de decisión, rechazadas y
// expiradas.
//
// OJO con el dinero: volvimos a NeoNet/Cybersource y con ellos a la RETENCIÓN.
// En un evento privado el importe se bloquea en la tarjeta al solicitar
// (`payment_authorized`) y solo se cobra al aprobar. Por eso "cobrado" es
// EXCLUSIVAMENTE `confirmed`, y lo retenido va aparte: es dinero comprometido
// que TODAVÍA no ha entrado y que se libera solo si nadie decide en 48 h.
//
// Los estados `awaiting_approval` / `approved_unpaid` son del desvío de dLocal
// (retirado en agosto 2026). Ya no se generan, pero se siguen contando porque
// hay histórico y puede quedar alguna orden viva: si desaparecieran del panel,
// el staff vería un aforo que no cuadra con la realidad.
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
	// esto son 3 queries por móvil compitiendo con el checkout.
	cacheKey := venueID + ":" + eventID
	if cached, ok := getCachedStats(cacheKey); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	// --- Dinero y personas: órdenes del evento agrupadas por estado. Los
	// totales YA incluyen el fee. Una sola query con `status=in.(...)` en vez
	// de una por estado — el panel refresca cada 30s desde varios móviles y
	// cada round-trip compite con el checkout por el pool de Supabase.
	// Agregamos en Go: un evento <1000 personas son pocas filas. El límite
	// explícito es para no truncar en silencio si algún día son muchas.
	const statsRowLimit = 5000
	currency := "GTQ"
	type orderBucket struct {
		total  float64 // dinero de esas órdenes (con fee)
		people int     // suma de quantity = personas
		orders int
	}
	buckets := map[string]*orderBucket{}
	rows, _ := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
		"select": "status,total,quantity,currency",
		"where": map[string]interface{}{
			"event_id": eventID,
			"status":   "in.(confirmed,payment_authorized,awaiting_approval,approved_unpaid,cancelled,expired)",
		},
		"limit": statsRowLimit,
	})
	for _, r := range rows {
		status := services.GetString(r, "status")
		b := buckets[status]
		if b == nil {
			b = &orderBucket{}
			buckets[status] = b
		}
		b.total += services.GetFloat64(r, "total")
		b.people += services.GetInt(r, "quantity")
		b.orders++
		if cur := services.GetString(r, "currency"); cur != "" {
			currency = cur
		}
	}
	bucketOf := func(status string) orderBucket {
		if b := buckets[status]; b != nil {
			return *b
		}
		return orderBucket{}
	}
	paid := bucketOf("confirmed")                 // pagadas: dinero YA en caja
	held := bucketOf("payment_authorized")        // RETENIDO en tarjeta, esperando decisión
	awaiting := bucketOf("awaiting_approval")     // LEGACY dLocal: sin decidir y SIN dinero
	approvedUnpaid := bucketOf("approved_unpaid") // LEGACY dLocal: aprobada, aún sin pagar
	rejected := bucketOf("cancelled")
	expired := bucketOf("expired")

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
			// Cobrado de verdad. Solo `confirmed`: retener ≠ cobrar.
			"collected": round2(paid.total),
			// Alias legacy para builds móviles anteriores al cambio de flujo,
			// que leen revenue.captured. Mismo valor que collected.
			"captured": round2(paid.total),
			// RETENIDO en tarjeta y sin capturar. No está cobrado: si nadie
			// decide en 48 h la retención se libera y este dinero no entra.
			"pending_capture": round2(held.total),
			// LEGACY dLocal: aprobadas esperando que el cliente pague su
			// enlace. Se mantiene la clave porque las builds móviles viejas
			// la leen como "pendiente de cobro" y quitarla les pondría 0.
			"pending_payment": round2(approvedUnpaid.total),
			"currency":        currency,
		},
		"people": gin.H{
			"scanned":            scanned,               // ya dentro (QR escaneado)
			"pending_scan":       pendingScan,           // con entrada, aún no han llegado
			"total_tickets":      totalTickets,          // entradas emitidas
			"paid":               paid.people,           // han pagado
			"payment_authorized": held.people,           // retenidos esperando al staff
			"awaiting_approval":  awaiting.people,       // LEGACY dLocal sin decidir
			"approved_unpaid":    approvedUnpaid.people, // LEGACY dLocal sin pagar
			"rejected":           rejected.people,
			"expired":            expired.people,
		},
		"orders": gin.H{
			"confirmed":          paid.orders,
			"payment_authorized": held.orders,
			"awaiting_approval":  awaiting.orders,
			"approved_unpaid":    approvedUnpaid.orders,
			"rejected":           rejected.orders,
			"expired":            expired.orders,
		},
	}
	setCachedStats(cacheKey, payload)
	c.JSON(http.StatusOK, payload)
}
