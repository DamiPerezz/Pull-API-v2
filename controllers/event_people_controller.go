package controllers

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// MobileGetEventPeople — las PERSONAS que hay detrás de cada contador del panel.
// GET /event/people/:eventId?status=…   (auth staff)
//
// El panel de control enseña números ("12 por aprobar", "40 dentro") y el staff
// necesita poder abrirlos: quién es, cómo contactarle y por dónde entra. Sin
// esto, un número es un callejón sin salida.
//
// POR QUÉ NO VALE LA LISTA DE ÓRDENES: una orden puede llevar 4 entradas a
// nombre de 4 personas distintas. Contar órdenes y contar personas da números
// diferentes, y en la puerta manda el de personas. Así que esto devuelve una
// fila POR ASISTENTE, no por compra.
//
// De dónde sale cada dato:
//   - nombre, correo y teléfono → de `orders.metadata.tickets_data`, que es lo
//     que rellenó el comprador para CADA asistente
//   - instagram → de ahí también; es opcional, así que puede venir vacío
//   - si ya entró → de `tickets.checked_in_at`, cruzado por orden
//
// El `metadata` NO sale al cliente: se lee aquí y se publica solo lo de arriba.
func MobileGetEventPeople(c *gin.Context) {
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

	// `status` mapea 1:1 con las tarjetas del panel. Dos son especiales porque
	// NO son estados de orden sino de escaneo: "dentro" y "sin escanear" salen
	// de tickets.checked_in_at, no de orders.status.
	estado := strings.TrimSpace(c.Query("status"))

	where := map[string]interface{}{"event_id": eventID}
	switch estado {
	case "", "all", "All":
		// todas las que representan una persona con sitio: pagadas, dentro y
		// retenidas esperando decisión.
		where["status"] = "in.(confirmed,checked_in,payment_authorized,awaiting_approval,approved_unpaid)"
	case "scanned", "pending_scan":
		// Personas CON entrada emitida. El corte por escaneo se aplica abajo.
		where["status"] = "in.(confirmed,checked_in)"
	default:
		if !safeLookupCode(estado) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "status inválido"})
			return
		}
		where["status"] = estado
	}

	const filaLimite = 5000
	orders, err := venueDB.QueryCtx(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,status,quantity,user_name,user_email,user_phone,metadata",
		"where":  where,
		"limit":  filaLimite,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "No se pudo leer la lista"})
		return
	}

	// Quién ya entró. Se consulta UNA vez y se indexa por orden, en vez de una
	// query por persona: en la puerta esto se abre con el móvil en la mano y
	// varios round-trips se notan.
	dentroPorOrden := map[string]int{}
	if tickets, terr := venueDB.QueryCtx(ctx, "tickets", map[string]interface{}{
		"select": "order_id,checked_in_at",
		"where":  map[string]interface{}{"event_id": eventID, "checked_in_at": "not.is.null"},
		"limit":  filaLimite,
	}); terr == nil {
		for _, t := range tickets {
			dentroPorOrden[services.GetString(t, "order_id")]++
		}
	}

	type persona struct {
		Nombre    string `json:"name"`
		Email     string `json:"email"`
		Telefono  string `json:"phone"`
		Instagram string `json:"instagram,omitempty"`
		Genero    string `json:"gender,omitempty"`
		Orden     string `json:"order_number"`
		Estado    string `json:"status"`
		Dentro    bool   `json:"checked_in"`
	}
	personas := []persona{}

	for _, o := range orders {
		orderID := services.GetString(o, "id")
		ordenNum := services.GetString(o, "order_number")
		st := services.GetString(o, "status")
		// Cuántas personas de esta orden ya entraron. No se sabe CUÁL de ellas
		// (los tickets no guardan a qué asistente del metadata corresponden),
		// así que se marcan las N primeras. Para la puerta es suficiente: lo
		// que importa es cuántos faltan, no el orden.
		yaDentro := dentroPorOrden[orderID]

		var lista []map[string]interface{}
		if md, ok := o["metadata"].(map[string]interface{}); ok {
			if raw, ok := md["tickets_data"].([]interface{}); ok {
				for _, it := range raw {
					if t, ok := it.(map[string]interface{}); ok {
						lista = append(lista, t)
					}
				}
			}
		}
		// Sin detalle por asistente (compras viejas o de grupo): al menos el
		// comprador, repetido tantas veces como entradas tenga. Mejor un nombre
		// repetido que una persona que no aparece en la lista de la puerta.
		if len(lista) == 0 {
			n := services.GetInt(o, "quantity")
			if n <= 0 {
				n = 1
			}
			for i := 0; i < n; i++ {
				lista = append(lista, map[string]interface{}{
					"owner_name":  services.GetString(o, "user_name"),
					"owner_email": services.GetString(o, "user_email"),
					"owner_phone": services.GetString(o, "user_phone"),
				})
			}
		}

		for i, t := range lista {
			nombre := strings.TrimSpace(
				services.GetString(t, "owner_name") + " " + services.GetString(t, "owner_last_name"))
			if nombre == "" {
				nombre = services.GetString(o, "user_name")
			}
			tel := strings.TrimSpace(
				services.GetString(t, "owner_phone_prefix") + " " + services.GetString(t, "owner_phone"))
			p := persona{
				Nombre:    nombre,
				Email:     services.GetString(t, "owner_email"),
				Telefono:  strings.TrimSpace(tel),
				Instagram: strings.TrimPrefix(strings.TrimSpace(services.GetString(t, "owner_instagram")), "@"),
				Genero:    services.GetString(t, "owner_gender"),
				Orden:     ordenNum,
				Estado:    st,
				Dentro:    i < yaDentro,
			}
			if p.Email == "" {
				p.Email = services.GetString(o, "user_email")
			}
			// Los dos filtros de escaneo cortan aquí, ya con el dato real.
			if estado == "scanned" && !p.Dentro {
				continue
			}
			if estado == "pending_scan" && p.Dentro {
				continue
			}
			personas = append(personas, p)
		}
	}

	// Por nombre: en la puerta se busca a la gente por como se llama.
	sort.Slice(personas, func(i, j int) bool {
		return strings.ToLower(personas[i].Nombre) < strings.ToLower(personas[j].Nombre)
	})

	c.JSON(http.StatusOK, gin.H{
		"event_id": eventID,
		"status":   estado,
		"count":    len(personas),
		"people":   personas,
	})
}
