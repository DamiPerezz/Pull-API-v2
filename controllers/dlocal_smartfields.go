package controllers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"pull-api-v2/models"
	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// =============================================================================
// SMARTFIELDS — cobro con tarjeta en NUESTRA página (checkout transparente).
//
// POR QUÉ EXISTE ESTO (2026-08-13, verificado contra producción):
// el checkout ALOJADO de dLocal no ofrece tarjeta en Guatemala. Un pago creado
// sin filtrar métodos solo muestra "Efectivo"; filtrando por CREDIT_CARD la
// lista sale VACÍA — que es la pantalla en blanco que veía el comprador.
// SmartFields NO consulta esa lista: montamos el formulario nosotros y la
// tarjeta se tokeniza contra dLocal desde el navegador. Es la ÚNICA vía por la
// que hoy se puede cobrar con tarjeta en GT.
//
// EL DINERO VA ASÍ:
//
//	1. /smartfields/session  → creamos el pago con allow_transparent y
//	   devolvemos el merchant_checkout_token + la clave PÚBLICA de SmartFields.
//	2. El navegador tokeniza la tarjeta con el SDK de dLocal.
//	   LA TARJETA NUNCA PASA POR NUESTRO SERVIDOR. Solo llega un cardToken.
//	3. /smartfields/confirm  → confirmamos en dLocal con ese cardToken y
//	   aplicamos el resultado por el MISMO carril que el webhook
//	   (applyDLocalPaymentToOrder), que ya es idempotente y emite las entradas
//	   una sola vez.
//
// El paso 3 NO sustituye al webhook: los dos pueden llegar, y el segundo se
// encuentra la orden ya confirmada y no hace nada. Esa redundancia es
// deliberada — si el navegador se cierra justo después de pagar, el webhook (o
// el reconciliador) termina el trabajo igual.
// =============================================================================

// smartFieldsRequest es el cuerpo de los dos endpoints. `payment_link_code` es
// obligatorio: es la prueba de que quien paga es quien creó la orden (flujo
// público) o quien recibió el enlace por correo (flujo privado). Sin él, un
// tercero que adivinara un UUID podría usar nuestro checkout para probar
// tarjetas robadas.
type smartFieldsRequest struct {
	OrderID         string `json:"order_id" binding:"required,uuid"`
	PaymentLinkCode string `json:"payment_link_code"`
	VenueID         string `json:"venue_id"`
	VenueSlug       string `json:"venue_slug"`

	// Solo en /session: a dónde vuelve el comprador. dLocal los exige aunque
	// en transparente no se use la redirección.
	SuccessURL string `json:"success_url"`
	BackURL    string `json:"back_url"`

	// Solo en /confirm.
	CardToken      string `json:"card_token"`
	InstallmentsID string `json:"installments_id"`
}

// checkoutTarget es la orden ya resuelta y validada, lista para cobrar.
type checkoutTarget struct {
	VenueID string
	VenueDB *services.SupabaseClient
	Order   map[string]interface{}
}

// resolveVenueForCheckout resuelve el venue igual que CreateCheckout:
// id explícito > slug > único venue activo. La web del comprador no manda
// venue_id en la página de pago.
func resolveVenueForCheckout(ctx context.Context, venueID, venueSlug string) (string, *services.SupabaseClient) {
	if venueID == "" && venueSlug != "" {
		if id, err := resolveVenueIDFromSlug(ctx, venueSlug); err == nil {
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
	return venueID, services.DB.ForVenue(venueID)
}

// orderPaymentLinkCode saca el código del metadata de la orden.
func orderPaymentLinkCode(order map[string]interface{}) string {
	if md, ok := order["metadata"].(map[string]interface{}); ok {
		return strings.TrimSpace(services.GetString(md, "payment_link_code"))
	}
	return ""
}

// loadPayableOrder carga la orden y comprueba que se puede cobrar. Escribe la
// respuesta de error y devuelve nil si algo no cuadra.
//
// Los estados pagables son los mismos que en el checkout alojado: `pending`
// (compra pública), `approved_unpaid` (solicitud privada ya aprobada) y
// `processing` (el comprador reintenta tras un rechazo, o recarga la página:
// el pago anterior sigue vivo en dLocal y NO se le puede negar el reintento).
func loadPayableOrder(c *gin.Context, ctx context.Context, req smartFieldsRequest) *checkoutTarget {
	venueID, venueDB := resolveVenueForCheckout(ctx, strings.TrimSpace(req.VenueID), strings.TrimSpace(req.VenueSlug))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return nil
	}

	order, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,event_id,ticket_type_id,user_id,quantity,total,currency,status,user_name,user_email,metadata,stripe_session_id,paid_at",
		"where":  map[string]interface{}{"id": req.OrderID},
	})
	if err != nil || order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return nil
	}

	// ANTI-CARDING: si la orden tiene código, el que pide pagar debe traerlo.
	// Sin esta comprobación el endpoint sería un tokenizador de tarjetas
	// gratuito para cualquiera que supiera un id de orden.
	if want := orderPaymentLinkCode(order); want != "" {
		got := strings.TrimSpace(req.PaymentLinkCode)
		if got == "" || !secureEqualCode(got, want) {
			log.Printf("[SmartFields] código de pago inválido order=%s", services.GetString(order, "order_number"))
			c.JSON(http.StatusForbidden, gin.H{"error": "Enlace de pago inválido o caducado"})
			return nil
		}
	}

	switch st := services.GetString(order, "status"); st {
	case "pending", "approved_unpaid", "processing", "":
		// pagable
	case "confirmed", "checked_in":
		c.JSON(http.StatusOK, gin.H{
			"success": true, "already_paid": true,
			"message":      "Esta orden ya está pagada",
			"order_number": services.GetString(order, "order_number"),
		})
		return nil
	default:
		c.JSON(http.StatusConflict, gin.H{"error": "Esta orden no se puede pagar", "status": st})
		return nil
	}

	return &checkoutTarget{VenueID: venueID, VenueDB: venueDB, Order: order}
}

// secureEqualCode compara códigos en tiempo constante-ish. Los códigos son
// cortos y de un solo uso, pero no cuesta nada no filtrar por timing.
func secureEqualCode(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// dlocalProcessorFor devuelve el procesador del venue, exigiendo que sea dLocal.
func dlocalProcessorFor(c *gin.Context, ctx context.Context, venueID string) *services.DLocalProcessor {
	processor, err := services.Payments.GetProcessor(ctx, venueID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway not configured"})
		return nil
	}
	dl, ok := processor.(*services.DLocalProcessor)
	if !ok {
		c.JSON(http.StatusConflict, gin.H{
			"error": "SmartFields solo está disponible con dLocal; este venue cobra con otra pasarela",
		})
		return nil
	}
	return dl
}

// SmartFieldsSession abre un cobro transparente.
// POST /orders/smartfields/session
//
// Devuelve al navegador lo justo para montar el formulario: el token de ESTE
// pago y la clave PÚBLICA de SmartFields. La secret key jamás sale de aquí.
func SmartFieldsSession(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	var req smartFieldsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request", "details": err.Error()})
		return
	}

	target := loadPayableOrder(c, ctx, req)
	if target == nil {
		return
	}
	dl := dlocalProcessorFor(c, ctx, target.VenueID)
	if dl == nil {
		return
	}

	apiKey := dl.SmartFieldsKey()
	if apiKey == "" {
		// Sin la clave pública el SDK no arranca y el comprador vería un hueco.
		// Mejor decirlo claro que servir un formulario que no va a funcionar.
		log.Printf("[SmartFields] ALERT venue=%s SIN smartfields_key — no se puede cobrar con tarjeta", target.VenueID)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Falta la clave de SmartFields para este venue (smartfields_key)",
		})
		return
	}

	order := target.Order

	// GUARDIA ANTES DE ABRIR OTRO COBRO.
	//
	// Cada /session crea un pago nuevo en dLocal y pisa `stripe_session_id`.
	// Si el pago ANTERIOR seguía vivo, se queda huérfano: nadie vuelve a
	// mirarlo (el reconciliador solo conoce el último id). Y si ese pago
	// huérfano ya estaba PAGADO, el comprador pagó y se queda sin entrada.
	// Pasa con algo tan tonto como recargar la página de pago.
	//
	// Así que antes de crear nada, se le pregunta a dLocal por el pago previo.
	if prevID := services.DLocalPaymentIDFromSession(services.GetString(order, "stripe_session_id")); prevID != "" {
		if prev, perr := dlocalGetPayment(ctx, target.VenueID, prevID); perr == nil && prev != nil {
			switch {
			case prev.IsPaid():
				// Ya estaba pagado: se emiten las entradas por el carril de
				// siempre y NO se abre otro cobro. Contexto despegado: aquí ya
				// hay dinero de por medio.
				bgCtx, bgCancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer bgCancel()
				outcome := applyDLocalPaymentToOrder(bgCtx, target.VenueID, target.VenueDB, order, prev)
				log.Printf("[SmartFields] order=%s ya tenía el pago %s PAGADO → %s (no se abre otro cobro)",
					services.GetString(order, "order_number"), prevID, outcome)
				c.JSON(http.StatusOK, gin.H{
					"success": true, "already_paid": true,
					"message":      "Esta orden ya está pagada. Te enviamos las entradas por correo.",
					"order_number": services.GetString(order, "order_number"),
				})
				return

			case prev.Status == services.DLocalStatusPending:
				// Sigue vivo: se REUTILIZA en vez de abrir otro. Así no se
				// abandona un cobro que el comprador podría completar, ni se
				// deja a dLocal con dos pagos abiertos por la misma entrada.
				if tok := prevCheckoutToken(order); tok != "" {
					log.Printf("[SmartFields] order=%s reutiliza el cobro %s (sigue PENDING)",
						services.GetString(order, "order_number"), prevID)
					c.JSON(http.StatusOK, gin.H{
						"checkout_token": tok,
						"api_key":        apiKey,
						"payment_id":     prevID,
						"session_id":     services.DLocalSessionID(prevID),
						"amount":         services.GetFloat64(order, "total"),
						"currency":       services.GetString(order, "currency"),
						"country":        services.DLocalCountry(),
						"order_number":   services.GetString(order, "order_number"),
						"reused":         true,
					})
					return
				}
			}
			// REJECTED/CANCELLED/EXPIRED: el intento anterior está muerto,
			// se puede abrir otro sin dejar nada colgando.
		} else if perr != nil {
			// No se pudo preguntar. NO se abre otro cobro a ciegas: si el
			// anterior estuviera pagado, crearíamos un segundo cobro sobre una
			// entrada ya comprada. Mejor pedirle que reintente en un momento.
			log.Printf("[SmartFields] ALERT no se pudo consultar el pago previo %s de order=%s: %v — no se abre otro cobro",
				prevID, services.GetString(order, "order_number"), perr)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "No podemos verificar el estado de tu pago anterior. Inténtalo de nuevo en unos segundos.",
			})
			return
		}
	}

	ticketTypeName := "Entradas"
	if tt, _ := target.VenueDB.QueryOne(ctx, "ticket_types", map[string]interface{}{
		"select": "name", "where": map[string]interface{}{"id": services.GetString(order, "ticket_type_id")},
	}); tt != nil {
		if n := services.GetString(tt, "name"); n != "" {
			ticketTypeName = n
		}
	}

	checkout, err := dl.CreateCheckout(ctx, models.CheckoutParams{
		Amount:        services.GetFloat64(order, "total"),
		Currency:      services.GetString(order, "currency"),
		OrderID:       services.GetString(order, "id"),
		ProductName:   fmt.Sprintf("%d x %s", services.GetInt(order, "quantity"), ticketTypeName),
		CustomerEmail: services.GetString(order, "user_email"),
		CustomerName:  services.GetString(order, "user_name"),
		SuccessURL:    strings.TrimSpace(req.SuccessURL),
		CancelURL:     strings.TrimSpace(req.BackURL),
		Transparent:   true,
		Metadata: map[string]string{
			"venue_id":     target.VenueID,
			"order_id":     services.GetString(order, "id"),
			"event_id":     services.GetString(order, "event_id"),
			"order_number": services.GetString(order, "order_number"),
		},
	})
	if err != nil {
		log.Printf("[SmartFields] no se pudo abrir el cobro order=%s: %v",
			services.GetString(order, "order_number"), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "No se pudo iniciar el pago", "details": err.Error()})
		return
	}

	// Se marca `processing` con el reloj del intento, igual que el checkout
	// alojado: es lo que mira el reconciliador para saber si un pago se quedó
	// a medias. Síncrono a propósito — si esto no se guarda y el comprador
	// paga, el webhook llegaría antes de que la orden supiera su session id.
	meta := map[string]interface{}{}
	if md, ok := order["metadata"].(map[string]interface{}); ok {
		for k, v := range md {
			meta[k] = v
		}
	}
	meta["checkout_started_at"] = time.Now().Format(time.RFC3339)
	meta["checkout_mode"] = "smartfields"
	// OJO: el merchant_checkout_token NO es el payment_id — son valores
	// distintos (payment_id "DP-7904234" vs token "3YCx8RXz..."), verificado
	// contra la API. `stripe_session_id` guarda el payment_id, así que el token
	// hay que guardarlo aparte: es lo ÚNICO que sirve para
	// POST /v1/payments/confirm/{token}. Reconstruirlo desde el session_id
	// haría fallar TODAS las confirmaciones.
	meta["dlocal_checkout_token"] = checkout.MerchantCheckoutToken
	if _, uerr := target.VenueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
		"status":            "processing",
		"stripe_session_id": checkout.SessionID,
		"payment_gateway":   models.GatewayDLocal.String(),
		"metadata":          meta,
	}, map[string]interface{}{"id": services.GetString(order, "id")}); uerr != nil {
		log.Printf("[SmartFields] ALERT no se pudo marcar processing order=%s: %v — se sigue, el webhook reconcilia",
			services.GetString(order, "order_number"), uerr)
	}

	log.Printf("[SmartFields] sesión abierta order=%s pago=%s",
		services.GetString(order, "order_number"), checkout.PaymentID)

	c.JSON(http.StatusOK, gin.H{
		"checkout_token": checkout.MerchantCheckoutToken,
		"api_key":        apiKey,
		"payment_id":     checkout.PaymentID,
		"session_id":     checkout.SessionID,
		"amount":         services.GetFloat64(order, "total"),
		"currency":       services.GetString(order, "currency"),
		"country":        services.DLocalCountry(),
		"order_number":   services.GetString(order, "order_number"),
		// El SDK necesita el nombre del titular al tokenizar
		// (createCardToken(field, {name})). Sin él, dLocal acepta la
		// tokenización pero luego rechaza el cobro con "Missing payment
		// method". Se manda desde aquí y no desde el navegador para que sea
		// SIEMPRE el de la orden.
		"client_name": services.GetString(order, "user_name"),
	})
}

// SmartFieldsConfirm cierra el cobro con el token de tarjeta del navegador.
// POST /orders/smartfields/confirm
//
// Responder 200 NO significa "pagado": hay que mirar `paid`. Un rechazo del
// banco es una respuesta legítima (paid=false + motivo), no un error del
// servidor.
func SmartFieldsConfirm(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	var req smartFieldsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request", "details": err.Error()})
		return
	}
	if strings.TrimSpace(req.CardToken) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Falta el token de la tarjeta"})
		return
	}

	target := loadPayableOrder(c, ctx, req)
	if target == nil {
		return
	}
	dl := dlocalProcessorFor(c, ctx, target.VenueID)
	if dl == nil {
		return
	}

	order := target.Order
	orderNumber := services.GetString(order, "order_number")

	// El merchant_checkout_token lo guardó /session en el metadata. NO se puede
	// derivar de `stripe_session_id`: ahí vive el payment_id, que es OTRO valor
	// (ver el comentario en SmartFieldsSession). Confundirlos hace que dLocal
	// rechace todas las confirmaciones.
	checkoutToken := ""
	if md, ok := order["metadata"].(map[string]interface{}); ok {
		checkoutToken = strings.TrimSpace(services.GetString(md, "dlocal_checkout_token"))
	}
	if checkoutToken == "" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "Esta orden no tiene un cobro abierto. Vuelve a empezar el pago.",
		})
		return
	}

	// Datos del comprador para el antifraude del emisor. Salen de la orden, no
	// del navegador: así nadie puede mandar los de otro.
	firstName, lastName := splitFullName(services.GetString(order, "user_name"))
	payment, err := dl.ConfirmCardToken(ctx, checkoutToken, services.DLocalConfirmRequest{
		CardToken:       strings.TrimSpace(req.CardToken),
		ClientFirstName: firstName,
		ClientLastName:  lastName,
		ClientEmail:     services.GetString(order, "user_email"),
		InstallmentsID:  strings.TrimSpace(req.InstallmentsID),
	})
	if err != nil {
		// NO se sabe si hubo cobro: puede haber fallado la red DESPUÉS de que
		// dLocal cobrara. Jamás se declara fallo aquí — se deja que el webhook
		// y el reconciliador digan la última palabra.
		log.Printf("[SmartFields] ALERT confirmación indeterminada order=%s: %v — lo resolverá el webhook/reconciliador",
			orderNumber, err)
		c.JSON(http.StatusAccepted, gin.H{
			"success":       false,
			"paid":          false,
			"indeterminate": true,
			"message":       "No hemos podido confirmar el pago en este momento. Si se te ha cobrado, recibirás tus entradas por correo en unos minutos.",
			"order_number":  orderNumber,
		})
		return
	}

	// CONTEXTO DESPEGADO DE LA REQUEST — imprescindible, no es cosmética.
	//
	// A partir de aquí la tarjeta YA está cobrada. Si siguiéramos con el
	// contexto de la petición, bastaría con que el comprador cerrara la
	// pestaña (Go cancela el contexto al desconectarse el cliente) para que
	// muriera a media emisión: la orden ya habría hecho el flip atómico a
	// `confirmed` y el INSERT de las entradas fallaría. Quedaría status
	// confirmed, paid_at NULL, CERO entradas y el dinero cobrado — y nadie lo
	// repararía: el reconciliador solo barre `processing` y el webhook, al ver
	// `confirmed`, se retira sin tocar nada.
	//
	// Misma precaución (y mismo motivo) que en GetOrderPaymentStatus.
	bgCtx, bgCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer bgCancel()

	// MISMO carril que el webhook: idempotente, emite entradas una sola vez y
	// libera aforo si el intento muere. No se duplica nada de esa lógica aquí.
	outcome := applyDLocalPaymentToOrder(bgCtx, target.VenueID, target.VenueDB, order, payment)
	paid := payment.IsPaid()

	log.Printf("[SmartFields] confirmado order=%s pago=%s estado=%s → %s",
		orderNumber, payment.PaymentID(), payment.Status, outcome)

	status := http.StatusOK
	message := "Pago aprobado. Te enviamos las entradas por correo."
	if !paid {
		// 200 igualmente: la petición se procesó bien, lo que no cuajó es el
		// cobro. La web distingue por `paid`, no por el código HTTP.
		message = "El pago no se completó."
		if r := strings.TrimSpace(payment.RejectedReason); r != "" {
			message = "El pago fue rechazado: " + r
		} else if payment.Status == services.DLocalStatusPending {
			message = "El pago está pendiente de confirmación. Si se completa, recibirás tus entradas por correo."
		}
	}

	c.JSON(status, gin.H{
		"success":      paid,
		"paid":         paid,
		"status":       payment.Status,
		"payment_id":   payment.PaymentID(),
		"order_number": orderNumber,
		"message":      message,
	})
}

// prevCheckoutToken devuelve el merchant_checkout_token del intento anterior,
// si /session llegó a guardarlo. Es lo único con lo que se puede reutilizar un
// cobro que sigue PENDING (el payment_id NO sirve para confirmar).
func prevCheckoutToken(order map[string]interface{}) string {
	if md, ok := order["metadata"].(map[string]interface{}); ok {
		return strings.TrimSpace(services.GetString(md, "dlocal_checkout_token"))
	}
	return ""
}

// splitFullName parte "Damián Pérez García" en nombre y apellidos. dLocal los
// pide por separado. Si solo hay una palabra, va toda como nombre: mandar un
// apellido inventado sería peor para el antifraude que no mandar ninguno.
func splitFullName(full string) (string, string) {
	parts := strings.Fields(strings.TrimSpace(full))
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.Join(parts[1:], " ")
	}
}
