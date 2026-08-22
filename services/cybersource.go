package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// =============================================
// CYBERSOURCE REST CLIENT (Visa Platform Connect / NeoNet)
// Implements HTTP Signature authentication and the payment operations the
// NeoNetProcessor needs: sale (auth+capture), authorization reversal and
// refund. Reference: "Payments Developer Guide — REST API, Visa Platform
// Connect" (PDF in the workspace root).
// =============================================

const (
	cybsHostTest = "apitest.cybersource.com"
	cybsHostProd = "api.cybersource.com"
)

// CybersourceClient is a minimal REST client for the Payments API.
type CybersourceClient struct {
	MerchantID   string
	KeyID        string // REST shared-secret key id (access_key column)
	SharedSecret string // base64 shared secret (secret_key_encrypted column)
	Host         string // apitest.cybersource.com | api.cybersource.com
	client       *http.Client
}

// cybsHTTPClient es COMPARTIDO por todos los clientes Cybersource: reutiliza
// conexiones TLS al host de la pasarela (el http.DefaultTransport da solo 2
// idle/host → bajo carga casi cada cobro reabría TLS). Timeout 18s por
// llamada: 2 llamadas por compra caben en el ctx de 60s con margen, y si la
// pasarela cuelga no atamos una goroutine 30s×2.
var cybsHTTPClient = &http.Client{
	Timeout: 18 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// NewCybersourceClient builds a client. environment "production" targets the
// live host; anything else targets the test host.
func NewCybersourceClient(merchantID, keyID, sharedSecret, environment string) *CybersourceClient {
	host := cybsHostTest
	if strings.EqualFold(environment, "production") || strings.EqualFold(environment, "live") {
		host = cybsHostProd
	}
	return &CybersourceClient{
		MerchantID:   merchantID,
		KeyID:        keyID,
		SharedSecret: sharedSecret,
		Host:         host,
		client:       cybsHTTPClient,
	}
}

// sign builds the HTTP Signature headers for a request.
// Signed headers (POST): host date request-target digest v-c-merchant-id
// Signed headers (GET):  host date request-target v-c-merchant-id
func (c *CybersourceClient) sign(method, path string, body []byte) (map[string]string, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(c.SharedSecret)
	if err != nil {
		return nil, fmt.Errorf("cybersource shared secret is not valid base64: %w", err)
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	target := strings.ToLower(method) + " " + path

	headers := map[string]string{
		"Host":            c.Host,
		"Date":            date,
		"v-c-merchant-id": c.MerchantID,
	}

	signedList := []string{"host", "date", "request-target", "v-c-merchant-id"}
	lines := []string{
		"host: " + c.Host,
		"date: " + date,
		"request-target: " + target,
	}

	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		sum := sha256.Sum256(body)
		digest := "SHA-256=" + base64.StdEncoding.EncodeToString(sum[:])
		headers["Digest"] = digest
		signedList = []string{"host", "date", "request-target", "digest", "v-c-merchant-id"}
		lines = append(lines, "digest: "+digest)
	}
	lines = append(lines, "v-c-merchant-id: "+c.MerchantID)

	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(strings.Join(lines, "\n")))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	headers["Signature"] = fmt.Sprintf(
		`keyid="%s", algorithm="HmacSHA256", headers="%s", signature="%s"`,
		c.KeyID, strings.Join(signedList, " "), signature,
	)
	return headers, nil
}

// doRaw firma y ejecuta la llamada, devolviendo el cuerpo SIN interpretar.
// Existe porque no todas las respuestas de Cybersource son JSON: la de
// /up/v1/capture-contexts es un JWT en texto plano (Content-Type
// application/jwt). Parsearla como JSON devolvería vacío y el error saldría
// mucho más tarde, en el navegador, sin pista de por qué.
func (c *CybersourceClient) doRaw(ctx context.Context, method, path string, payload interface{}) (int, []byte, error) {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
	}

	sigHeaders, err := c.sign(method, path, body)
	if err != nil {
		return 0, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.Host+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range sigHeaders {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("cybersource request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("cybersource response read failed: %w", err)
	}
	return resp.StatusCode, raw, nil
}

func (c *CybersourceClient) do(ctx context.Context, method, path string, payload interface{}) (int, map[string]interface{}, error) {
	status, raw, err := c.doRaw(ctx, method, path, payload)
	if err != nil {
		return 0, nil, err
	}
	// Igual que antes: un cuerpo vacío o no-JSON deja `parsed` a nil y lo
	// resuelve el caller mirando el status. No se convierte en error.
	var parsed map[string]interface{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	return status, parsed, nil
}

// CybsCard holds raw card data collected by our payment page.
type CybsCard struct {
	Number     string
	ExpMonth   string // "12"
	ExpYear    string // "2031"
	SecurityCode string
}

// CybsBillTo is the minimum billing info Cybersource requires.
type CybsBillTo struct {
	FirstName  string
	LastName   string
	Email      string
	Phone      string
	Address1   string
	Locality   string
	AdminArea  string
	PostalCode string
	Country    string
}

// =============================================
// SEÑALES DE DISPOSITIVO — lo que Decision Manager necesita para juzgar
//
// Hasta el 22-ago-2026 NO se mandaba ninguna. En el panel de Decision Manager
// las columnas IP Address, IP Country y Device Fingerprint salían VACÍAS en el
// 100% de nuestras transacciones: el antifraude estaba juzgando a ciegas, y las
// reglas de velocidad de NeoNet —que son compartidas con el resto de sus
// comercios y agrupan por IP y por huella— no tenían con qué distinguir a 200
// compradores distintos.
//
// NOMBRES Y LÍMITES sacados del "REST API Field Reference" que NeoNet nos pasó
// (apartado deviceInformation, págs. 285-297). NO son intercambiables, y dos de
// ellos se confunden con facilidad:
//
//	ipAddress              (45)   IP del comprador. 45 = IPv6 con zona.
//	fingerprintSessionId   (—)    huella del dispositivo. Ver el campo.
//	userAgent              (40)   el TIPO de navegador ("Chrome"), NO la cadena
//	                              User-Agent entera. Mandar la cadena aquí la
//	                              trunca a 40 y ensucia el dato.
//	userAgentBrowserValue  (2048) la cabecera User-Agent ENTERA. Es esta.
//	httpAcceptBrowserValue (255)  la cabecera Accept entera.
//	httpBrowserLanguage    (8)    BCP47 ("es-GT"), NO la cabecera Accept-Language
//	                              entera ("es-GT,es;q=0.9,en;q=0.8" no cabe).
//
// REGLA DE ORO de este bloque: un campo que no venga informado, o que no pase
// su validación, NO VIAJA. Si no viaja ninguno, `deviceInformation` no se añade
// al cuerpo y el pago sale byte a byte como salía antes de que esto existiera.
// Eso es lo que hace que este añadido no pueda romper el carril de tarjeta —
// el único que hoy cobra dinero real. Lo fija TestSaleCardBodyByteIdenticalToLegacy.
// =============================================

// CybsDeviceInfo son las señales del dispositivo del comprador. Todos los
// campos son OPCIONALES y se validan antes de enviarse.
type CybsDeviceInfo struct {
	// IPAddress es la IP REAL del comprador (middleware.GetRealIP). Se descarta
	// si no parsea como IP: mandar basura aquí es peor que no mandar nada,
	// porque Decision Manager la usaría para geolocalizar y decidir.
	IPAddress string

	// FingerprintSessionID es el identificador de la huella de dispositivo
	// (deviceInformation.fingerprintSessionId).
	//
	// ⚠️ NO SE INVENTA AQUÍ, Y ESO ES DELIBERADO. La huella la produce el
	// NAVEGADOR: o bien el script de profiling de Cybersource, que necesita el
	// `org_id` del comercio (NeoNet aún no nos lo ha dado), o bien Unified
	// Checkout cuando la sesión se abre con completeMandate.decisionManager=true
	// —ahí la huella viaja YA DENTRO del transient token, sin pasar por este
	// campo—. Generar un id al azar en el servidor rellenaría la columna del
	// panel con un valor que nunca se perfiló: le estaríamos dando al antifraude
	// un dato falso sobre un dispositivo que no existe. Prefiero la columna
	// vacía a la columna mentirosa.
	//
	// El campo existe para que, en cuanto NeoNet dé el org_id, la web solo tenga
	// que mandarlo en `device_fingerprint_id` (ver payOrderRequest) y esto ya
	// esté enchufado y validado.
	FingerprintSessionID string

	// UserAgent es la cabecera User-Agent ENTERA → userAgentBrowserValue.
	UserAgent string
	// AcceptHeader es la cabecera Accept entera → httpAcceptBrowserValue.
	AcceptHeader string
	// Language es la cabecera Accept-Language cruda; se recorta a la etiqueta
	// BCP47 principal antes de viajar → httpBrowserLanguage.
	Language string
}

// LooksLikeFingerprintSessionID comprueba la FORMA del id de huella antes de
// reenviarlo a la pasarela. Mismo criterio que LooksLikeJWT y por el mismo
// motivo: este valor entra por la petición de pago, o sea que lo escribe el
// navegador, y no vamos a hacer de eco de cualquier cadena hacia Cybersource.
// No valida que la huella EXISTA —eso solo lo sabe quien la perfiló—, solo que
// sea un identificador acotado y sin caracteres raros.
func LooksLikeFingerprintSessionID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// cybsHeaderValue deja una cabecera HTTP en condiciones de viajar dentro de un
// JSON hacia la pasarela: fuera los caracteres de control (un \n o un \r
// colados en un User-Agent no tienen nada que hacer en un campo de antifraude)
// y recorte al límite del campo SIN partir un carácter multibyte por la mitad.
func cybsHeaderValue(v string, max int) string {
	var b strings.Builder
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// cybsBrowserLanguage saca la etiqueta BCP47 principal de una cabecera
// Accept-Language. El campo admite 8 caracteres y una cabecera real trae varias
// etiquetas con pesos ("es-GT,es;q=0.9,en;q=0.8"): mandarla entera la truncaría
// a "es-GT,es" —que no es un idioma— así que se coge solo la primera y se
// comprueba la forma. Si no encaja, no viaja.
func cybsBrowserLanguage(v string) string {
	tag := strings.TrimSpace(v)
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	if i := strings.IndexByte(tag, ';'); i >= 0 {
		tag = tag[:i]
	}
	tag = strings.TrimSpace(tag)
	if tag == "" || len(tag) > 8 {
		return ""
	}
	// Forma: subetiquetas alfanuméricas separadas por guiones ("es", "es-GT").
	// Descarta de paso el comodín "*" y cualquier cosa con caracteres raros.
	for _, part := range strings.Split(tag, "-") {
		if part == "" {
			return ""
		}
		for _, r := range part {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				return ""
			}
		}
	}
	return tag
}

// payload construye el bloque `deviceInformation` SOLO con los campos que vengan
// informados y pasen su validación. Devuelve nil cuando no queda ninguno — y ese
// nil es lo que garantiza que el cuerpo del pago no cambie si no hay señales.
func (d CybsDeviceInfo) payload() map[string]interface{} {
	out := map[string]interface{}{}
	if ip := strings.TrimSpace(d.IPAddress); len(ip) <= 45 && net.ParseIP(ip) != nil {
		out["ipAddress"] = ip
	}
	if fp := strings.TrimSpace(d.FingerprintSessionID); LooksLikeFingerprintSessionID(fp) {
		out["fingerprintSessionId"] = fp
	}
	if ua := cybsHeaderValue(d.UserAgent, 2048); ua != "" {
		out["userAgentBrowserValue"] = ua
	}
	if ac := cybsHeaderValue(d.AcceptHeader, 255); ac != "" {
		out["httpAcceptBrowserValue"] = ac
	}
	if lang := cybsBrowserLanguage(d.Language); lang != "" {
		out["httpBrowserLanguage"] = lang
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CybsSaleResult is the outcome of a sale (auth+capture).
type CybsSaleResult struct {
	Success          bool
	PaymentID        string // Cybersource transaction id (used for reversal/refund)
	Status           string // AUTHORIZED | DECLINED | ...
	AuthCode         string
	CardLast4        string
	// CardBrand solo se rellena en el carril de token (Apple/Google Pay), donde
	// no hay PAN del que deducirla. En el carril de tarjeta queda vacía y la
	// marca la sigue calculando brandFor() a partir del número, como siempre.
	CardBrand        string
	AuthorizedAmount float64 // lo que el emisor aprobó de verdad (parcial < pedido)
	ErrorReason      string
	ErrorMessage     string

	// CaptureState dice si el dinero quedó RETENIDO o LIQUIDADO según la propia
	// respuesta de la pasarela — no según lo que nosotros pedimos. Ver
	// captureStateFromResponse.
	CaptureState CybsCaptureState
	// CaptureEvidence es la señal concreta que produjo ese veredicto, para
	// poder auditarlo en los logs sin volver a llamar a la pasarela.
	CaptureEvidence string
}

// =============================================
// ¿RETENIDO O COBRADO? — verificación de la respuesta
//
// Pedir `capture=false` NO es garantía de que la pasarela obedezca: un perfil
// de merchant configurado como "sale forzado", un wallet que complete la
// sesión con otro mandato, o un cambio de NeoNet pueden liquidar el cobro
// igualmente. Si eso pasa y nosotros anotamos "retenido", al aprobar
// intentaríamos capturar algo ya capturado y al rechazar reversaríamos una
// venta liquidada (no-op) — el comprador se queda cobrado y sin entrada.
//
// Por eso el resultado de Sale() incluye lo que la respuesta DEMUESTRA.
// =============================================

// CybsCaptureState es el veredicto sobre el dinero de una transacción.
type CybsCaptureState string

const (
	// CybsCaptureUnknown: la respuesta no trae ninguna señal fiable. El
	// llamador debe quedarse con lo que pidió y dejar rastro en los logs.
	CybsCaptureUnknown CybsCaptureState = "unknown"
	// CybsCaptureHeld: autorizada y NO liquidada — se puede capturar o reversar.
	CybsCaptureHeld CybsCaptureState = "held"
	// CybsCaptureSettled: ya liquidada. Deshacerla exige REEMBOLSO, no reversa.
	CybsCaptureSettled CybsCaptureState = "settled"
)

// captureStateFromResponse deduce el estado del dinero a partir de `_links`,
// que es la única señal de la API que distingue las dos cosas: `status` vale
// "AUTHORIZED" tanto en una autorización sola como en una venta completa.
//
// Cybersource enlaza SOLO las operaciones que esa transacción todavía admite:
//
//	autorización sola → capture + authReversal   (aún se puede capturar)
//	venta liquidada   → void / refund            (ya no hay nada que capturar)
//
// Se exige que las señales no se contradigan: si aparecieran las dos, el
// veredicto es "unknown" y decide el llamador. Preferimos no saber a afirmar
// algo falso sobre dinero ajeno.
func captureStateFromResponse(resp map[string]interface{}) (CybsCaptureState, string) {
	links, _ := resp["_links"].(map[string]interface{})
	if len(links) == 0 {
		return CybsCaptureUnknown, "la respuesta no trae _links"
	}
	names := make([]string, 0, len(links))
	for k := range links {
		names = append(names, k)
	}
	sort.Strings(names)
	evidence := "_links=" + strings.Join(names, ",")

	has := func(k string) bool { _, ok := links[k]; return ok }
	capturable := has("capture")
	settled := has("void") || has("refund")

	switch {
	case capturable && !settled:
		return CybsCaptureHeld, evidence
	case settled && !capturable:
		return CybsCaptureSettled, evidence
	case !capturable && !settled && has("authReversal"):
		// Reversable pero sin enlace de captura: sigue siendo una autorización
		// viva, no una venta liquidada.
		return CybsCaptureHeld, evidence
	}
	return CybsCaptureUnknown, evidence
}

// cardTypeFor maps a PAN prefix to Cybersource's card type codes.
func cardTypeFor(number string) string {
	switch {
	case strings.HasPrefix(number, "4"):
		return "001" // Visa
	case strings.HasPrefix(number, "5"), strings.HasPrefix(number, "2"):
		return "002" // Mastercard
	case strings.HasPrefix(number, "34"), strings.HasPrefix(number, "37"):
		return "003" // Amex
	default:
		return ""
	}
}

// cybsBrandForType hace el camino inverso a cardTypeFor: del código que
// devuelve Cybersource al nombre de marca que guardamos en la orden. Se usa
// SOLO en el carril de token (Apple/Google Pay), donde no hay PAN del que
// deducir la marca.
func cybsBrandForType(code string) string {
	switch code {
	case "001":
		return "visa"
	case "002":
		return "mastercard"
	case "003":
		return "amex"
	default:
		return ""
	}
}

// LooksLikeJWT comprueba la FORMA de un JWT (tres segmentos base64url
// separados por puntos). No valida la firma — eso solo lo puede hacer quien
// lo emitió. Sirve para dos cosas defensivas:
//   - no dar por buena una respuesta de capture-context que llegue vacía o
//     con un error en texto plano;
//   - no reenviar a la pasarela cualquier cosa que el navegador nos mande en
//     el campo del token.
func LooksLikeJWT(s string) bool {
	if s == "" || len(s) > 8192 {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '='
			if !ok {
				return false
			}
		}
	}
	return true
}

// =============================================
// UNIFIED CHECKOUT — capture context
//
// Unified Checkout es el componente de Cybersource que pinta los botones de
// Apple Pay / Google Pay. El navegador NO puede pedirlo por su cuenta: primero
// el servidor abre un "capture context" (una sesión firmada, con el importe y
// los orígenes autorizados dentro) y le pasa ese JWT al SDK.
//
// Lo que devuelve el comprador después NO es una tarjeta, es un "transient
// token": un JWT de un solo uso y vida corta que representa el medio de pago.
// Ese token es lo que viaja a Sale(). El PAN nunca pasa por aquí.
// =============================================

// CaptureContextParams describe la sesión de Unified Checkout que se abre.
// El importe y la moneda SIEMPRE los pone el servidor leyéndolos de la orden:
// van firmados dentro del JWT, así que si viniesen del navegador cualquiera
// podría pagar 1 GTQ por una entrada de 300.
type CaptureContextParams struct {
	// TargetOrigins son los orígenes (esquema+host+puerto, SIN ruta) donde se
	// puede embeber el checkout. Cybersource rechaza la sesión si el iframe
	// carga desde otro sitio: es lo que impide que un clon del sitio use
	// nuestra pasarela.
	TargetOrigins []string
	Amount        float64
	Currency      string
	Country       string
	Locale        string
	// AllowedPaymentTypes: APPLEPAY, GOOGLEPAY, PANENTRY, CLICKTOPAY...
	AllowedPaymentTypes []string
	AllowedCardNetworks []string
	// ClientVersion es la versión del SDK de Unified Checkout. Configurable
	// porque la fija Cybersource/NeoNet, no nosotros.
	//
	// ⚠️ NO ES UN NÚMERO COSMÉTICO: cada versión habilita campos del cuerpo, y
	// una versión vieja IGNORA EN SILENCIO los que no conoce. Ver
	// defaultUCClientVersion en unified_checkout_controller.go — mandar
	// `completeMandate` con clientVersion 0.24 fue exactamente eso.
	ClientVersion string

	// GooglePayAuthMethods restringe qué credenciales devuelve Google Pay
	// (manual de Unified Checkout, pág. 73-74):
	//
	//	"PAN_ONLY"        → Google devuelve el número de tarjeta real
	//	"CRYPTOGRAM_3DS"  → Google devuelve el token de red del dispositivo
	//	""                → las dos, y elige Google. ES EL POR DEFECTO.
	//
	// Vacío = GOOGLEPAY viaja como texto suelto, igual que siempre. Con valor,
	// se emite en su forma de objeto.
	//
	// NO sirve para esquivar la regla "CVN no enviado" de NeoNet: Google Pay no
	// pide CVV en ninguno de los dos modos. Sirve para el BIN — con PAN_ONLY,
	// Cybersource ve el BIN real de la tarjeta en vez del del token, que es lo
	// que hizo saltar `MM-BIN: Card BIN inconsistent with country`.
	GooglePayAuthMethods string

	// BillingType dice QUÉ DATOS le pide el widget al comprador: "NONE" (nada),
	// "PARTIAL" o "FULL" (dirección de facturación completa).
	//
	// Está en NONE porque nombre, correo y dirección ya viajan en Sale() desde
	// la orden, y pedírselos otra vez es fricción en el peor momento.
	//
	// PERO hay un motivo real para subirlo a FULL: hoy la dirección que
	// mandamos es FIJA para todos los compradores ("Ciudad de Guatemala",
	// "01001"). Con FULL, el comprador teclea la suya y esa repetición
	// desaparece — que importa porque el perfil de NeoNet lleva reglas de
	// velocidad por dirección (GVEL-R5038 "Misma Direccion Ent > 2 x 1 semana").
	//
	// Vacío = "NONE".
	BillingType string
	// RequestEmail / RequestPhone: mismo razonamiento que BillingType. El
	// ejemplo oficial de "Unified Checkout with Sale and Decision Manager"
	// (pág. 119) los pone a true; nosotros a false porque ya los tenemos.
	RequestEmail bool
	RequestPhone bool
	// ReferenceCode se guarda en la sesión para poder conciliar en el Business
	// Center qué orden abrió cada capture context.
	ReferenceCode string

	// CompleteMandateType declara ANTE LA PASARELA qué se va a hacer con el
	// dinero de esta sesión:
	//
	//	"CAPTURE" → cobrar al momento          (evento público)
	//	"AUTH"    → autorizar ahora, capturar después (evento privado: la
	//	            retención que el local aprueba o rechaza en 48 h)
	//
	// Documentación de NeoNet (Payments Developer Guide — REST API, Visa
	// Platform Connect, pág. 36): completeMandate.type acepta AUTH =
	// "Authorize the payment and capture the funds at a later date"; la pág. 31
	// trae el ejemplo "Requesting an Authorization with a Transient Token".
	//
	// ⚠️ NO ES `captureMandate`. Los nombres se parecen y significan cosas sin
	// relación: `captureMandate` (más abajo en este mismo cuerpo) dice qué
	// DATOS le pide el widget al comprador —facturación, correo, teléfono—; el
	// que habla de dinero es ESTE.
	//
	// Vacío = no se manda el bloque, y el cuerpo queda idéntico al de antes de
	// que este campo existiera.
	CompleteMandateType string

	// DecisionManager enciende el antifraude Y —lo que de verdad nos importa—
	// LA HUELLA DE DISPOSITIVO dentro del componente de Unified Checkout.
	//
	// CONFIRMADO EN LA DOCUMENTACIÓN QUE NEONET NOS PASÓ, no deducido: "Unified
	// Checkout" (completeMandate.decisionManager) y "Digital Accept Secure
	// Integration" dicen lo mismo con las mismas palabras — "When this field is
	// set to true, both Decision Manager and device fingerprinting services are
	// run. [...] When this field is set to false or is not included in the
	// request, Decision Manager and device fingerprinting services do not run."
	//
	// O sea que hasta hoy, en el carril de wallet, NO se ejecutaba el
	// fingerprinting: por eso la columna Device Fingerprint del panel sale vacía
	// también ahí. Con esto encendido, Unified Checkout perfila el dispositivo y
	// mete el `fingerprintSessionId` DENTRO del transient token (se ve en el
	// ejemplo de token descifrado del manual), así que llega a la pasarela solo
	// —no hace falta que la web nos lo mande ni que nosotros lo reenviemos.
	//
	// La misma documentación exige que, para usar este campo, vaya acompañado de
	// completeMandate.type. Por eso solo se emite cuando hay mandato.
	DecisionManager bool
}

// allowedPaymentTypesPayload monta el array `allowedPaymentTypes`.
//
// Normalmente cada método es un texto suelto ("APPLEPAY", "GOOGLEPAY"). Pero
// Google Pay admite una forma expandida de objeto para restringir qué
// credenciales devuelve (manual pág. 73):
//
//	{ "type": "GOOGLEPAY", "options": { "allowedAuthMethods": "PAN_ONLY" } }
//
// Solo GOOGLEPAY se expande, y solo si se pidió: sin `authMethods` el array
// sale exactamente igual que antes de que esta función existiera, que es la
// condición para que esto no pueda romper el carril que hoy cobra.
//
// ⚠️ El ejemplo del manual está mal escrito (le falta una coma después de
// "PANENTRY"). La forma correcta es la de aquí: elementos del array separados
// por comas, unos texto y otros objeto.
func allowedPaymentTypesPayload(types []string, googlePayAuthMethods string) []interface{} {
	authMethods := strings.ToUpper(strings.TrimSpace(googlePayAuthMethods))
	out := make([]interface{}, 0, len(types))
	for _, t := range types {
		t = strings.ToUpper(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if t == "GOOGLEPAY" && authMethods != "" {
			out = append(out, map[string]interface{}{
				"type":    "GOOGLEPAY",
				"options": map[string]interface{}{"allowedAuthMethods": authMethods},
			})
			continue
		}
		out = append(out, t)
	}
	return out
}

// CaptureContext abre una sesión de Unified Checkout.
//
// ⚠️ LA RESPUESTA NO ES JSON. Cybersource contesta con el JWT en texto plano
// (Content-Type: application/jwt). Se devuelve TAL CUAL, sin tocarlo: el SDK
// del navegador lo necesita entero y cualquier reserialización lo invalidaría.
func (c *CybersourceClient) CaptureContext(ctx context.Context, p CaptureContextParams) (string, error) {
	if len(p.TargetOrigins) == 0 {
		return "", fmt.Errorf("capture context: targetOrigins vacío (revisa ALLOWED_ORIGINS / UNIFIED_CHECKOUT_TARGET_ORIGINS)")
	}
	if p.Amount <= 0 {
		return "", fmt.Errorf("capture context: importe inválido")
	}

	billingType := strings.ToUpper(strings.TrimSpace(p.BillingType))
	if billingType == "" {
		billingType = "NONE"
	}

	payload := map[string]interface{}{
		"clientVersion":       p.ClientVersion,
		"targetOrigins":       p.TargetOrigins,
		"allowedCardNetworks": p.AllowedCardNetworks,
		"allowedPaymentTypes": allowedPaymentTypesPayload(p.AllowedPaymentTypes, p.GooglePayAuthMethods),
		"country":             p.Country,
		"locale":              p.Locale,
		// captureMandate = qué datos le PIDE el componente al comprador. Por
		// defecto no pide nada: nombre, correo y dirección ya están en la orden
		// y son los que mandamos en Sale(). Pedírselos otra vez en la hoja del
		// wallet añade fricción y abre la puerta a que el comprador mande unos
		// datos distintos de los de su reserva.
		//
		// Configurable porque la dirección que mandamos hoy es la MISMA para
		// todos, y eso alimenta las reglas de velocidad por dirección del
		// perfil de NeoNet. Ver CaptureContextParams.BillingType.
		"captureMandate": map[string]interface{}{
			"billingType":              billingType,
			"requestEmail":             p.RequestEmail,
			"requestPhone":             p.RequestPhone,
			"requestShipping":          false,
			"showAcceptedNetworkIcons": true,
		},
		"orderInformation": map[string]interface{}{
			"amountDetails": map[string]interface{}{
				"totalAmount": fmt.Sprintf("%.2f", p.Amount),
				"currency":    p.Currency,
			},
		},
	}
	// completeMandate = la INTENCIÓN sobre el dinero (CAPTURE = cobrar ahora,
	// AUTH = autorizar y capturar después). La pone el llamador leyendo el
	// evento, para que no pueda divergir de lo que después se le pide a Sale().
	if t := strings.ToUpper(strings.TrimSpace(p.CompleteMandateType)); t != "" {
		mandate := map[string]interface{}{"type": t}
		// La doc condiciona decisionManager a que exista `type`, así que vive
		// dentro de este if a propósito: fuera, un DecisionManager=true sin
		// mandato produciría un cuerpo que la pasarela no acepta.
		if p.DecisionManager {
			mandate["decisionManager"] = true
		}
		payload["completeMandate"] = mandate
	}
	if p.ReferenceCode != "" {
		payload["clientReferenceInformation"] = map[string]interface{}{"code": p.ReferenceCode}
	}

	status, raw, err := c.doRaw(ctx, http.MethodPost, "/up/v1/capture-contexts", payload)
	if err != nil {
		return "", err
	}
	jwt := strings.TrimSpace(string(raw))
	if status != http.StatusOK && status != http.StatusCreated {
		// El cuerpo de error SÍ es JSON. Se recorta antes de propagarlo: no
		// lleva secretos, pero tampoco hace falta volcar kilobytes en el log.
		snippet := jwt
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[Cybersource] capture-context HTTP %d ref=%s body=%s", status, p.ReferenceCode, snippet)
		return "", fmt.Errorf("capture context HTTP %d", status)
	}
	if !LooksLikeJWT(jwt) {
		// 200 con un cuerpo que no es un JWT = integración mal configurada
		// (merchant sin Unified Checkout habilitado, por ejemplo). Mejor
		// fallar aquí que servirle al navegador algo que no va a arrancar.
		log.Printf("[Cybersource] capture-context devolvió algo que NO es un JWT (len=%d) ref=%s", len(jwt), p.ReferenceCode)
		return "", fmt.Errorf("capture context: respuesta inesperada de la pasarela")
	}
	return jwt, nil
}

// Sale authorizes a card. When capture is true it's an auth+capture (charges
// now — public events); when false it's authorization-only, which HOLDS the
// funds without charging until a follow-on Capture (private/approval events).
//
// transientToken (Unified Checkout / wallets): cuando llega informado, el
// medio de pago viaja como `tokenInformation.transientTokenJwt` EN LUGAR de
// `paymentInformation.card`, y `card` se ignora por completo. Cuando llega
// vacío —el 100% del tráfico de hoy— el cuerpo enviado es EXACTAMENTE el de
// siempre. Esa es la condición de que este añadido no pueda romper el único
// carril que hoy cobra de verdad.
//
// device (señales de dispositivo para Decision Manager): mismo contrato. Cada
// campo viaja solo si viene informado Y pasa su validación; si no queda
// ninguno, `deviceInformation` NO se añade al cuerpo. Ver CybsDeviceInfo.
func (c *CybersourceClient) Sale(ctx context.Context, referenceCode string, amount float64, currency string, card CybsCard, billTo CybsBillTo, capture bool, transientToken string, device CybsDeviceInfo) (*CybsSaleResult, error) {
	bill := map[string]interface{}{
		"firstName":          billTo.FirstName,
		"lastName":           billTo.LastName,
		"email":              billTo.Email,
		"address1":           billTo.Address1,
		"locality":           billTo.Locality,
		"administrativeArea": billTo.AdminArea,
		"postalCode":         billTo.PostalCode,
		"country":            billTo.Country,
	}
	// El teléfono viaja SOLO si lo tenemos de verdad. Antes se mandaba un
	// literal ("502" + ocho ceros) idéntico en todas las compras de todos los
	// venues; ver `telefonoDelComprador` en pay_controller.go. Un campo ausente
	// es un dato que no tenemos; un campo con un número inventado y compartido
	// es una firma de fraude que le regalábamos al antifraude de NeoNet.
	if p := strings.TrimSpace(billTo.Phone); p != "" {
		bill["phoneNumber"] = p
	}

	payload := map[string]interface{}{
		"clientReferenceInformation": map[string]interface{}{"code": referenceCode},
		"processingInformation": map[string]interface{}{
			"capture":           capture,
			"commerceIndicator": "internet",
		},
		"orderInformation": map[string]interface{}{
			"amountDetails": map[string]interface{}{
				"totalAmount": fmt.Sprintf("%.2f", amount),
				"currency":    currency,
			},
			"billTo": bill,
		},
	}

	// SEÑALES DE DISPOSITIVO. Se añaden ANTES de elegir el medio de pago porque
	// aplican a los dos carriles por igual: la regla de velocidad que rechaza no
	// distingue entre tarjeta y wallet. Si no hay ninguna señal válida, esta
	// clave no aparece y el cuerpo es el de siempre.
	if dev := device.payload(); dev != nil {
		payload["deviceInformation"] = dev
	}

	// EL MEDIO DE PAGO: token O tarjeta, nunca los dos. Mandar ambos hace que
	// Cybersource rechace la petición, así que la bifurcación es excluyente.
	if transientToken != "" {
		payload["tokenInformation"] = map[string]interface{}{
			"transientTokenJwt": transientToken,
		}
		// NOTA para quien integre wallets con NeoNet: si su perfil de Visa
		// Platform Connect exigiera identificar la solución de pago, aquí es
		// donde iría processingInformation.paymentSolution ("001" Apple Pay,
		// "012" Google Pay). Con el transient token de Unified Checkout suele
		// venir ya dentro del token; no se manda hasta que se compruebe en
		// sandbox que hace falta.
	} else {
		payload["paymentInformation"] = map[string]interface{}{
			"card": map[string]interface{}{
				"number":          card.Number,
				"expirationMonth": card.ExpMonth,
				"expirationYear":  card.ExpYear,
				"securityCode":    card.SecurityCode,
				"type":            cardTypeFor(card.Number),
			},
		}
	}

	status, resp, err := c.do(ctx, http.MethodPost, "/pts/v2/payments", payload)
	if err != nil {
		return nil, err
	}

	result := &CybsSaleResult{
		PaymentID: GetString(resp, "id"),
		Status:    GetString(resp, "status"),
	}
	if proc, ok := resp["processorInformation"].(map[string]interface{}); ok {
		result.AuthCode = GetString(proc, "approvalCode")
	}
	if n := len(card.Number); n >= 4 {
		result.CardLast4 = card.Number[n-4:]
	}
	// Con token no hay PAN del que sacar los 4 últimos ni la marca: los da la
	// respuesta. Solo se mira en el carril de token — en el de tarjeta esto no
	// se ejecuta y el resultado es idéntico al de siempre.
	if transientToken != "" {
		if pi, ok := resp["paymentInformation"].(map[string]interface{}); ok {
			src, _ := pi["tokenizedCard"].(map[string]interface{})
			if src == nil {
				src, _ = pi["card"].(map[string]interface{})
			}
			if src != nil {
				if suffix := GetString(src, "suffix"); suffix != "" {
					result.CardLast4 = suffix
				}
				result.CardBrand = cybsBrandForType(GetString(src, "type"))
			}
		}
	}

	// AUTORIZACIÓN PARCIAL (tarjetas prepago, o el trigger "SDISCOUNT" del
	// sandbox de VisaNet GT, que con ciertos importes autoriza el 80%): el
	// emisor aprueba MENOS de lo pedido. NO se decide aquí — se expone el
	// importe autorizado y el CALLER elige (la parte del venue se rechaza y
	// reversa; el fee de Pull se acepta recortado y se captura lo autorizado,
	// porque matar una venta entera por el fee es peor negocio).
	result.AuthorizedAmount = amount
	if oi, ok := resp["orderInformation"].(map[string]interface{}); ok {
		if ad, ok := oi["amountDetails"].(map[string]interface{}); ok {
			if auth := GetFloat64(ad, "authorizedAmount"); auth > 0 {
				result.AuthorizedAmount = auth
			}
			if result.AuthorizedAmount < amount-0.005 {
				log.Printf("[Cybersource] PARTIAL AUTH ref=%s pedido=%.2f autorizado=%.2f",
					referenceCode, amount, result.AuthorizedAmount)
			}
		}
	}
	// ¿RETENIDO O COBRADO? Se calcula SIEMPRE (es lectura pura de la respuesta,
	// no cambia el cuerpo enviado ni ninguna decisión de este método), pero solo
	// importa cuando se pidió retención: ahí el llamador tiene que anotar en la
	// orden lo que pasó de verdad, no lo que pidió.
	result.CaptureState, result.CaptureEvidence = captureStateFromResponse(resp)

	// Diagnóstico: cuando el status no es el AUTHORIZED de manual, dejar en
	// logs qué devolvió la pasarela (sin datos de tarjeta) para conciliar.
	if result.Status != "AUTHORIZED" {
		oiJSON, _ := json.Marshal(resp["orderInformation"])
		procJSON, _ := json.Marshal(resp["processorInformation"])
		log.Printf("[Cybersource] status=%s ref=%s orderInformation=%s processorInformation=%s",
			result.Status, referenceCode, oiJSON, procJSON)
	}

	// 201 + AUTHORIZED es el caso normal; el sandbox de VisaNet GT devuelve
	// a veces 201 + ACCEPTED en autorizaciones (visto en la 2ª auth del par
	// atómico con capture=false) — es una aprobación, no un rechazo. Todo lo
	// demás (DECLINED, INVALID_REQUEST, AUTHORIZED_RISK_DECLINED...) cae al
	// camino de rechazo de abajo.
	if status == 201 && (result.Status == "AUTHORIZED" || result.Status == "ACCEPTED") {
		result.Success = true
		// Solo cuando se pidió RETENER: dejar en el log lo que la pasarela
		// demuestra que hizo. El llamador decide qué anotar en la orden; aquí
		// se registra la evidencia cruda para poder conciliar en el Business
		// Center sin volver a preguntar.
		if !capture {
			linksJSON, _ := json.Marshal(resp["_links"])
			switch result.CaptureState {
			case CybsCaptureSettled:
				log.Printf("[Cybersource] ALERTA: se pidió RETENCIÓN (capture=false) y la pasarela LIQUIDÓ el cobro ref=%s id=%s status=%s %s links=%s",
					referenceCode, result.PaymentID, result.Status, result.CaptureEvidence, linksJSON)
			case CybsCaptureUnknown:
				log.Printf("[Cybersource] retención SIN VERIFICAR ref=%s id=%s status=%s (%s) links=%s",
					referenceCode, result.PaymentID, result.Status, result.CaptureEvidence, linksJSON)
			}
		}
		return result, nil
	}

	// Declined / error paths — surface reason without leaking internals.
	if errInfo, ok := resp["errorInformation"].(map[string]interface{}); ok {
		result.ErrorReason = GetString(errInfo, "reason")
		result.ErrorMessage = GetString(errInfo, "message")
	}
	if result.ErrorMessage == "" {
		result.ErrorMessage = "Pago rechazado (" + result.Status + ")"
	}
	log.Printf("[Cybersource] sale NOT approved ref=%s http=%d status=%s reason=%s",
		referenceCode, status, result.Status, result.ErrorReason)
	return result, nil
}

// Reverse releases an authorization (used to undo the first sale when the
// second one of the atomic pair fails).
func (c *CybersourceClient) Reverse(ctx context.Context, paymentID, referenceCode string, amount float64, currency string) error {
	payload := map[string]interface{}{
		"clientReferenceInformation": map[string]interface{}{"code": referenceCode},
		"reversalInformation": map[string]interface{}{
			"amountDetails": map[string]interface{}{"totalAmount": fmt.Sprintf("%.2f", amount)},
			"reason":        "atomic pair rollback",
		},
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/pts/v2/payments/"+paymentID+"/reversals", payload)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("reversal HTTP %d status=%s", status, GetString(resp, "status"))
	}
	log.Printf("[Cybersource] reversal OK ref=%s status=%s", referenceCode, GetString(resp, "status"))
	return nil
}

// Capture settles a previously authorized (capture=false) payment. Used to
// charge a held authorization when staff approves a private-event order.
func (c *CybersourceClient) Capture(ctx context.Context, paymentID, referenceCode string, amount float64, currency string) error {
	payload := map[string]interface{}{
		"clientReferenceInformation": map[string]interface{}{"code": referenceCode},
		"orderInformation": map[string]interface{}{
			"amountDetails": map[string]interface{}{
				"totalAmount": fmt.Sprintf("%.2f", amount),
				"currency":    currency,
			},
		},
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/pts/v2/payments/"+paymentID+"/captures", payload)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("capture HTTP %d status=%s", status, GetString(resp, "status"))
	}
	// El status del body queda en logs para conciliar contra el EBC
	// (capturas suelen volver PENDING hasta el settlement del batch).
	log.Printf("[Cybersource] capture OK ref=%s status=%s", referenceCode, GetString(resp, "status"))
	return nil
}

// Refund issues a follow-on refund of a captured sale.
func (c *CybersourceClient) Refund(ctx context.Context, paymentID, referenceCode string, amount float64, currency string) error {
	payload := map[string]interface{}{
		"clientReferenceInformation": map[string]interface{}{"code": referenceCode},
		"orderInformation": map[string]interface{}{
			"amountDetails": map[string]interface{}{
				"totalAmount": fmt.Sprintf("%.2f", amount),
				"currency":    currency,
			},
		},
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/pts/v2/payments/"+paymentID+"/refunds", payload)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("refund HTTP %d status=%s", status, GetString(resp, "status"))
	}
	log.Printf("[Cybersource] refund OK ref=%s status=%s", referenceCode, GetString(resp, "status"))
	return nil
}
