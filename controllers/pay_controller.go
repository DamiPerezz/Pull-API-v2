package controllers

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"pull-api-v2/middleware"
	"pull-api-v2/models"
	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// =============================================
// DIRECT CARD PAYMENT (NeoNet/Cybersource)
// POST /api/v1/orders/pay
//
// Acordado con NeoNet: cada pago del comprador son DOS transacciones
// atómicas — la parte del venue (precio de las entradas) y el fee de
// servicio de Pull. Si la segunda falla, la primera se reversa y la orden
// queda sin cobrar. Solo con ambas aprobadas se emiten los tickets, por el
// mismo carril compartido de ConfirmPayment.
//
// En DEMO_MODE el MockProcessor implementa el mismo contrato, así que este
// flujo se puede probar end-to-end sin credenciales reales.
// =============================================

type payOrderRequest struct {
	OrderID string `json:"order_id"`
	// Anti-carding: el código que create-pending-order devolvió al crear la
	// orden. Sin él (o sin coincidir) no se toca la pasarela.
	PaymentLinkCode string `json:"payment_link_code"`
	TurnstileToken  string `json:"turnstile_token"`
	VenueID         string `json:"venue_id"`
	VenueSlug       string `json:"venue_slug"`
	Card            struct {
		Number   string `json:"number"`
		ExpMonth string `json:"exp_month"`
		ExpYear  string `json:"exp_year"`
		CVV      string `json:"cvv"`
	} `json:"card"`
	// ALTERNATIVA a `card` (Unified Checkout: Apple Pay / Google Pay). Es el
	// "transient token" de un solo uso que devuelve el SDK en el navegador.
	// Cuando viene informado, `card` se ignora por completo — en ese flujo el
	// PAN no llega nunca al servidor. Ver unified_checkout_controller.go.
	TransientToken string `json:"transient_token"`
	// DeviceFingerprintID (OPCIONAL) es el identificador de la huella de
	// dispositivo que haya generado el NAVEGADOR. Es el único campo de las
	// señales de dispositivo que no podemos observar nosotros: la IP y las
	// cabeceras las leemos de la propia petición, pero la huella la produce el
	// script de profiling de Cybersource dentro de la página.
	//
	// CONTRATO CON LA WEB — cuando esté disponible:
	//	1. La web carga el script de profiling con el `org_id` del comercio y un
	//	   `session_id` que ELLA genera (un UUID por intento de pago).
	//	2. Manda ese MISMO valor aquí, en `device_fingerprint_id`.
	//	3. Aquí se valida la FORMA (LooksLikeFingerprintSessionID) y se reenvía
	//	   como deviceInformation.fingerprintSessionId.
	//
	// HOY LA WEB NO LO MANDA, y es correcto que no lo haga: NeoNet todavía no nos
	// ha dado el `org_id`, y sin él el script no perfila nada — el id viajaría
	// apuntando a una huella inexistente. El campo se deja definido y validado
	// para que ese día sea un cambio de una línea en la web y ninguno aquí.
	//
	// El carril de wallet no lo necesita: con completeMandate.decisionManager
	// encendido, Unified Checkout perfila el dispositivo él mismo y mete la
	// huella dentro del transient token.
	DeviceFingerprintID string `json:"device_fingerprint_id"`
	BillTo              struct {
		Address1   string `json:"address1"`
		City       string `json:"city"`
		State      string `json:"state"`
		PostalCode string `json:"postal_code"`
		Country    string `json:"country"`
	} `json:"bill_to"`
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// deviceSignalsEnabled dice si el cobro lleva señales de dispositivo (IP,
// User-Agent, huella) para Decision Manager. ENCENDIDO por defecto: no mandarlas
// es lo que dejó al antifraude decidiendo a ciegas.
//
// Apagar SOLO como marcha atrás de emergencia:
//
//	flyctl secrets set PAYMENT_DEVICE_SIGNALS=false -a pull-api-v2-prod
//
// Con eso el cuerpo que se le manda a la pasarela vuelve a ser exactamente el
// de antes de este cambio.
func deviceSignalsEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("PAYMENT_DEVICE_SIGNALS")), "false")
}

// PayOrder charges a pending order: venue share + service fee, atomically.
func PayOrder(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	var req payOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.OrderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id is required"})
		return
	}
	// SECURITY: block PostgREST operator injection via order_id.
	if !safeLookupCode(req.OrderID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid order_id"})
		return
	}

	// WALLET (Unified Checkout) vs TARJETA. Son excluyentes: si llega token, la
	// tarjeta ni se mira. Con el interruptor apagado el token se rechaza y el
	// handler se comporta exactamente igual que antes de que esto existiera.
	transientToken := strings.TrimSpace(req.TransientToken)
	if transientToken != "" {
		if !unifiedCheckoutEnabled() {
			c.JSON(http.StatusNotImplemented, gin.H{
				"error":   "Unified Checkout no está habilitado en este entorno.",
				"enabled": false,
			})
			return
		}
		// El token es un JWT emitido por Cybersource. Comprobar la FORMA antes
		// de reenviarlo evita usar la pasarela como eco de cualquier cadena que
		// nos manden.
		if !services.LooksLikeJWT(transientToken) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Token de pago inválido"})
			return
		}
	}

	// Never log or echo card data anywhere in this handler.
	card := services.CybsCard{
		Number:       strings.ReplaceAll(req.Card.Number, " ", ""),
		ExpMonth:     req.Card.ExpMonth,
		ExpYear:      req.Card.ExpYear,
		SecurityCode: req.Card.CVV,
	}
	if transientToken == "" {
		// --- CARRIL DE TARJETA ---
		if len(card.Number) < 12 || card.ExpMonth == "" || card.ExpYear == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Datos de tarjeta incompletos"})
			return
		}
		// EL CVV ES OBLIGATORIO AQUÍ. Añadido el 2026-08-22.
		//
		// Antes NO se comprobaba. La web no deja pulsar "Pagar" sin CVV, pero
		// eso es validación de navegador: un atacante hace POST directo a
		// /orders/pay. Con el campo vacío, `Sale()` montaba
		// `securityCode: ""` y la petición salía igual a la pasarela.
		//
		// Y aquí está lo importante: lo único que hoy impedía que eso acabara
		// en carding es LA REGLA "CVN no enviado" DEL PERFIL DE NEONET — la
		// misma que estamos pidiendo que relajen para que funcionen los
		// wallets. Su regla nos estaba tapando un agujero nuestro.
		//
		// Con esto arreglado, la petición se invierte y deja de ser peligrosa:
		// si ningún pago con tarjeta puede salir ya sin CVV, esa regla solo
		// puede dispararse en el carril de wallet —el único que estructuralmente
		// no tiene ese dato— y relajarla no abre nada.
		//
		// 3 dígitos, o 4 en American Express.
		cvv := strings.TrimSpace(card.SecurityCode)
		if len(cvv) < 3 || len(cvv) > 4 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Falta el código de seguridad de la tarjeta"})
			return
		}
		for _, r := range cvv {
			if r < '0' || r > '9' {
				c.JSON(http.StatusBadRequest, gin.H{"error": "El código de seguridad solo puede tener números"})
				return
			}
		}
		card.SecurityCode = cvv
		if len(card.ExpYear) == 2 {
			card.ExpYear = "20" + card.ExpYear
		}
	} else {
		// Con wallet no hay tarjeta que mandar: se vacía por si el navegador
		// mandó ambos campos, para que no viaje un PAN a la pasarela junto al
		// token (Cybersource rechazaría la petición) ni quede en memoria.
		card = services.CybsCard{}
	}

	// Etiqueta para los logs de intentos. Con tarjeta es el hash del PAN; con
	// wallet no hay PAN, y publicar el hash de la cadena vacía (que sería el
	// mismo para todos) solo confundiría al leer los logs.
	attemptLabel := cardAttemptKey(card.Number)
	if transientToken != "" {
		attemptLabel = "wallet"
	}

	// Anti-carding: CAPTCHA (si está activado) y límite por tarjeta, antes de
	// gastar queries o tocar la pasarela.
	if err := verifyTurnstile(ctx, req.TurnstileToken, c.ClientIP()); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Verificación de seguridad fallida. Recarga la página."})
		return
	}
	// El tope por HASH DE PAN no aplica al wallet: no hay PAN que hashear, y
	// aplicarlo sobre la cadena vacía metería TODOS los pagos con wallet en el
	// mismo cubo, tumbando compradores legítimos en cuanto hubiera 10 seguidos.
	// El tope POR ORDEN (más abajo) sí sigue aplicando, que es el que de verdad
	// impide usar esto como oráculo de tarjetas. Además, con wallet el medio de
	// pago lo autentica Apple/Google: no se puede iterar sobre tarjetas ajenas.
	if transientToken == "" {
		if !allowCardAttempt(cardAttemptKey(card.Number)) {
			log.Printf("[PayOrder] card attempt limit hit cardkey=%s", cardAttemptKey(card.Number))
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Demasiados intentos con esta tarjeta. Espera unos minutos."})
			return
		}
	}

	// Resolve venue: explicit id > slug > first active (single-venue deploys).
	venueID := req.VenueID
	if venueID == "" && req.VenueSlug != "" {
		if id, err := resolveVenueIDFromSlug(ctx, req.VenueSlug); err == nil {
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
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	order, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,status,total,currency,quantity,ticket_type_id,event_id,user_name,user_email,metadata",
		"where":  map[string]interface{}{"id": req.OrderID},
	})
	if err != nil || order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	// PUBLIC vs PRIVATE: a private event (is_private or require_approval on the
	// event) needs staff approval before charging. For those we AUTHORIZE only
	// (hold the funds) and settle on approval / reverse on rejection. Public
	// events capture immediately.
	//
	// ⚠️ orderRequiresApproval() (unified_checkout_controller.go) hace ESTA
	// MISMA lectura para declarar el completeMandate de la sesión de wallet
	// (CAPTURE en público, AUTH en privado). Las dos tienen que decir lo mismo:
	// si divergen, la sesión y el cobro estarían hablando de operaciones
	// distintas sobre el mismo dinero. Si cambian aquí las columnas que definen
	// "privado", hay que cambiarlas allí también.
	needsApproval := false
	if eventID := services.GetString(order, "event_id"); eventID != "" {
		if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "is_private,require_approval",
			"where":  map[string]interface{}{"id": eventID},
		}); ev != nil {
			needsApproval = services.GetBool(ev, "is_private") || services.GetBool(ev, "require_approval")
		}
	}

	// WALLET + EVENTO PRIVADO = SÍ (agosto 2026). Aquí había un rechazo: el
	// wallet solo se aceptaba en eventos de compra directa, porque no estaba
	// confirmado que una retención de 48 h se pudiera capturar sobre un pago
	// con Apple/Google Pay. Ya lo está:
	//
	//	· La doc de NeoNet (Payments Developer Guide — REST API, Visa Platform
	//	  Connect, pág. 36) da completeMandate.type = AUTH, "Authorize the
	//	  payment and capture the funds at a later date", y la pág. 31 el
	//	  ejemplo de autorización CON transient token.
	//	· La retención de 48 h ya está probada contra NeoNet y bancos
	//	  guatemaltecos por el carril de tarjeta.
	//	· Apple Pay y Google Pay son tarjetas (pág. 84): mismo carril, mismos
	//	  parámetros, solo cambia que el PAN llega tokenizado.
	//
	// El `capture` de abajo sigue saliendo del EVENTO, no del medio de pago:
	// el token sustituye a la tarjeta dentro de los mismos parámetros. Y lo que
	// la pasarela conteste NO se da por bueno — ver el guard de verificación
	// después del cobro.

	status := services.GetString(order, "status")
	if status == "confirmed" {
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Order already confirmed",
			"order_number": services.GetString(order, "order_number"), "order_id": req.OrderID})
		return
	}
	// 'processing' significa que OTRO request ya está cobrando esta orden (o
	// ya la cobró y espera confirmación). NO recobrar — evita el doble cargo
	// por doble-click o reintento de red.
	if status == "processing" {
		c.JSON(http.StatusConflict, gin.H{"error": "Este pago ya se está procesando. No vuelvas a pagar; revisa tu correo en unos minutos."})
		return
	}
	if status != "pending" && status != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "Order is not payable", "status": status})
		return
	}

	// Anti-carding: solo quien tiene el payment_link_code de ESTA orden puede
	// intentar cobrarla, y cada orden aguanta un número finito de declinadas.
	orderMeta, _ := order["metadata"].(map[string]interface{})
	if orderMeta == nil {
		orderMeta = map[string]interface{}{}
	}
	if !matchPaymentLinkCode(orderMeta, req.PaymentLinkCode) {
		log.Printf("[PayOrder] payment_link_code mismatch order=%s", req.OrderID)
		c.JSON(http.StatusForbidden, gin.H{"error": "Código de pago inválido para esta orden."})
		return
	}
	// TOPE POR ORDEN. Aplica igual con tarjeta que con wallet: se cuenta sobre
	// la orden, no sobre el medio de pago. Es el único tope que le queda al
	// carril de wallet (el de hash de PAN no puede aplicarse: no hay PAN), y el
	// que de verdad impide usar esto como oráculo de tarjetas. El contador lo
	// incrementa recordDeclinedAttempt, que se llama en TODA declinada, venga
	// del carril que venga.
	priorAttempts := services.GetInt(orderMeta, "payment_attempts")
	if priorAttempts >= maxAttemptsPerOrder {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Demasiados intentos de pago para esta orden. Crea una nueva."})
		return
	}
	// recordDeclinedAttempt persiste el contador ANTES de responder al
	// cliente, para que el límite no se pueda esquivar con reintentos rápidos.
	// Al declinar: incrementa el contador Y devuelve la orden a 'pending'
	// (liberando el claim atómico) para que el comprador pueda reintentar con
	// otra tarjeta.
	recordDeclinedAttempt := func() {
		priorAttempts++
		orderMeta["payment_attempts"] = priorAttempts
		venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
			"metadata": orderMeta,
			"status":   "pending",
		}, map[string]interface{}{"id": req.OrderID})
		log.Printf("[PayOrder] DECLINED order=%s attempts=%d cardkey=%s",
			services.GetString(order, "order_number"), priorAttempts, attemptLabel)
	}

	total := services.GetFloat64(order, "total")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	if total <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Order total is invalid"})
		return
	}

	// UN SOLO COBRO (18-ago-2026). Antes se hacían DOS cargos a la misma tarjeta
	// —uno por la entrada y otro por la comisión del 8%— y el comprador veía dos
	// apuntes en su extracto. Ahora se cobra (o se retiene) el total de una vez.
	//
	// El 8% NO desaparece: lo sigue pagando el comprador, porque el total ya se
	// creó como subtotal * (1 + fee). Lo que cambia es que deja de viajar en una
	// transacción aparte — entra entero por la cuenta del venue y Pull liquida su
	// parte por fuera. Aquí el porcentaje solo se calcula para DEJARLO ANOTADO en
	// la orden, no para cobrarlo por separado.
	//
	// Además de simplificar el extracto, esto elimina la pieza más frágil que
	// tenía este handler: el rollback de "si el segundo cargo falla, deshaz el
	// primero", que en público exigía un reembolso (la venta ya estaba liquidada)
	// y en privado una reversa.
	feePercent := 0.0
	if venue, err := services.DB.GetVenue(ctx, venueID); err == nil && venue.PlatformFeePercent > 0 {
		feePercent = venue.PlatformFeePercent
	}
	// Lo que se le pide a la tarjeta, sin partir.
	chargeAmount := total
	// Desglose contable, informativo: feeIncluded va DENTRO de chargeAmount.
	feeIncluded := 0.0
	if feePercent > 0 {
		feeIncluded = round2(total - round2(total/(1+feePercent/100)))
	}

	processor, err := services.Payments.GetProcessor(ctx, venueID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway not configured"})
		return
	}
	charger, ok := processor.(services.DirectCardCharger)
	if !ok {
		// dLocal Go no acepta el PAN: sus cobros van por checkout alojado
		// (POST /orders/checkout → redirect_url). Si la web sigue mandando la
		// tarjeta aquí, el mensaje tiene que decir exactamente qué hacer, no un
		// "no soportado" genérico que cueste media hora de depuración.
		detail := "esta pasarela solo cobra por checkout alojado"
		if processor.GetGateway() == models.GatewayDLocal {
			detail = services.DLocalRawCardError().Error()
		}
		log.Printf("[PayOrder] gateway=%s NO soporta tarjeta cruda order=%s — la web debe usar el checkout alojado",
			processor.GetGateway(), req.OrderID)
		c.JSON(http.StatusNotImplemented, gin.H{
			"error":               "Esta pasarela no acepta los datos de la tarjeta en nuestra web: el pago se hace en la página segura de la pasarela.",
			"gateway":             processor.GetGateway().String(),
			"use_hosted_checkout": true,
			"detail":              detail,
		})
		return
	}

	// (Aquí vivía la resolución de la SEGUNDA cuenta merchant, la de Pull, para
	// cobrar el fee aparte. Ya no hace falta: hay un único cobro por la cuenta del
	// venue. Las capturas y reversas de órdenes ANTIGUAS que sí tienen dos
	// transacciones siguen funcionando — `feeChargerForSplit` resuelve su cuenta
	// leyendo `fee_gateway_venue` del propio split guardado en la orden.)

	// Billing info: sensible Guatemala defaults when the form doesn't ask.
	userName := services.GetString(order, "user_name")
	firstName, lastName := userName, "."
	if parts := strings.SplitN(userName, " ", 2); len(parts) == 2 {
		firstName, lastName = parts[0], parts[1]
	}
	// PAÍS DEL COMPRADOR, deducido de su propio prefijo telefónico.
	//
	// Antes aquí había "GT" fijo para TODO el mundo. Para un comprador
	// guatemalteco eso acierta —y sigue aciertando, porque +502 da GT— pero
	// para cualquier otro le estábamos jurando a Cybersource un país que no era
	// el suyo. Y eso no es inocuo: en el rechazo del 22-ago saltaron
	// `MM-BIN: Card BIN inconsistent with country` y `MM-EMBCO: Email address
	// inconsistent with billing country`. Los provocábamos nosotros, diciendo
	// "este señor es de Guatemala" sobre una tarjeta española.
	//
	// El prefijo no es la dirección de facturación de la tarjeta, y por eso no
	// es perfecto. Pero es un dato REAL que el comprador nos dio, y sustituye a
	// una constante inventada: para el +502 el resultado es idéntico al de
	// antes, y solo cambia donde antes fallábamos seguro.
	pais := orDefault(req.BillTo.Country, paisDelComprador(order))

	billTo := services.CybsBillTo{
		FirstName: firstName,
		LastName:  lastName,
		Email:     services.GetString(order, "user_email"),
		Phone:     telefonoDelComprador(order),
		Country:   pais,
	}
	// ¿NOS LA VA A DAR EL WIDGET? Si el pago viene por Unified Checkout Y la
	// sesión se abrió pidiendo datos de facturación (billingType PARTIAL/FULL),
	// entonces el comprador —o su wallet, que la lleva guardada con la
	// tarjeta— ya entregó su dirección REAL dentro del transient token.
	//
	// En ese caso NO mandamos la nuestra. Si la mandásemos, la inventada
	// pisaría a la verdadera y habríamos añadido un paso al checkout para
	// tirar el dato a la basura. La guía lo dice al revés y con las mismas
	// palabras: "cuando billingType es NONE, TÚ tienes que incluir los campos
	// requeridos" — o sea que cuando no es NONE, los pone el widget.
	//
	// Solo aplica al carril del widget. El formulario de tarjeta de respaldo no
	// recoge dirección de nadie, así que ahí se sigue rellenando como siempre.
	laDaElWidget := transientToken != "" &&
		!strings.EqualFold(strings.TrimSpace(os.Getenv("UNIFIED_CHECKOUT_BILLING_TYPE")), "NONE") &&
		strings.TrimSpace(os.Getenv("UNIFIED_CHECKOUT_BILLING_TYPE")) != ""

	// La dirección la rellena el backend porque nuestro formulario no la pide.
	// Pero SOLO tiene sentido inventar una guatemalteca si el comprador es de
	// Guatemala: mandar "Ciudad de Guatemala, 01001" junto a country=ES es una
	// incoherencia que el antifraude lee como dato falso. Para el resto se
	// manda solo lo que sabemos de verdad.
	switch {
	case laDaElWidget:
		// Nada nuestro: manda la del comprador, que es la buena.
		billTo.Address1 = req.BillTo.Address1
		billTo.Locality = req.BillTo.City
		billTo.AdminArea = req.BillTo.State
		billTo.PostalCode = req.BillTo.PostalCode
	case pais == "GT":
		billTo.Address1 = orDefault(req.BillTo.Address1, "Ciudad de Guatemala")
		billTo.Locality = orDefault(req.BillTo.City, "Guatemala")
		billTo.AdminArea = orDefault(req.BillTo.State, "GT")
		billTo.PostalCode = orDefault(req.BillTo.PostalCode, "01001")
	default:
		// Comprador de fuera y sin widget que le pregunte: no hay dirección
		// que mandar. Mejor vacío que una inventada del país equivocado.
		billTo.Address1 = req.BillTo.Address1
		billTo.Locality = req.BillTo.City
		billTo.AdminArea = req.BillTo.State
		billTo.PostalCode = req.BillTo.PostalCode
	}

	// ===== SEÑALES DE DISPOSITIVO PARA DECISION MANAGER =====
	// Hasta hoy no se mandaba NINGUNA: en el panel de Decision Manager las
	// columnas IP Address, IP Country y Device Fingerprint salían vacías en el
	// 100% de nuestras transacciones. El antifraude estaba decidiendo sobre
	// pagos sin saber desde dónde ni desde qué se pagaba.
	//
	// ⚠️ ESTO SE APLICA A LOS DOS CARRILES, TARJETA INCLUIDA, Y ES DELIBERADO.
	// No es una mejora del carril de wallet que se cuela en el de tarjeta por
	// descuido: las reglas de velocidad de NeoNet —compartidas con el resto de
	// sus comercios— agrupan por IP y por huella sin mirar el medio de pago, así
	// que el carril que hoy cobra dinero real es justo el que está ciego. Meter
	// esto solo en wallet no arreglaría nada de lo que se rechazó en producción.
	//
	// La IP sale de GetRealIP, NO de c.ClientIP(): detrás del proxy de Cloudflare
	// esta última es la IP de SALIDA de Cloudflare, la misma para todos los
	// compradores. Mandar esa sería peor que no mandar ninguna — le estaríamos
	// diciendo al antifraude que las 200 compras de la noche del evento salen de
	// una sola máquina, que es exactamente el patrón que sus reglas de velocidad
	// buscan para rechazar. GetRealIP valida la cabecera del proxy con un secreto
	// compartido, así que tampoco es falsificable desde fuera.
	//
	// VÁLVULA DE EMERGENCIA: PAYMENT_DEVICE_SIGNALS=false deja el struct a cero y
	// con él el cuerpo del cobro vuelve a ser byte a byte el de antes de este
	// cambio (lo fija TestSaleCardBodyByteIdenticalToLegacy con el valor cero).
	// Existe por un riesgo concreto: mandar la IP puede ACTIVAR reglas de
	// Decision Manager que hasta ahora no podían dispararse porque el dato no
	// llegaba —una regla de país de IP, por ejemplo, con compradores usando VPN—.
	// Si eso apareciera la noche del evento, se apaga con un secret y un
	// reinicio, sin desplegar código y sin tocar el carril del dinero.
	// Encendido por defecto: la ceguera actual es el problema que se está
	// arreglando, no el estado seguro.
	device := services.CybsDeviceInfo{}
	if deviceSignalsEnabled() {
		device = services.CybsDeviceInfo{
			IPAddress:    middleware.GetRealIP(c),
			UserAgent:    c.GetHeader("User-Agent"),
			AcceptHeader: c.GetHeader("Accept"),
			Language:     c.GetHeader("Accept-Language"),
		}
	}
	// La huella la manda la web (ver payOrderRequest.DeviceFingerprintID). Se
	// comprueba la FORMA antes de reenviarla: este valor lo escribe el navegador
	// y no vamos a usar la pasarela como eco de cualquier cadena.
	//
	// Si viene MALFORMADA se DESCARTA y se avisa, en vez de responder 400 como se
	// hace con el transient token. La diferencia es a propósito: sin token no hay
	// medio de pago y el cobro no puede seguir, pero la huella es una señal
	// auxiliar del antifraude. Tumbar una compra real la noche del evento porque
	// un script cacheado mandó un id raro sería peor que la ceguera que estamos
	// arreglando. Lo que NO se hace en ningún caso es reenviarla sin validar.
	if fp := strings.TrimSpace(req.DeviceFingerprintID); fp != "" && deviceSignalsEnabled() {
		if services.LooksLikeFingerprintSessionID(fp) {
			device.FingerprintSessionID = fp
		} else {
			log.Printf("[PayOrder] huella de dispositivo con forma inválida (descartada) order=%s len=%d", req.OrderID, len(fp))
		}
	}

	orderNumber := services.GetString(order, "order_number")
	// capture=true for public events (charge now); false for private events
	// (hold the funds until staff approves).
	capture := !needsApproval

	// ===== CLAIM ATÓMICO anti DOBLE-COBRO =====
	// Solo UN request puede pasar de aquí a cobrar esta orden. pending→
	// processing en un UPDATE condicional; si afecta 0 filas, otro request
	// (doble-click, retry de red tras un cobro que sí pasó) ya la reclamó →
	// 409 SIN tocar la pasarela. En declinada, recordDeclinedAttempt la
	// devuelve a pending para reintentar.
	claimed, claimErr := venueDB.UpdateCtx(ctx, "orders",
		map[string]interface{}{"status": "processing"},
		map[string]interface{}{"id": req.OrderID, "status": "pending"})
	if claimErr != nil {
		log.Printf("[PayOrder] claim error order=%s: %v", orderNumber, claimErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "No se pudo procesar el pago. Intenta de nuevo."})
		return
	}
	if len(claimed) == 0 {
		// Otro request ganó el claim. Releer para responder con precisión.
		cur, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
			"select": "status,order_number", "where": map[string]interface{}{"id": req.OrderID}})
		if services.GetString(cur, "status") == "confirmed" {
			c.JSON(http.StatusOK, gin.H{"success": true, "message": "Order already confirmed",
				"order_number": services.GetString(cur, "order_number"), "order_id": req.OrderID})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": "Este pago ya se está procesando. No vuelvas a pagar; revisa tu correo en unos minutos."})
		return
	}

	// --- Cobro único: el total, comisión incluida ---
	// El sufijo "-VENUE" del código de referencia se MANTIENE a propósito aunque
	// ya no haya un "-FEE" que lo acompañe: los carriles de captura y reversa
	// construyen "-VENUE-CAP" y "-VENUE-REL" a partir de él, y así las órdenes
	// nuevas y las viejas se leen igual en el Business Center.
	charge1, err := charger.ChargeCard(ctx, services.ChargeParams{
		ReferenceCode: orderNumber + "-VENUE",
		Amount:        chargeAmount,
		Currency:      currency,
		Card:          card,
		BillTo:        billTo,
		Capture:       capture,
		// Vacío en el carril de tarjeta: el cuerpo que se manda a la pasarela
		// es entonces idéntico al de siempre.
		TransientTokenJWT: transientToken,
		// Señales de dispositivo: van por los DOS carriles. Ver arriba.
		Device: device,
	})
	if err != nil {
		log.Printf("[PayOrder] charge1 error order=%s: %v", orderNumber, err)
		// Error transitorio de pasarela: liberar el claim para reintentar.
		venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{"status": "pending"},
			map[string]interface{}{"id": req.OrderID})
		c.JSON(http.StatusBadGateway, gin.H{"error": "No se pudo procesar el pago. Intenta de nuevo."})
		return
	}
	// El importe debe autorizarse ENTERO: una autorización parcial (prepago sin
	// saldo, o el trigger SDISCOUNT del sandbox) dejaría al local cobrando de
	// menos → se trata como rechazo y se libera lo retenido.
	venuePartial := charge1.Success && charge1.AuthorizedAmount > 0 && charge1.AuthorizedAmount < chargeAmount-0.005
	if !charge1.Success || venuePartial {
		// SOLO se reversa la autorización PARCIAL. En un rechazo liso no hay
		// nada que liberar —la pasarela no llegó a autorizar— y pedir la
		// reversa devolvía `INVALID_REQUEST`, que se registraba como ALERT.
		//
		// Parecía inofensivo y no lo es: cada tarjeta rechazada dejaba una
		// alerta falsa en los logs y una llamada de más a la pasarela. El día
		// del evento, con rechazos normales de tarjeta, esas alertas falsas
		// enterrarían a las de verdad — que son las que avisan de dinero
		// atrapado. Una alerta que salta cuando no pasa nada acaba ignorándose.
		//
		// Visto en producción el 2026-08-22 con los rechazos de Decision
		// Manager (`DECISION_PROFILE_REJECT`).
		if venuePartial && charge1.TransactionID != "" {
			rbCtx, rbCancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer rbCancel()
			if revErr := charger.ReverseCharge(rbCtx, charge1.TransactionID, orderNumber+"-VENUE-PARB", chargeAmount, currency); revErr != nil {
				log.Printf("[PayOrder] ALERT: liberación de autorización PARCIAL fallida order=%s tx=%s: %v — puede haber importe retenido en la tarjeta del comprador", orderNumber, charge1.TransactionID, revErr)
			}
		}
		recordDeclinedAttempt()
		msg := charge1.ErrorMessage
		if venuePartial {
			msg = "Tu tarjeta no autorizó el importe completo. Usa otra tarjeta."
		}
		c.JSON(http.StatusPaymentRequired, gin.H{"error": msg, "declined": true})
		return
	}

	// ===== GUARD: ¿DE VERDAD QUEDÓ RETENIDO? =====
	// Pedir capture=false NO garantiza que el dinero se quede retenido. Un
	// perfil de merchant configurado como venta forzada, un cambio del lado de
	// NeoNet, o un wallet que complete la sesión con otro mandato, pueden
	// liquidar el cobro igualmente. Hasta ahora dábamos por hecho que la
	// pasarela obedecía y anotábamos `captured` = lo que PEDIMOS.
	//
	// El daño de equivocarse ahí es silencioso y caro: con la orden marcada
	// "retenida" pero el dinero ya cobrado, al aprobar se intentaría capturar
	// algo ya capturado y al rechazar se reversaría una venta liquidada (que en
	// Cybersource no hace nada). El comprador se queda cobrado y sin entrada, y
	// nadie se entera.
	//
	// Así que se COMPRUEBA en la respuesta y se anota lo que PASÓ. Lo que NO se
	// hace es fallar el pago: el dinero ya se movió, y devolver un error aquí
	// dejaría al comprador cobrado, sin entrada y creyendo que no pagó.
	// Registrar la verdad es lo correcto — con ella, aprobar no recaptura de
	// más y rechazar DEVUELVE en vez de reversar en balde.
	captured := capture
	captureUnrequested := false
	if !capture {
		switch charge1.CaptureState {
		case services.CybsCaptureHeld:
			// Retención verificada. Camino normal, nada que anotar aparte.
		case services.CybsCaptureSettled:
			captured = true
			captureUnrequested = true
			log.Printf("[PayOrder] ALERTA — COBRO NO PEDIDO order=%s tx=%s: se pidió RETENER %.2f %s y la pasarela LIQUIDÓ el cobro (%s). La orden se anota como COBRADA: al aprobar no se recapturará, y al rechazar se DEVOLVERÁ el dinero.",
				orderNumber, charge1.TransactionID, chargeAmount, currency, charge1.CaptureEvidence)
		default:
			// Sin señal fiable. Nos quedamos con lo pedido —es exactamente lo
			// que hacía este código antes del guard— pero dejando rastro para
			// poder conciliarlo contra el Business Center.
			log.Printf("[PayOrder] retención SIN VERIFICAR order=%s tx=%s (%s) — se anota captured=false; si en el Business Center aparece liquidada, corregir la orden a mano",
				orderNumber, charge1.TransactionID, charge1.CaptureEvidence)
		}
	}

	// (Aquí iba la SEGUNDA transacción, la del fee, con su rollback: si el
	// segundo cargo fallaba había que deshacer el primero, y además de forma
	// distinta según el caso —en público la venta ya estaba liquidada y tocaba
	// REEMBOLSAR, en privado bastaba con reversar la autorización—. Todo ese
	// camino desaparece con el cobro único: un solo cargo no puede quedarse a
	// medias. Es la mayor ganancia de este cambio, más que el extracto limpio.)
	//
	// Las órdenes ANTIGUAS que sí tienen dos transacciones se siguen capturando y
	// reversando bien: sus carriles leen `fee_transaction` del split guardado y
	// solo actúan si viene informado.
	feeTxID := ""

	// La pasarela sale del PROCESADOR real, nunca escrita a mano: con dLocal
	// conviviendo con NeoNet, un literal "neonet" etiquetaría mal los cobros y
	// el carril de captura/reversa buscaría transacciones en la cuenta
	// equivocada.
	gatewayName := processor.GetGateway().String()

	// Persistir el desglose en la orden. La clave sigue llamándose
	// `payment_split` aunque ya no haya reparto: la leen las capturas, las
	// reversas y los informes, y renombrarla dejaría ilegibles las órdenes
	// anteriores a agosto de 2026. Los NOMBRES se mantienen; lo que cambia es
	// que ahora hay una sola transacción.
	//
	// OJO al significado de cada importe, que es lo que hace que órdenes viejas y
	// nuevas convivan sin tocar los carriles de captura/reversa:
	//   venue_amount = lo que la pasarela tiene retenido o cobrado. Es la cifra
	//                  que se captura y se reversa, así que ahora es el TOTAL.
	//   fee_amount   = lo que se cobró en una transacción APARTE. Ahora es 0, y
	//                  por eso los carriles no intentan capturar ningún fee.
	//   fee_included = el 8% que va DENTRO de venue_amount. Es contable: sirve
	//                  para liquidarle a Pull su parte. No se cobra por separado.
	metadata := orderMeta
	metadata["payment_split"] = map[string]interface{}{
		"venue_amount":      chargeAmount,
		"fee_amount":        0.0,
		"fee_included":      feeIncluded,
		"fee_percent":       feePercent,
		"venue_transaction": charge1.TransactionID,
		"fee_transaction":   feeTxID, // siempre "" desde el cobro único
		"gateway":           gatewayName,
		"fee_gateway_venue": "", // una sola cuenta: la del venue
		// `captured` = lo que PASÓ, no lo que se pidió (ver el guard de arriba).
		// De esta clave dependen los carriles de aprobar (capturar o no) y
		// rechazar (reversar o devolver), así que tiene que ser la verdad.
		"captured": captured,
	}
	// Auditoría del guard. Solo en las órdenes donde se pidió RETENER: las de
	// compra directa no ganan ninguna clave nueva y su metadata queda idéntica
	// a la de siempre.
	if !capture {
		if split, ok := metadata["payment_split"].(map[string]interface{}); ok {
			split["capture_requested"] = false
			split["capture_verified"] = string(charge1.CaptureState) // held | settled | unknown
			split["capture_evidence"] = charge1.CaptureEvidence
			if captureUnrequested {
				// MARCA EXPLÍCITA de "se cobró sin haberlo pedido". Es la que
				// hace que el rechazo DEVUELVA el dinero en vez de no hacer
				// nada: una orden capturada por la vía normal (aprobación del
				// local) no la lleva, y su rechazo sigue bloqueado como antes.
				split["capture_unrequested"] = true
			}
		}
	}
	// Trazabilidad: por dónde entró el pago. Solo se anota en el carril de
	// wallet — las órdenes pagadas con tarjeta siguen sin esta clave, como
	// todas las anteriores, y nada la exige.
	if transientToken != "" {
		metadata["checkout_mode"] = "unified_checkout"
	}

	// --- PRIVATE: funds are HELD. Leave the order awaiting staff approval,
	// notify staff, and do NOT issue tickets yet. ---
	if needsApproval {
		// 48h staff-decision deadline; the expiry job auto-reverses after it.
		deadline := time.Now().Add(48 * time.Hour)
		metadata["approval_deadline"] = deadline.Format(time.RFC3339)
		metadata["authorized_at"] = time.Now().Format(time.RFC3339)

		// Ruta de dinero: si el hold no se puede PERSISTIR, la tarjeta quedó
		// autorizada pero la orden no lo sabría (ni staff, ni job de 48h, ni
		// nadie que pueda capturar o reversar). Liberar las autorizaciones y
		// devolver error limpio — el cliente reintenta y no queda dinero
		// retenido huérfano.
		if err := venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
			"status":          "payment_authorized",
			"payment_gateway": gatewayName,
			"metadata":        metadata,
		}, map[string]interface{}{"id": req.OrderID}); err != nil {
			log.Printf("[PayOrder] ALERT: hold persist FAILED order=%s venueTx=%s feeTx=%s captured=%v: %v — deshaciendo",
				orderNumber, charge1.TransactionID, feeTxID, captured, err)
			rbCtx, rbCancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer rbCancel()
			// Deshacer POR EL CAMINO QUE CORRESPONDA: si la pasarela liquidó el
			// cobro pese al capture=false, un authReversal no hace nada y el
			// comprador se quedaría cobrado — ahí toca REEMBOLSO.
			if revErr := undoVenueCharge(rbCtx, charger, captured, charge1.TransactionID, orderNumber+"-VENUE-RB", chargeAmount, currency); revErr != nil {
				log.Printf("[PayOrder] ALERT: hold undo ALSO failed order=%s tx=%s captured=%v: %v", orderNumber, charge1.TransactionID, captured, revErr)
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "No se pudo registrar el pago. No se ha realizado ningún cargo — intenta de nuevo."})
			return
		}
		// El texto dice lo que pasó con el dinero, no lo que se pidió: si la
		// pasarela liquidó pese al capture=false, poner "RETENIDO" en el log
		// mandaría a conciliar por el camino equivocado.
		estado := "RETENIDO"
		if captured {
			estado = "COBRADO (la pasarela no retuvo — ver ALERTA arriba)"
		}
		log.Printf("[PayOrder] %s (pendiente de aprobación) order=%s total=%.2f (fee incluido %.2f) %s deadline=%s",
			estado, orderNumber, chargeAmount, feeIncluded, currency, deadline.Format(time.RFC3339))

		// Notify staff (push) + email the buyer the "pending approval" notice
		// (NO ticket/QR yet — that only goes out on approval).
		go func() {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer bgCancel()
			if services.Push != nil {
				services.Push.NotifyVenueStaff(bgCtx, venueID, "Nueva solicitud de entrada",
					services.GetString(order, "user_name")+" solicita entrada — pendiente de aprobar",
					"reservations", map[string]interface{}{
						"type":     "order_pending_approval",
						"order_id": req.OrderID,
					})
			}
			if services.Email != nil {
				sendApprovalStatusEmail(bgCtx, venueID, order, total, currency, "pending", false)
			}
		}()

		c.JSON(http.StatusOK, gin.H{
			"success":          true,
			"pending_approval": true,
			"message":          "Payment authorized, awaiting staff approval",
			"order_number":     orderNumber,
			"order_id":         req.OrderID,
		})
		return
	}

	// --- PUBLIC: funds captured. Register for the shared confirmation rail
	// (issues tickets + email + push). ---
	sessionID := "neonet_" + charge1.TransactionID
	services.RegisterNeoNetPayment(sessionID, &models.PaymentResult{
		Success:           true,
		TransactionID:     charge1.TransactionID,
		AuthorizationCode: charge1.AuthCode,
		Gateway:           processor.GetGateway(),
		CardLast4:         charge1.CardLast4,
		CardBrand:         charge1.CardBrand,
	})
	// Ruta de dinero: el cargo YA se ejecutó (capture=true). Si no se puede
	// persistir la sesión, ConfirmPayment devolvería 404 con el cliente ya
	// cobrado — reintentar una vez y si no, gritar con los tx IDs para poder
	// reversar/conciliar a mano.
	writeCharge := func() error {
		return venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
			"status":            "processing",
			"stripe_session_id": sessionID,
			"payment_gateway":   gatewayName,
			"metadata":          metadata,
		}, map[string]interface{}{"id": req.OrderID})
	}
	if err := writeCharge(); err != nil {
		log.Printf("[PayOrder] charge persist failed, retrying once order=%s: %v", orderNumber, err)
		if err = writeCharge(); err != nil {
			log.Printf("[PayOrder] ALERT: charge persist FAILED after retry order=%s venueTx=%s feeTx=%s: %v",
				orderNumber, charge1.TransactionID, feeTxID, err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "El pago se procesó pero hubo un error registrándolo. NO vuelvas a pagar — contacta al local con tu número de orden " + orderNumber + ".",
			})
			return
		}
	}

	log.Printf("[PayOrder] COBRADO order=%s total=%.2f (fee incluido %.2f) %s", orderNumber, chargeAmount, feeIncluded, currency)

	// Delegar al carril compartido: emite tickets, email y push.
	q := c.Request.URL.Query()
	q.Set("session_id", sessionID)
	q.Set("venue_id", venueID)
	c.Request.URL.RawQuery = q.Encode()
	ConfirmPayment(c)
}

// heldCaptureOutcome distinguishes "not a held order" from "held but the
// gateway capture failed", so the approve handler can respond correctly.
type heldCaptureOutcome int

const (
	notHeld       heldCaptureOutcome = iota // no gateway authorization to settle
	captureOK                               // funds captured; proceed to issue tickets
	captureFailed                           // held, but the gateway rejected the capture
)

// captureHeldOrder settles the two held authorizations of a private-event
// order (on staff approval) and returns the sessionID to feed ConfirmPayment.
func captureHeldOrder(ctx context.Context, venueID string, order map[string]interface{}) (sessionID string, outcome heldCaptureOutcome) {
	metadata, _ := order["metadata"].(map[string]interface{})
	split, _ := metadata["payment_split"].(map[string]interface{})
	if split == nil || services.GetString(split, "gateway") != "neonet" {
		return "", notHeld
	}
	venueTx := services.GetString(split, "venue_transaction")
	if venueTx == "" {
		return "", notHeld
	}
	// Ya capturada: o es un approve reintentado, o el guard de PayOrder detectó
	// que la pasarela liquidó el cobro sin habérselo pedido
	// (`capture_unrequested`). En los dos casos NO hay que volver a capturar
	// —sería un segundo cobro— y el camino correcto es el mismo: registrar la
	// sesión y emitir las entradas, que es justo lo que el local acaba de
	// aprobar.
	//
	// Already captured (e.g. a retried approve) — nothing to settle, but the
	// session MUST re-registrarse en el mapa en memoria: es por-máquina y
	// por-proceso, y ConfirmPayment lo consume (LoadAndDelete). Sin esto un
	// retry (u otra máquina) da "Failed to confirm payment" con el dinero ya
	// capturado.
	if captured, _ := split["captured"].(bool); captured {
		sessionID = "neonet_" + venueTx
		services.RegisterNeoNetPayment(sessionID, &models.PaymentResult{
			Success:       true,
			TransactionID: venueTx,
			Gateway:       models.GatewayNeoNet,
		})
		return sessionID, captureOK
	}
	processor, err := services.Payments.GetProcessor(ctx, venueID)
	if err != nil {
		return "", notHeld
	}
	charger, isCharger := processor.(services.DirectCardCharger)
	if !isCharger {
		return "", notHeld
	}
	orderNumber := services.GetString(order, "order_number")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	// Capture the venue share first — if THIS fails, the money was never
	// taken, so report the failure and let staff retry (the auth may have
	// expired issuer-side near the 48h edge).
	if err := charger.CapturePayment(ctx, venueTx, orderNumber+"-VENUE-CAP", services.GetFloat64(split, "venue_amount"), currency); err != nil {
		log.Printf("[Approve] venue capture FAILED order=%s: %v", orderNumber, err)
		return "", captureFailed
	}
	if feeTx := services.GetString(split, "fee_transaction"); feeTx != "" {
		if err := feeChargerForSplit(ctx, venueID, split, charger).CapturePayment(ctx, feeTx, orderNumber+"-FEE-CAP", services.GetFloat64(split, "fee_amount"), currency); err != nil {
			// Venue already captured; log but continue so tickets still issue.
			log.Printf("[Approve] fee capture failed (venue already captured) order=%s: %v", orderNumber, err)
		}
	}
	split["captured"] = true

	sessionID = "neonet_" + venueTx
	services.RegisterNeoNetPayment(sessionID, &models.PaymentResult{
		Success:       true,
		TransactionID: venueTx,
		Gateway:       processor.GetGateway(),
	})
	return sessionID, captureOK
}

// reverseHeldOrder releases the two held authorizations of a private-event
// order (on staff rejection). Si el guard de PayOrder detectó que la pasarela
// COBRÓ pese a habérsele pedido retención (`capture_unrequested`), deshace por
// REEMBOLSO en vez de por reversa — ver dentro.
//
// Returns (held, released): held=false si no hay
// autorizaciones que liberar; released=false si ALGUNA reversa falló en la
// pasarela (queda released_ok=false en metadata y ALERT en logs — la
// autorización caducará sola del lado del emisor, pero no digas "liberada").
func reverseHeldOrder(ctx context.Context, venueID string, order map[string]interface{}) (held bool, released bool) {
	metadata, _ := order["metadata"].(map[string]interface{})
	split, _ := metadata["payment_split"].(map[string]interface{})
	if split == nil || services.GetString(split, "gateway") != "neonet" {
		return false, false
	}
	// Ya capturada: una reversa no sirve, haría falta un reembolso.
	//
	// EXCEPCIÓN — `capture_unrequested`: la orden pidió RETENER y la pasarela
	// liquidó el cobro igualmente (lo detecta el guard de PayOrder). Ahí el
	// dinero salió de la cuenta del comprador sin que nadie lo aprobara, así
	// que rechazar SÍ tiene que deshacerlo, y la forma correcta es un
	// REEMBOLSO. Sin esta rama el rechazo se retiraba en silencio y el
	// comprador se quedaba cobrado y sin entrada.
	//
	// Las órdenes capturadas por la vía normal (el local aprobó) NO llevan esa
	// marca: para ellas el comportamiento es exactamente el de siempre.
	alreadyCaptured, _ := split["captured"].(bool)
	unrequested, _ := split["capture_unrequested"].(bool)
	if alreadyCaptured && !unrequested {
		return false, false
	}
	venueTx := services.GetString(split, "venue_transaction")
	if venueTx == "" {
		return false, false
	}
	processor, err := services.Payments.GetProcessor(ctx, venueID)
	if err != nil {
		return false, false
	}
	charger, isCharger := processor.(services.DirectCardCharger)
	if !isCharger {
		return false, false
	}
	orderNumber := services.GetString(order, "order_number")
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}
	released = true
	// undoVenueCharge elige reversa o REEMBOLSO según cómo quedó el dinero de
	// verdad: `alreadyCaptured` aquí solo puede ser true en el caso
	// `capture_unrequested` (cobro que la pasarela liquidó sin pedírselo).
	if err := undoVenueCharge(ctx, charger, alreadyCaptured, venueTx, orderNumber+"-VENUE-REL", services.GetFloat64(split, "venue_amount"), currency); err != nil {
		log.Printf("[Reject] venue undo failed (captured=%v) order=%s: %v", alreadyCaptured, orderNumber, err)
		released = false
	} else if alreadyCaptured {
		// Deja constancia de que esto fue una DEVOLUCIÓN, no una liberación de
		// retención: en el extracto del comprador se ve distinto y tarda días.
		split["refunded_unrequested_capture"] = true
		log.Printf("[Reject] REEMBOLSADO (la pasarela había cobrado pese a la retención) order=%s tx=%s", orderNumber, venueTx)
	}
	if feeTx := services.GetString(split, "fee_transaction"); feeTx != "" {
		if err := feeChargerForSplit(ctx, venueID, split, charger).ReverseCharge(ctx, feeTx, orderNumber+"-FEE-REL", services.GetFloat64(split, "fee_amount"), currency); err != nil {
			log.Printf("[Reject] fee auth reversal failed order=%s: %v", orderNumber, err)
			released = false
		}
	}
	// Traceability: stamp when the hold was released and whether it fully
	// succeeded, so a failed reversal can be spotted/retried later.
	split["released_at"] = time.Now().Format(time.RFC3339)
	split["released_ok"] = released
	venueDB := services.DB.ForVenue(venueID)
	if venueDB != nil {
		venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{"metadata": metadata},
			map[string]interface{}{"id": services.GetString(order, "id")})
	}
	if !released {
		log.Printf("[Reject] ALERT: REVERSAL INCOMPLETE order=%s — revisar en EBC y reversar a mano si sigue retenida",
			orderNumber)
	}
	return true, released
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// prefijosPais traduce el prefijo telefónico internacional al código de país
// de dos letras (ISO 3166-1) que espera Cybersource.
//
// Es una lista corta a propósito: cubre Centroamérica (de donde son los
// compradores), el resto de Latinoamérica, España, y los sitios desde los que
// de verdad se compra. Lo que no esté aquí cae al país del local, que es la
// suposición menos mala.
//
// ⚠️ +1 es AMBIGUO: es Estados Unidos, Canadá y buena parte del Caribe. Se
// mapea a US porque es el caso dominante con diferencia. No es exacto, pero
// para un número +1 acertar "US" falla mucho menos que asumir "GT".
var prefijosPais = map[string]string{
	"502": "GT", // Guatemala — el nuestro
	"503": "SV", "504": "HN", "505": "NI", "506": "CR", "507": "PA", "501": "BZ",
	"52": "MX", "57": "CO", "51": "PE", "54": "AR", "56": "CL", "58": "VE",
	"593": "EC", "591": "BO", "598": "UY", "595": "PY", "55": "BR",
	"34": "ES", "1": "US",
	"44": "GB", "33": "FR", "39": "IT", "49": "DE", "351": "PT",
}

// paisDelComprador deduce el país a partir del prefijo que eligió el comprador
// en el formulario. Devuelve "GT" cuando no hay prefijo o no lo reconocemos:
// el local es guatemalteco y sus compradores también, así que ese es el
// respaldo correcto.
//
// El emparejado va de MÁS LARGO A MÁS CORTO, y eso importa: "+502" tiene que
// dar Guatemala, no Estados Unidos por empezar por "5"... ni "+1" comerse a
// "+52". Con un mapa hay que ordenar la búsqueda a mano.
func paisDelComprador(order map[string]interface{}) string {
	const respaldo = "GT"

	md, ok := order["metadata"].(map[string]interface{})
	if !ok {
		return respaldo
	}
	raw, ok := md["tickets_data"].([]interface{})
	if !ok || len(raw) == 0 {
		return respaldo
	}
	primero, ok := raw[0].(map[string]interface{})
	if !ok {
		return respaldo
	}

	// "+502", "502", "+502 " — todo vale; nos quedamos con los dígitos.
	var digitos strings.Builder
	for _, r := range services.GetString(primero, "owner_phone_prefix") {
		if r >= '0' && r <= '9' {
			digitos.WriteRune(r)
		}
	}
	prefijo := digitos.String()
	if prefijo == "" {
		return respaldo
	}

	// De 3 dígitos a 1: el más específico gana.
	for n := 3; n >= 1; n-- {
		if len(prefijo) < n {
			continue
		}
		if pais, ok := prefijosPais[prefijo[:n]]; ok {
			return pais
		}
	}
	return respaldo
}

// telefonoDelComprador saca el teléfono REAL de quien está pagando.
//
// Hasta el 2026-08-22 aquí había un literal: "50200000000". O sea 502 (el
// prefijo de Guatemala) seguido de ocho ceros. TODAS las transacciones de
// Pull, de todos los venues y de todos los compradores, llegaban a
// Cybersource con ese mismo número de teléfono inventado.
//
// Eso no es un detalle cosmético del panel, es munición para el antifraude:
//
//   - Las reglas de velocidad de NeoNet agrupan por dato repetido. Ya sabemos
//     que una salta con "mismo email > 8 en 3 meses". Si existe la gemela por
//     teléfono —y con 150 reglas activas es lo más probable— nuestras compras
//     la revientan por definición: comparten el número las 200.
//   - Un teléfono acabado en ocho ceros es, por sí solo, un patrón de datos
//     falsos. Es exactamente lo que un filtro de fraude busca.
//
// El teléfono de verdad SÍ lo tenemos: el formulario de la web lo exige y
// viaja en metadata.tickets_data[0] (owner_phone + owner_phone_prefix). El
// comprador es siempre el primer asistente. Solo había que leerlo.
//
// Si por lo que sea no está, se devuelve "" y `Sale()` omite la clave: mandar
// un teléfono vacío es honesto, mandar uno falso compartido por todos es lo
// que nos estaba haciendo daño.
func telefonoDelComprador(order map[string]interface{}) string {
	md, ok := order["metadata"].(map[string]interface{})
	if !ok {
		return ""
	}
	raw, ok := md["tickets_data"].([]interface{})
	if !ok || len(raw) == 0 {
		return ""
	}
	primero, ok := raw[0].(map[string]interface{})
	if !ok {
		return ""
	}
	numero := strings.TrimSpace(services.GetString(primero, "owner_phone"))
	if numero == "" {
		return ""
	}
	prefijo := strings.TrimSpace(services.GetString(primero, "owner_phone_prefix"))

	// Cybersource quiere dígitos. El prefijo llega como "+502" y el número
	// puede traer espacios o guiones de cómo lo escribió la persona.
	junto := prefijo + numero
	var b strings.Builder
	for _, r := range junto {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// claimHeldOrder atomically claims a held (payment_authorized) order for
// settlement/release, flipping it to 'processing' only if it's still
// payment_authorized. Returns true for the single caller that wins — so
// approve, reject and the 48h expiry job can't act on the same hold twice.
func claimHeldOrder(ctx context.Context, venueDB *services.SupabaseClient, orderID string) bool {
	res, err := venueDB.UpdateCtx(ctx, "orders", map[string]interface{}{
		"status": "processing",
	}, map[string]interface{}{
		"id":     orderID,
		"status": "payment_authorized",
	})
	return err == nil && len(res) > 0
}

// sendApprovalStatusEmail sends the private-event status email to the buyer.
// kind: "pending" (awaiting approval), "rejected" (staff declined),
// "expired" (48h passed). Resolves event + venue names for the copy.
// buildApprovalEmailData reúne los datos del evento/venue para los emails del
// flujo de aprobación. Devuelve también el destinatario ("" si no hay email).
// Compartido por sendApprovalStatusEmail y sendApprovalApprovedEmail.
func buildApprovalEmailData(ctx context.Context, venueID string, order map[string]interface{}, total float64, currency string) (services.ApprovalEmailData, string) {
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		return services.ApprovalEmailData{}, ""
	}
	eventName, eventImage, eventDate, eventTime, eventLocation := "", "", "", "", ""
	if eid := services.GetString(order, "event_id"); eid != "" {
		if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "name,image,cover_image,start_datetime,end_datetime,location,address",
			"where":  map[string]interface{}{"id": eid},
		}); ev != nil {
			services.EnrichEvent(ev)
			eventName = services.GetString(ev, "name")
			eventImage = services.GetString(ev, "image")
			if eventImage == "" {
				eventImage = services.GetString(ev, "cover_image")
			}
			eventDate = services.GetString(ev, "event_date")
			eventTime = services.GetString(ev, "start_time")
			eventLocation = services.GetString(ev, "location")
			if eventLocation == "" {
				eventLocation = services.GetString(ev, "address")
			}
		}
	}
	venueName := ""
	if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
		"select": "name", "where": map[string]interface{}{"id": venueID},
	}); v != nil {
		venueName = services.GetString(v, "name")
	}
	return services.ApprovalEmailData{
		CustomerName:  services.GetString(order, "user_name"),
		EventName:     eventName,
		EventImage:    eventImage,
		EventDate:     eventDate,
		EventTime:     eventTime,
		VenueName:     venueName,
		VenueLocation: eventLocation,
		OrderNumber:   services.GetString(order, "order_number"),
		Total:         fmt.Sprintf("%.2f", total),
		Currency:      currency,
	}, services.GetString(order, "user_email")
}

func sendApprovalStatusEmail(ctx context.Context, venueID string, order map[string]interface{}, total float64, currency, kind string, expired bool) {
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil || services.Email == nil {
		return
	}
	eventName, eventImage, eventDate, eventTime, eventLocation := "", "", "", "", ""
	if eid := services.GetString(order, "event_id"); eid != "" {
		if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "name,image,cover_image,start_datetime,end_datetime,location,address",
			"where":  map[string]interface{}{"id": eid},
		}); ev != nil {
			services.EnrichEvent(ev)
			eventName = services.GetString(ev, "name")
			eventImage = services.GetString(ev, "image")
			if eventImage == "" {
				eventImage = services.GetString(ev, "cover_image")
			}
			eventDate = services.GetString(ev, "event_date")
			eventTime = services.GetString(ev, "start_time")
			eventLocation = services.GetString(ev, "location")
			if eventLocation == "" {
				eventLocation = services.GetString(ev, "address")
			}
		}
	}
	venueName := ""
	if v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
		"select": "name", "where": map[string]interface{}{"id": venueID},
	}); v != nil {
		venueName = services.GetString(v, "name")
	}
	to := services.GetString(order, "user_email")
	if to == "" {
		return
	}
	data := services.ApprovalEmailData{
		CustomerName:  services.GetString(order, "user_name"),
		EventName:     eventName,
		EventImage:    eventImage,
		EventDate:     eventDate,
		EventTime:     eventTime,
		VenueName:     venueName,
		VenueLocation: eventLocation,
		OrderNumber:   services.GetString(order, "order_number"),
		Total:         fmt.Sprintf("%.2f", total),
		Currency:      currency,
	}
	switch kind {
	case "pending":
		_ = services.Email.SendApprovalPending(ctx, to, data)
	case "rejected", "expired":
		_ = services.Email.SendApprovalRejected(ctx, to, data, expired)
	}
}

// undoVenueCharge deshace la 1ª tx (entrada) según cómo se cobró: si fue
// captura inmediata (público) hay que REEMBOLSAR; si fue solo autorización
// (privado, retención) basta con reversar. Usar el método equivocado deja al
// comprador cobrado sin ticket (reverse sobre venta liquidada = no-op).
func undoVenueCharge(ctx context.Context, charger services.DirectCardCharger, captured bool, txID, ref string, amount float64, currency string) error {
	if captured {
		return charger.RefundCharge(ctx, txID, ref, amount, currency)
	}
	return charger.ReverseCharge(ctx, txID, ref, amount, currency)
}

// feeChargerForSplit devuelve el charger por el que se movió el FEE de una
// orden retenida: la cuenta de plataforma si el split la registró
// (fee_gateway_venue), o la del venue (fallback) si no.
func feeChargerForSplit(ctx context.Context, venueID string, split map[string]interface{}, fallback services.DirectCardCharger) services.DirectCardCharger {
	fv := services.GetString(split, "fee_gateway_venue")
	if fv == "" {
		return fallback
	}
	if p, err := services.Payments.GetProcessor(ctx, fv); err == nil {
		if fc, ok := p.(services.DirectCardCharger); ok {
			return fc
		}
	}
	log.Printf("[Fee] ALERT: no se pudo resolver el gateway de plataforma %s — usando el del venue", fv)
	return fallback
}

// safeLookupCode reports whether a user-supplied lookup value (order number,
// UUID, QR token) is safe to pass to PostgREST as an equality filter. Dots
// are excluded on purpose: they enable operator injection ("not.is.null").
func safeLookupCode(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for _, r := range v {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

var _ = fmt.Sprintf // keep fmt if unused paths change
