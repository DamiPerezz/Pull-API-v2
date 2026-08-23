package controllers

import (
	"context"
	"log"
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
// (`payment_authorized`) y solo se cobra al aprobar. Por eso "cobrado" son
// SOLO las órdenes ya cobradas —`confirmed` y `checked_in`— y lo retenido va
// aparte: es dinero comprometido que TODAVÍA no ha entrado y que se libera
// solo si nadie decide en 48 h.
//
// Y dos cifras distintas del mismo cobro, que no hay que confundir:
//
//	collected      lo que se le cobró a la TARJETA (lleva dentro el 8% de Pull)
//	collected_net  lo que se lleva EL LOCAL — es la que ve la dueña
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
		total    float64 // lo que se le cobró a la tarjeta (subtotal + fee)
		subtotal float64 // lo que es del LOCAL, sin la comisión de Pull
		people   int     // suma de quantity = personas
		orders   int
	}
	buckets := map[string]*orderBucket{}
	rows, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
		// subtotal viaja además de total: total lleva DENTRO la comisión de
		// Pull (legacy_compat_controller.go monta total = subtotal * 1,08).
		// Enseñarle el bruto a la dueña como "cobrado" le infla el ingreso un
		// 8% justo en la cifra con la que calcula su margen.
		"select": "status,total,subtotal,quantity,currency",
		"where": map[string]interface{}{
			"event_id": eventID,
			// ⚠️ checked_in TIENE que estar aquí. No es un estado aparte: es
			// una orden CONFIRMADA cuya última entrada ya se escaneó en la
			// puerta (mobile_compat_controller.go la mueve al escanear).
			//
			// Sin él pasaba esto: la noche del evento, según la gente entraba,
			// "Cobrado" iba BAJANDO. Con todo el mundo dentro, el panel de la
			// dueña marcaba Cobrado Q0,00 y aforo vacío, mientras "ya están
			// dentro" subía. La pantalla se contradecía a sí misma y el
			// histórico del evento quedaba falseado para siempre.
			//
			// La consulta hermana de personas (event_people_controller.go) SÍ
			// lo incluía desde el principio. Se olvidó solo aquí.
			"status": "in.(confirmed,checked_in,payment_authorized,awaiting_approval,approved_unpaid,cancelled,expired)",
		},
		"limit": statsRowLimit,
	})
	// ANTES ESTE ERROR SE TIRABA (`rows, _ :=`). Si la consulta fallaba, rows
	// quedaba vacío, todos los contadores a cero, y se respondía HTTP 200 con
	// la mentira — cacheada 20 s, así que pulsar "Actualizar" la repetía.
	//
	// Para la dueña, un cero por avería y un cero real se veían EXACTAMENTE
	// igual: "0 de 300 entradas (0%)" seis horas antes de abrir puertas. Puede
	// dar por muerto un evento que está vendiendo. Mejor un error honesto que
	// un cero falso.
	if err != nil {
		log.Printf("[EventStats] no se pudieron leer las órdenes del evento %s: %v", eventID, err)
		c.JSON(http.StatusBadGateway, gin.H{
			"error": "No se pudieron cargar las cifras de este evento. Vuelve a intentarlo.",
		})
		return
	}
	for _, r := range rows {
		status := services.GetString(r, "status")
		b := buckets[status]
		if b == nil {
			b = &orderBucket{}
			buckets[status] = b
		}
		b.total += services.GetFloat64(r, "total")
		// Si por lo que sea no hay subtotal guardado, se cae al total: es
		// preferible pasarse (enseñarle de más) a que aparezca un cero que
		// parezca una venta perdida.
		if sub := services.GetFloat64(r, "subtotal"); sub > 0 {
			b.subtotal += sub
		} else {
			b.subtotal += services.GetFloat64(r, "total")
		}
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
	// Pagadas = dinero YA en caja. SON LOS DOS ESTADOS: confirmed es "pagó y
	// no ha llegado", checked_in es "pagó y ya está dentro". El dinero es el
	// mismo; lo único que cambia es que alguien escaneó su QR en la puerta.
	sumaDe := func(estados ...string) orderBucket {
		var b orderBucket
		for _, estado := range estados {
			parcial := bucketOf(estado)
			b.total += parcial.total
			b.subtotal += parcial.subtotal
			b.people += parcial.people
			b.orders += parcial.orders
		}
		return b
	}
	paid := sumaDe("confirmed", "checked_in")
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
			// Cobrado de verdad: `confirmed` + `checked_in`. Retener ≠ cobrar,
			// pero escanear en la puerta tampoco descobra nada.
			// Es el BRUTO — lleva dentro la comisión. Ver collected_net.
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

			// --- LO QUE DE VERDAD SE LLEVA EL LOCAL -----------------------
			// `collected` es el BRUTO: lo que se le cobró a la tarjeta, con la
			// comisión de Pull dentro. Se deja como estaba porque las builds
			// móviles ya publicadas leen esa clave y cambiarla les movería los
			// números sin avisar.
			//
			// Estas dos son nuevas y son las honestas: net es el dinero de la
			// dueña, fee es lo que se queda Pull. La diferencia es el 7,4074%
			// del cobro (8% sobre la base), que en un mes de Q40.000 son unos
			// Q2.963 que ella creía tener.
			"collected_net":          round2(paid.subtotal),
			"collected_platform_fee": round2(paid.total - paid.subtotal),
			"pending_capture_net":    round2(held.subtotal),
			"currency":               currency,
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
