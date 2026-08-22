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
	"net/http"
	"sort"
	"strings"
	"time"
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
	ClientVersion string
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

	payload := map[string]interface{}{
		"clientVersion":       p.ClientVersion,
		"targetOrigins":       p.TargetOrigins,
		"allowedCardNetworks": p.AllowedCardNetworks,
		"allowedPaymentTypes": p.AllowedPaymentTypes,
		"country":             p.Country,
		"locale":              p.Locale,
		// captureMandate = qué datos le PIDE el componente al comprador.
		// Todo a false a propósito: nombre, correo y dirección de facturación
		// ya están en la orden y son los que mandamos en Sale(). Pedírselos
		// otra vez en la hoja del wallet añade fricción y abre la puerta a que
		// el comprador mande unos datos distintos de los de su reserva.
		"captureMandate": map[string]interface{}{
			"billingType":              "NONE",
			"requestEmail":             false,
			"requestPhone":             false,
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
		payload["completeMandate"] = map[string]interface{}{"type": t}
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
func (c *CybersourceClient) Sale(ctx context.Context, referenceCode string, amount float64, currency string, card CybsCard, billTo CybsBillTo, capture bool, transientToken string) (*CybsSaleResult, error) {
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
			"billTo": map[string]interface{}{
				"firstName":          billTo.FirstName,
				"lastName":           billTo.LastName,
				"email":              billTo.Email,
				"phoneNumber":        billTo.Phone,
				"address1":           billTo.Address1,
				"locality":           billTo.Locality,
				"administrativeArea": billTo.AdminArea,
				"postalCode":         billTo.PostalCode,
				"country":            billTo.Country,
			},
		},
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
