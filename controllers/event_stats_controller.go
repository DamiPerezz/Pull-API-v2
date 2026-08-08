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
// (dentro), con entrada sin escanear, solicitudes sin decidir, aprobadas que
// aún no han pagado, rechazadas y expiradas.
//
// OJO con el dinero (flujo dLocal Go, DLOCAL-FLUJO-PRIVADO.md): aprobar una
// solicitud NO cobra nada — solo manda un enlace de pago con plazo. Por eso
// aquí "cobrado" es EXCLUSIVAMENTE `confirmed`, y lo aprobado-sin-pagar va
// aparte como pendiente de cobro (puede caducar y no entrar nunca).
// Ya no existe "retenido": eso era la autorización de NeoNet/Cybersource,
// apagada desde agosto 2026. `payment_authorized` solo sobrevive en órdenes
// históricas, así que no se cuenta.
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
			"status":   "in.(confirmed,awaiting_approval,approved_unpaid,cancelled,expired)",
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
	awaiting := bucketOf("awaiting_approval")     // solicitud sin decidir, SIN dinero
	approvedUnpaid := bucketOf("approved_unpaid") // aprobada, enlace enviado, aún sin pagar
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
			// Cobrado de verdad. Solo `confirmed`: con dLocal, aprobar ≠ cobrar.
			"collected": round2(paid.total),
			// Alias legacy para builds móviles anteriores al cambio de flujo,
			// que leen revenue.captured. Mismo valor que collected.
			"captured": round2(paid.total),
			// Aprobadas esperando que el cliente pague su enlace. NO está
			// cobrado: si vence el plazo caduca y este dinero no entra nunca.
			"pending_payment": round2(approvedUnpaid.total),
			"currency":        currency,
		},
		"people": gin.H{
			"scanned":           scanned,               // ya dentro (QR escaneado)
			"pending_scan":      pendingScan,           // con entrada, aún no han llegado
			"total_tickets":     totalTickets,          // entradas emitidas
			"paid":              paid.people,           // han pagado
			"awaiting_approval": awaiting.people,       // solicitudes esperando al staff
			"approved_unpaid":   approvedUnpaid.people, // aprobados que aún no pagan
			"rejected":          rejected.people,
			"expired":           expired.people,
		},
		"orders": gin.H{
			"confirmed":         paid.orders,
			"awaiting_approval": awaiting.orders,
			"approved_unpaid":   approvedUnpaid.orders,
			"rejected":          rejected.orders,
			"expired":           expired.orders,
		},
	}
	setCachedStats(cacheKey, payload)
	c.JSON(http.StatusOK, payload)
}
