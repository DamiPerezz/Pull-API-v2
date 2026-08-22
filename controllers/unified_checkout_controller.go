package controllers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"pull-api-v2/config"
	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// =============================================================================
// UNIFIED CHECKOUT — Apple Pay / Google Pay
//
// QUÉ ES: el componente de Cybersource que pinta los botones de wallet. El
// navegador no puede invocarlo solo — necesita un "capture context", que es una
// sesión FIRMADA POR NOSOTROS con el importe y los orígenes autorizados dentro.
// Este fichero abre esa sesión. El cobro en sí sigue saliendo por /orders/pay.
//
// EL DINERO VA ASÍ:
//
//	1. POST /payments/capture-context → validamos la orden (mismo guard que el
//	   pago) y devolvemos el JWT de sesión.
//	2. El SDK del navegador pinta Apple/Google Pay y, cuando el comprador
//	   aprueba, devuelve un "transient token" (JWT de un solo uso).
//	   LA TARJETA NO PASA POR NUESTRO SERVIDOR. Solo llega ese token.
//	3. POST /orders/pay con `transient_token` en lugar de `card` → el mismo
//	   carril de siempre: Sale(), claim atómico, emisión de entradas.
//
// PÚBLICOS Y PRIVADOS, LOS DOS (decisión de agosto 2026):
// un evento privado no se cobra, se RETIENE (Sale con capture=false) y se
// captura o se revierte hasta 48 h después. Durante un tiempo el wallet se
// ofreció solo en públicos porque no estaba confirmado que esa retención
// funcionara sobre un pago con wallet. Ya lo está, por tres vías:
//
//	1. La documentación que NeoNet nos pasó (Payments Developer Guide — REST
//	   API, Visa Platform Connect) lo dice explícitamente: pág. 36,
//	   completeMandate.type = AUTH → "Authorize the payment and capture the
//	   funds at a later date"; pág. 31, ejemplo "Requesting an Authorization
//	   with a Transient Token" — o sea, autorización diferida CON token.
//	2. La retención de 48 h ya está PROBADA contra NeoNet y bancos
//	   guatemaltecos por el carril de tarjeta. No es teoría.
//	3. Apple Pay y Google Pay no son un medio de pago aparte: son tarjetas
//	   (tabla de la pág. 84 del mismo manual). Van por el mismo carril, con los
//	   mismos parámetros; lo único que cambia es que el PAN llega tokenizado.
//
// Lo que NO se da por supuesto es que la pasarela obedezca: después de cobrar,
// PayOrder VERIFICA en la respuesta si el dinero quedó retenido de verdad y
// anota la verdad en la orden. Ver el guard en pay_controller.go.
//
// La sesión declara su intención con completeMandate.type (CAPTURE en público,
// AUTH en privado), sacada de la MISMA lectura del evento que decide el
// `capture` del cobro.
//
// INTERRUPTOR: UNIFIED_CHECKOUT_ENABLED=true. Sin él este endpoint responde
// 501 "no habilitado" y /orders/pay rechaza cualquier `transient_token`, con lo
// que el sistema se comporta EXACTAMENTE igual que antes de existir esto.
// =============================================================================

// unifiedCheckoutEnabled es el interruptor. APAGADO por defecto: mientras no
// esté encendido, nada de este camino puede tocar la pasarela.
//
// Para encenderlo:
//
//	staging → flyctl secrets set UNIFIED_CHECKOUT_ENABLED=true -a pull-api-v2-staging
//	prod    → lo mismo con -a pull-api-v2-prod (NO antes de validarlo en staging)
//	local   → UNIFIED_CHECKOUT_ENABLED=true en el .env que estés usando
func unifiedCheckoutEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("UNIFIED_CHECKOUT_ENABLED")), "true")
}

// Valores por defecto de la sesión. Todos configurables porque los fija
// Cybersource/NeoNet, no nosotros.
const (
	// Versión del SDK de Unified Checkout.
	//
	// ⚠️ 0.26 ES UN MÍNIMO, NO UNA PREFERENCIA. La tabla de versiones del
	// manual (Capture Context API, pág. 41-42) dice literalmente:
	//
	//	0.26 — "Support for the complete mandate."
	//
	// Hasta el 2026-08-22 esto valía "0.24" Y ADEMÁS mandábamos
	// `completeMandate`. Una versión vieja no da error con un campo que no
	// conoce: LO IGNORA EN SILENCIO. O sea que durante todo ese tiempo:
	//
	//	- `completeMandate.decisionManager: true` no encendía nada. Es el campo
	//	  con el que creíamos haber activado el perfilado de dispositivo dentro
	//	  del widget, y el panel de Decision Manager seguía diciendo
	//	  "Device Fingerprint: Not Submitted" también en los pagos por wallet.
	//	- `completeMandate.type` (CAPTURE / AUTH) tampoco se declaraba.
	//
	// POR QUÉ 0.26 Y NO 0.28, que es la más nueva del manual: 0.28 cambia
	// además el comportamiento de la entrada manual de tarjeta (añade Payer
	// Authentication y quita pantallas de confirmación "para ciertos casos").
	// Ese es el carril que HOY cobra dinero de verdad y que hay que vender el
	// 6-sep, así que no se toca a ciegas. 0.26 es la versión mínima que hace
	// funcionar lo que ya mandábamos, y sus otros cambios son de Click to Pay
	// (que no usamos) y campos añadidos a la respuesta del token.
	//
	// Se puede mover con UNIFIED_CHECKOUT_CLIENT_VERSION sin desplegar.
	defaultUCClientVersion = "0.26"
	defaultUCCountry       = "GT"
	defaultUCLocale        = "es_GT"
	// Wallets ÚNICAMENTE. PANENTRY (formulario de tarjeta de Cybersource) se
	// deja fuera a propósito: la tarjeta ya se cobra por nuestro formulario, y
	// meter un segundo carril de tarjeta duplicaría la superficie de la única
	// parte del sistema que hoy mueve dinero de verdad.
	defaultUCPaymentTypes = "APPLEPAY,GOOGLEPAY"
	defaultUCCardNetworks = "VISA,MASTERCARD,AMEX"
)

// ucDecisionManagerEnabled dice si la sesión de Unified Checkout pide correr
// Decision Manager Y —lo que aquí importa— el perfilado del dispositivo.
//
// POR DEFECTO SÍ. Es el motivo de que este campo exista: sin él, la propia
// documentación de NeoNet dice que "Decision Manager and device fingerprinting
// services do not run", que es justo la ceguera que estamos arreglando. Con él,
// Unified Checkout perfila el dispositivo y mete el `fingerprintSessionId`
// dentro del transient token, así que la huella llega a la pasarela sin que la
// web ni nosotros tengamos que generarla.
//
// Se puede APAGAR con UNIFIED_CHECKOUT_DECISION_MANAGER=false. La válvula existe
// porque encender el antifraude dentro del widget es un cambio de
// comportamiento del lado de NeoNet que no podemos ensayar solos: si el día del
// evento resultara que Decision Manager rechaza en la sesión del wallet, se
// apaga con un secret y un reinicio, sin desplegar código.
func ucDecisionManagerEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("UNIFIED_CHECKOUT_DECISION_MANAGER")), "false")
}

func ucEnvList(key, def string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		raw = def
	}
	out := []string{}
	for _, p := range strings.Split(raw, ",") {
		if v := strings.ToUpper(strings.TrimSpace(p)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func ucEnvString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// ucEnvBool lee una variable booleana que por defecto está APAGADA. Solo un
// "true" explícito la enciende: cualquier otra cosa —vacía, basura, "1"— deja
// el comportamiento de siempre. En el carril del dinero, un valor mal escrito
// tiene que ser inofensivo.
func ucEnvBool(key string) bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(key)), "true")
}

// unifiedCheckoutTargetOrigins devuelve los orígenes donde se puede embeber el
// componente. NO es cosmético: Cybersource ata la sesión a esos orígenes, así
// que es lo que impide que un clon del sitio abra cobros con nuestra cuenta.
//
// Se sacan del entorno (ALLOWED_ORIGINS / FRONTEND_URL, que ya distinguen demo,
// staging y producción) y se normalizan a esquema+host+puerto sin ruta, que es
// lo único que Cybersource acepta. Override explícito:
// UNIFIED_CHECKOUT_TARGET_ORIGINS.
func unifiedCheckoutTargetOrigins() []string {
	candidates := []string{}
	if raw := strings.TrimSpace(os.Getenv("UNIFIED_CHECKOUT_TARGET_ORIGINS")); raw != "" {
		candidates = append(candidates, strings.Split(raw, ",")...)
	} else if config.App != nil {
		candidates = append(candidates, config.App.AllowedOrigins...)
		candidates = append(candidates, config.App.FrontendURL)
	}

	seen := map[string]bool{}
	out := []string{}
	for _, raw := range candidates {
		origin := normalizeOrigin(raw)
		if origin == "" || seen[origin] {
			continue
		}
		seen[origin] = true
		out = append(out, origin)
	}
	return out
}

// normalizeOrigin recorta una URL a "esquema://host[:puerto]". Descarta
// cualquier cosa que no sea https (Cybersource lo exige), salvo localhost, para
// poder integrar en local.
func normalizeOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	isLocal := host == "localhost" || host == "127.0.0.1"
	if u.Scheme != "https" && !(u.Scheme == "http" && isLocal) {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// orderRequiresApproval dice si la orden pertenece a un evento privado (el que
// retiene en vez de cobrar). Ya no sirve para NEGAR el wallet —los privados
// también lo tienen— sino para declarar la intención de la sesión:
// requiresApproval=true → completeMandate.type=AUTH; false → CAPTURE.
//
// ⚠️ MISMA LECTURA que hace PayOrder para decidir capture=true/false
// (controllers/pay_controller.go, variable `needsApproval`). Tienen que decir
// siempre lo mismo: si la sesión se abre declarando "cobro al momento" y el
// cobro se hace con capture=false (o al revés), la pasarela y nosotros
// estaríamos hablando de dos operaciones distintas sobre el mismo dinero. Si
// algún día cambian ahí las columnas que definen "privado", cámbialas AQUÍ.
//
// POR QUÉ DEVUELVE ERROR EN VEZ DE ADIVINAR (corregido 2026-08-22):
// aquí había un fail-closed —"si no puedo leer el evento, declaro AUTH"— que
// SONABA prudente y creaba justo la divergencia que este comentario dice que no
// puede existir. Porque PayOrder, ante el MISMO fallo de lectura, hace lo
// contrario: deja `needsApproval=false` y COBRA al momento. O sea que un fallo
// transitorio de la tabla `events` entre las dos llamadas abría la sesión
// declarando "autorizo y capturo luego", le decía a la web `requires_approval`
// (que pinta "se te retendrá el importe"), y acto seguido cobraba de verdad.
// El comprador leería "retenido" con el dinero ya fuera de su cuenta.
//
// No se toca el fail-open de PayOrder: ese es el comportamiento probado del
// carril que hoy cobra dinero real, y cambiarlo rompería la regla de que con el
// interruptor apagado nada se comporta distinto. Lo que se corrige es esto:
// si no se puede saber qué va a hacer el cobro, NO se abre sesión. El
// resultado para el comprador es el formulario de tarjeta de siempre.
func orderRequiresApproval(ctx context.Context, venueDB *services.SupabaseClient, order map[string]interface{}) (bool, error) {
	eventID := services.GetString(order, "event_id")
	if eventID == "" {
		// Igual que PayOrder: sin evento no hay aprobación que pedir.
		return false, nil
	}
	ev, err := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": "is_private,require_approval",
		"where":  map[string]interface{}{"id": eventID},
	})
	if err != nil || ev == nil {
		return false, fmt.Errorf("no se pudo leer el evento %s: %v", eventID, err)
	}
	return services.GetBool(ev, "is_private") || services.GetBool(ev, "require_approval"), nil
}

// Valores de completeMandate.type. Ver CaptureContextParams.CompleteMandateType
// en services/cybersource.go — y no confundirlo con `captureMandate`, que habla
// de qué datos pide el widget, no de dinero.
const (
	ucMandateCapture = "CAPTURE" // cobrar al momento — evento público
	ucMandateAuth    = "AUTH"    // autorizar y capturar después — evento privado
)

// mandateForApproval traduce "este evento necesita aprobación" al mandato que
// se le declara a la pasarela. Es UNA LÍNEA a propósito y está aquí fuera para
// poder fijarla en un test: es la bisagra entre lo que la sesión promete y lo
// que el cobro hace.
//
// La equivalencia que NO puede romperse nunca es esta:
//
//	needsApproval=true  → capture=false (retener) → mandato AUTH
//	needsApproval=false → capture=true  (cobrar)  → mandato CAPTURE
//
// Invertirla significaría retener declarando venta, o cobrar declarando
// retención. Ver TestMandateMatchesCapture.
func mandateForApproval(requiresApproval bool) string {
	if requiresApproval {
		return ucMandateAuth
	}
	return ucMandateCapture
}

type captureContextRequest struct {
	OrderID string `json:"order_id"`
	// Mismo guard que el pago: sin el código de ESTA orden no se abre sesión.
	PaymentLinkCode string `json:"payment_link_code"`
	VenueID         string `json:"venue_id"`
	VenueSlug       string `json:"venue_slug"`
}

// PaymentsCaptureContext abre la sesión de Unified Checkout de una orden.
// POST /api/v1/payments/capture-context
//
// NO acepta importe del navegador: el importe y la moneda salen de la orden y
// viajan firmados dentro del JWT. Si se aceptasen de fuera, cualquiera podría
// pagar 1 GTQ una entrada de 300 y el cobro sería perfectamente válido.
func PaymentsCaptureContext(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	// INTERRUPTOR. Primero de todo: apagado, esto no existe.
	if !unifiedCheckoutEnabled() {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error":   "Unified Checkout no está habilitado en este entorno.",
			"enabled": false,
		})
		return
	}

	var req captureContextRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.OrderID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id is required"})
		return
	}
	req.OrderID = strings.TrimSpace(req.OrderID)
	// SECURITY: mismo saneado que PayOrder — bloquea inyección de operadores
	// PostgREST por el id.
	if !safeLookupCode(req.OrderID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid order_id"})
		return
	}

	venueID, venueDB := resolveVenueForCheckout(ctx, strings.TrimSpace(req.VenueID), strings.TrimSpace(req.VenueSlug))
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	order, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,status,total,currency,event_id,metadata",
		"where":  map[string]interface{}{"id": req.OrderID},
	})
	if err != nil || order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	// ANTI-CARDING: el MISMO guard que el pago (pay_guard.go). Sin esto, quien
	// adivinara un id de orden podría abrir sesiones de cobro ajenas. Falla
	// cerrado: una orden sin código guardado no es elegible.
	orderMeta, _ := order["metadata"].(map[string]interface{})
	if orderMeta == nil {
		orderMeta = map[string]interface{}{}
	}
	if !matchPaymentLinkCode(orderMeta, req.PaymentLinkCode) {
		log.Printf("[UnifiedCheckout] payment_link_code mismatch order=%s", req.OrderID)
		c.JSON(http.StatusForbidden, gin.H{"error": "Código de pago inválido para esta orden."})
		return
	}

	switch st := services.GetString(order, "status"); st {
	case "pending", "":
		// pagable
	case "confirmed":
		c.JSON(http.StatusOK, gin.H{
			"success": true, "already_paid": true,
			"message":      "Esta orden ya está pagada",
			"order_number": services.GetString(order, "order_number"),
		})
		return
	default:
		// Incluye `processing` (otro intento en curso) y `payment_authorized`.
		c.JSON(http.StatusConflict, gin.H{"error": "Esta orden no se puede pagar", "status": st})
		return
	}

	// ===== QUÉ SE VA A HACER CON EL DINERO =====
	// Público → CAPTURE (se cobra al momento). Privado → AUTH (se retiene y se
	// captura cuando el local aprueba, o se revierte). Antes esto era una
	// puerta cerrada: el privado se rechazaba. Ya no — ver la cabecera del
	// fichero para el porqué (doc de NeoNet pág. 36 + retención de 48 h ya
	// probada con tarjeta contra bancos guatemaltecos).
	//
	// Esta lectura tiene que coincidir con el `needsApproval` de PayOrder: es
	// el mismo dato leído del mismo sitio, y de él salen tanto el mandato de la
	// sesión como el `capture` del cobro.
	requiresApproval, err := orderRequiresApproval(ctx, venueDB, order)
	if err != nil {
		// No se puede saber si el cobro va a RETENER o a COBRAR, así que no se
		// abre ninguna sesión: cualquier mandato que declarásemos aquí podría
		// contradecir lo que haga /orders/pay un segundo después. La web se cae
		// al formulario de tarjeta, que es el camino probado.
		log.Printf("[UnifiedCheckout] sesión NO abierta order=%s: %v — la web usará el formulario de tarjeta", req.OrderID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "No se pudo iniciar el pago con wallet. Usa el pago con tarjeta."})
		return
	}
	completeMandate := mandateForApproval(requiresApproval)

	total := services.GetFloat64(order, "total")
	if total <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Order total is invalid"})
		return
	}
	currency := services.GetString(order, "currency")
	if currency == "" {
		currency = "GTQ"
	}

	origins := unifiedCheckoutTargetOrigins()
	if len(origins) == 0 {
		// Sin orígenes la sesión no sirve para nada y Cybersource la rechaza.
		// Es un fallo de configuración del entorno, no del comprador.
		log.Printf("[UnifiedCheckout] ALERT sin targetOrigins válidos — revisa ALLOWED_ORIGINS/FRONTEND_URL o UNIFIED_CHECKOUT_TARGET_ORIGINS")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unified Checkout mal configurado en este entorno."})
		return
	}

	processor, err := services.Payments.GetProcessor(ctx, venueID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway not configured"})
		return
	}
	provider, ok := processor.(services.UnifiedCheckoutProvider)
	if !ok {
		// Pasarela que cobra pero no ofrece wallets (o DEMO_MODE con el mock).
		// Decirlo claro evita media hora de depuración contra un 500 genérico.
		log.Printf("[UnifiedCheckout] gateway=%s no soporta Unified Checkout order=%s",
			processor.GetGateway(), req.OrderID)
		c.JSON(http.StatusNotImplemented, gin.H{
			"error":            "Esta pasarela no ofrece Apple Pay / Google Pay.",
			"gateway":          processor.GetGateway().String(),
			"wallets_eligible": false,
		})
		return
	}

	orderNumber := services.GetString(order, "order_number")
	// decisionManager=true es lo que hace que Unified Checkout PERFILE EL
	// DISPOSITIVO. Ver ucDecisionManagerEnabled y CaptureContextParams.
	decisionManager := ucDecisionManagerEnabled()
	jwt, err := provider.CaptureContext(ctx, services.CaptureContextParams{
		TargetOrigins:       origins,
		Amount:              total,
		Currency:            currency,
		Country:             ucEnvString("UNIFIED_CHECKOUT_COUNTRY", defaultUCCountry),
		Locale:              ucEnvString("UNIFIED_CHECKOUT_LOCALE", defaultUCLocale),
		AllowedPaymentTypes: ucEnvList("UNIFIED_CHECKOUT_PAYMENT_TYPES", defaultUCPaymentTypes),
		AllowedCardNetworks: ucEnvList("UNIFIED_CHECKOUT_CARD_NETWORKS", defaultUCCardNetworks),
		ClientVersion:       ucEnvString("UNIFIED_CHECKOUT_CLIENT_VERSION", defaultUCClientVersion),
		ReferenceCode:       orderNumber + "-UC",
		CompleteMandateType: completeMandate,
		DecisionManager:     decisionManager,

		// Palancas nuevas (2026-08-22), todas APAGADAS por defecto: sin
		// ponerlas, el cuerpo que sale es idéntico al de antes. Existen porque
		// las tres salieron de releer el manual y ninguna se puede ensayar
		// contra el sandbox —el perfil de NeoNet solo está publicado en
		// producción—, así que se prueban de una en una con un secret.
		//
		//	UNIFIED_CHECKOUT_GOOGLEPAY_AUTH=PAN_ONLY
		//	  Google devuelve el número real en vez del token del dispositivo.
		//	  NO arregla "CVN no enviado" (Google Pay no pide CVV nunca). Sirve
		//	  para que Cybersource vea el BIN real: con el token vio uno español
		//	  y saltó "MM-BIN: Card BIN inconsistent with country".
		//	  ⚠️ Puede hacer que Google Pay DESAPAREZCA si la tarjeta del
		//	  comprador solo está tokenizada en el dispositivo.
		//
		//	UNIFIED_CHECKOUT_BILLING_TYPE=FULL
		//	  El widget le pide la dirección real al comprador. Arregla que hoy
		//	  las 200 compras de una noche lleven la MISMA dirección fija, que
		//	  es justo lo que buscan las reglas GVEL de velocidad por dirección.
		//	  ⚠️ Añade fricción al carril que hoy cobra. Es decisión de
		//	  producto, no técnica.
		GooglePayAuthMethods: ucEnvString("UNIFIED_CHECKOUT_GOOGLEPAY_AUTH", ""),
		BillingType:          ucEnvString("UNIFIED_CHECKOUT_BILLING_TYPE", "NONE"),
		RequestEmail:         ucEnvBool("UNIFIED_CHECKOUT_REQUEST_EMAIL"),
		RequestPhone:         ucEnvBool("UNIFIED_CHECKOUT_REQUEST_PHONE"),
	})
	if err != nil {
		log.Printf("[UnifiedCheckout] no se pudo abrir la sesión order=%s: %v", orderNumber, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "No se pudo iniciar el pago con wallet. Usa el pago con tarjeta."})
		return
	}

	log.Printf("[UnifiedCheckout] sesión abierta order=%s total=%.2f %s mandato=%s decisionManager=%v origins=%v",
		orderNumber, total, currency, completeMandate, decisionManager, origins)

	// El JWT viaja tal cual: el SDK lo verifica y saca de dentro la URL de la
	// librería y su hash de integridad. Importe y moneda se devuelven solo para
	// que la web pinte el resumen — los de verdad son los que van FIRMADOS
	// dentro del JWT, no estos.
	//
	// `requires_approval` va informativo: le permite a la web avisar de que el
	// pago se retiene y no se cobra ("se te retendrá el importe hasta que el
	// local confirme"). No es un permiso — el wallet se ofrece en los dos
	// casos.
	c.JSON(http.StatusOK, gin.H{
		"capture_context":   jwt,
		"amount":            total,
		"currency":          currency,
		"order_number":      orderNumber,
		"requires_approval": requiresApproval,
		"complete_mandate":  completeMandate,
		"wallets_eligible":  true,
	})
}
