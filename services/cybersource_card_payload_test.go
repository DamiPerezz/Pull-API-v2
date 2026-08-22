package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// =============================================================================
// EL CARRIL QUE HOY COBRA DINERO REAL NO PUEDE HABER CAMBIADO
//
// Sale() ganó un parámetro (transientToken) para poder cobrar con Apple/Google
// Pay. Ese añadido solo vale si el cuerpo que se le manda a Cybersource en un
// pago CON TARJETA sigue siendo EXACTAMENTE el mismo — byte a byte, no
// "equivalente". Un campo de más, un campo que se movió, y estamos probando en
// producción con el dinero de otros.
//
// Estos tests fijan eso: el cuerpo esperado se escribe aquí a mano, copiado del
// código ANTERIOR al cambio (git show HEAD:services/cybersource.go), y se
// compara contra lo que sale hoy por el cable.
// =============================================================================

// roundTripFunc intercepta la petición HTTP sin abrir ningún puerto: se le
// enchufa al http.Client del cliente de Cybersource y devuelve una respuesta
// de mentira. Nada sale de este proceso.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// captureSaleBody ejecuta un Sale() contra un transporte falso y devuelve el
// cuerpo crudo que se habría enviado a la pasarela.
//
// device se añadió cuando Sale() aprendió a mandar señales de dispositivo. El
// valor CERO (CybsDeviceInfo{}) es el que reproduce el comportamiento anterior,
// y es el que usan los tests de identidad byte a byte.
func captureSaleBody(t *testing.T, card CybsCard, billTo CybsBillTo, capture bool, token string, device CybsDeviceInfo, respBody string) (*CybsSaleResult, []byte, string) {
	t.Helper()

	var sent []byte
	var sentPath string
	cli := NewCybersourceClient("merchant-test", "key-id-test", "c2hhcmVkLXNlY3JldA==", "test")
	cli.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			sent = body
			sentPath = r.URL.Path
			return &http.Response{
				StatusCode: 201,
				Body:       io.NopCloser(strings.NewReader(respBody)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}),
	}

	res, err := cli.Sale(context.Background(), "ORD-1-VENUE", 350.75, "GTQ", card, billTo, capture, token, device)
	if err != nil {
		t.Fatalf("Sale devolvió error: %v", err)
	}
	return res, sent, sentPath
}

func testCard() CybsCard {
	return CybsCard{Number: "4111111111111111", ExpMonth: "12", ExpYear: "2030", SecurityCode: "123"}
}

func testBillTo() CybsBillTo {
	return CybsBillTo{
		FirstName: "Damián", LastName: "Pérez", Email: "d@example.com", Phone: "+50255555555",
		Address1: "Zona 10", Locality: "Guatemala", AdminArea: "GT", PostalCode: "01010", Country: "GT",
	}
}

// legacySalePayload es una COPIA LITERAL del payload que construía Sale() antes
// de que existiera el carril de wallet (git show HEAD:services/cybersource.go).
// No se refactoriza ni se comparte con el código de producción a propósito: si
// alguien toca Sale(), este bloque NO se mueve con él y el test salta.
func legacySalePayload(referenceCode string, amount float64, currency string, card CybsCard, billTo CybsBillTo, capture bool) map[string]interface{} {
	return map[string]interface{}{
		"clientReferenceInformation": map[string]interface{}{"code": referenceCode},
		"processingInformation": map[string]interface{}{
			"capture":           capture,
			"commerceIndicator": "internet",
		},
		"paymentInformation": map[string]interface{}{
			"card": map[string]interface{}{
				"number":          card.Number,
				"expirationMonth": card.ExpMonth,
				"expirationYear":  card.ExpYear,
				"securityCode":    card.SecurityCode,
				"type":            cardTypeFor(card.Number),
			},
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
}

// TestSaleCardBodyByteIdenticalToLegacy — LA PRUEBA CENTRAL.
// Con transientToken vacío (el 100% del tráfico con el interruptor apagado) el
// cuerpo tiene que salir idéntico al de antes del cambio, en los dos modos:
// capture=true (público, se cobra) y capture=false (privado, se retiene).
//
// AMPLIADA (22-ago-2026) cuando Sale() aprendió a mandar `deviceInformation`:
// sigue demostrando exactamente lo mismo —el cuerpo del pago con tarjeta no
// cambió— y ahora además fija la condición que lo hace cierto: si no hay
// señales de dispositivo VÁLIDAS, la clave `deviceInformation` NO aparece.
// Por eso se prueban dos formas de "sin señales":
//
//	· el valor cero, que es lo que se manda desde cualquier llamador que no las
//	  rellene;
//	· un struct LLENO DE BASURA, porque un campo informado pero inválido tiene
//	  que descartarse — si se colara, el cuerpo cambiaría y además le estaríamos
//	  dando datos falsos al antifraude.
func TestSaleCardBodyByteIdenticalToLegacy(t *testing.T) {
	sinSenales := map[string]CybsDeviceInfo{
		"sin señales (valor cero)": {},
		"señales informadas pero TODAS inválidas": {
			IPAddress:            "no-es-una-ip",
			FingerprintSessionID: "corta", // < 8 caracteres
			UserAgent:            "\x00\x01\x02",
			AcceptHeader:         "   ",
			Language:             "*",
		},
	}
	for devName, device := range sinSenales {
		for _, capture := range []bool{true, false} {
			name := devName + " / capture=false (privado, retencion)"
			if capture {
				name = devName + " / capture=true (publico, cobro)"
			}
			t.Run(name, func(t *testing.T) {
				_, sent, path := captureSaleBody(t, testCard(), testBillTo(), capture, "", device,
					`{"id":"tx1","status":"AUTHORIZED","_links":{"capture":{"href":"/x"}}}`)

				want, err := json.Marshal(legacySalePayload("ORD-1-VENUE", 350.75, "GTQ", testCard(), testBillTo(), capture))
				if err != nil {
					t.Fatalf("no se pudo serializar el payload de referencia: %v", err)
				}

				if path != "/pts/v2/payments" {
					t.Fatalf("la ruta cambió: %q (esperada /pts/v2/payments)", path)
				}
				if string(sent) != string(want) {
					t.Fatalf("EL CUERPO DEL PAGO CON TARJETA CAMBIÓ.\n esperado: %s\n obtenido: %s", want, sent)
				}
			})
		}
	}
}

// TestSaleCardResultUnchanged — el resultado que lee el llamador en el carril
// de tarjeta tampoco cambia: los 4 últimos salen del PAN y CardBrand queda
// VACÍA (la marca la sigue calculando brandFor() a partir del número, como
// siempre). Si CardBrand se rellenara aquí, machacaría a brandFor().
func TestSaleCardResultUnchanged(t *testing.T) {
	res, _, _ := captureSaleBody(t, testCard(), testBillTo(), true, "", CybsDeviceInfo{},
		`{"id":"tx1","status":"AUTHORIZED",
		  "paymentInformation":{"card":{"suffix":"9999","type":"002"}},
		  "_links":{"void":{"href":"/x"}}}`)

	if res.CardLast4 != "1111" {
		t.Fatalf("CardLast4 = %q, esperado 1111 (los 4 últimos del PAN, como siempre)", res.CardLast4)
	}
	if res.CardBrand != "" {
		t.Fatalf("CardBrand = %q; en el carril de tarjeta tiene que quedar vacía para que la calcule brandFor()", res.CardBrand)
	}
	if !res.Success {
		t.Fatal("201 + AUTHORIZED tiene que ser éxito")
	}
}

// TestSaleTokenBodyExcludesCard — el carril nuevo: con token viaja
// tokenInformation y NO paymentInformation. Mandar los dos hace que Cybersource
// rechace la petición por ambigua.
func TestSaleTokenBodyExcludesCard(t *testing.T) {
	token := "aaa.bbb.ccc"
	res, sent, _ := captureSaleBody(t, testCard(), testBillTo(), false, token, CybsDeviceInfo{},
		`{"id":"tx1","status":"AUTHORIZED",
		  "paymentInformation":{"tokenizedCard":{"suffix":"4242","type":"002"}},
		  "_links":{"capture":{"href":"/x"},"authReversal":{"href":"/y"}}}`)

	var body map[string]interface{}
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatalf("cuerpo no serializable: %v", err)
	}
	if _, ok := body["paymentInformation"]; ok {
		t.Fatal("con token NO puede viajar paymentInformation.card")
	}
	ti, ok := body["tokenInformation"].(map[string]interface{})
	if !ok || ti["transientTokenJwt"] != token {
		t.Fatalf("falta tokenInformation.transientTokenJwt: %s", sent)
	}
	// El resto del cuerpo (importe, moneda, billTo, capture) es el mismo que en
	// el carril de tarjeta: el token SUSTITUYE al PAN, no cambia los parámetros.
	legacy := legacySalePayload("ORD-1-VENUE", 350.75, "GTQ", testCard(), testBillTo(), false)
	for _, key := range []string{"clientReferenceInformation", "processingInformation", "orderInformation"} {
		wantJSON, _ := json.Marshal(legacy[key])
		gotJSON, _ := json.Marshal(body[key])
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("%s difiere entre carriles.\n tarjeta: %s\n wallet:  %s", key, wantJSON, gotJSON)
		}
	}
	if res.CardLast4 != "4242" || res.CardBrand != "mastercard" {
		t.Fatalf("con token los 4 últimos y la marca salen de la respuesta: last4=%q brand=%q", res.CardLast4, res.CardBrand)
	}
	if res.CaptureState != CybsCaptureHeld {
		t.Fatalf("CaptureState = %q, esperado held", res.CaptureState)
	}
}

// TestAuthorizationNeverReadsAsSettled — EL CARRIL DE TARJETA CON EL
// INTERRUPTOR APAGADO.
//
// El guard de verificación de captura NO está detrás del interruptor de
// wallets: se aplica también a un pago con tarjeta de evento privado, que es
// dinero real hoy. El único veredicto que cambiaría el comportamiento de
// siempre es "settled" (haría que aprobar NO capturase → el local nunca cobra).
//
// Así que la pregunta que decide si ese guard es seguro en el carril de tarjeta
// es: ¿puede una AUTORIZACIÓN leerse como venta liquidada? Estas son todas las
// formas de `_links` que aparecen en el manual de NeoNet (Payments Developer
// Guide — REST API, Visa Platform Connect) para una autorización. Ninguna puede
// dar "settled": o dan "held" (lo mismo que se pidió) o "unknown" (que también
// se queda con lo que se pidió). Es decir, con el interruptor apagado
// `captured` sigue valiendo exactamente lo que valía antes.
func TestAuthorizationNeverReadsAsSettled(t *testing.T) {
	// Formas reales de `_links` en respuestas de autorización del manual.
	authShapes := []string{
		`{"_links":{"self":{"href":"/x"},"capture":{"href":"/x/captures"},"authReversal":{"href":"/x/reversals"}}}`,
		`{"_links":{"capture":{"href":"/x/captures"}}}`,
		`{"_links":{"authReversal":{"href":"/x/reversals"}}}`,
		`{"_links":{"authReversal":{"href":"/x/reversals"},"self":{"href":"/x"}}}`,
		`{"_links":{"self":{"href":"/x"}}}`, // sin señales → unknown
		`{}`,                                // sin _links → unknown
	}
	for _, body := range authShapes {
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("muestra inválida: %v", err)
		}
		state, evidence := captureStateFromResponse(resp)
		if state == CybsCaptureSettled {
			t.Fatalf("una AUTORIZACIÓN se leyó como venta liquidada (%s) — eso haría que aprobar no capturase y el local no cobrara: %s", evidence, body)
		}
	}

	// Y la contraria, para que el guard no sea decorativo: una venta liquidada
	// (lo que se pidió NO retener, o la pasarela desobedeció) sí se detecta.
	for _, body := range []string{
		`{"_links":{"self":{"href":"/x"},"void":{"href":"/x/voids"}}}`,
		`{"_links":{"void":{"href":"/x/voids"}}}`,
		`{"_links":{"refund":{"href":"/x/refunds"}}}`,
	} {
		var resp map[string]interface{}
		_ = json.Unmarshal([]byte(body), &resp)
		if state, _ := captureStateFromResponse(resp); state != CybsCaptureSettled {
			t.Fatalf("una venta liquidada se leyó como %q: %s", state, body)
		}
	}
}

// TestLooksLikeJWTRejectsGarbage — el token del navegador se reenvía a la
// pasarela, así que la puerta de entrada tiene que cerrar bien.
func TestLooksLikeJWTRejectsGarbage(t *testing.T) {
	bad := []string{
		"", "no-puntos", "a.b", "a.b.c.d", "..", "a..c",
		"a.b.c\n", "a.b.c ", "<script>.b.c", "a.b.c;rm -rf /",
		strings.Repeat("a", 3000) + "." + strings.Repeat("b", 3000) + "." + strings.Repeat("c", 3000), // >8192
	}
	for _, s := range bad {
		if LooksLikeJWT(s) {
			t.Fatalf("LooksLikeJWT aceptó basura: %q", s)
		}
	}
	good := []string{"aaa.bbb.ccc", "eyJhbGciOiJSUzI1NiJ9.eyJhIjoxfQ.c2ln", "a-b_c=.d.e"}
	for _, s := range good {
		if !LooksLikeJWT(s) {
			t.Fatalf("LooksLikeJWT rechazó un JWT válido: %q", s)
		}
	}
}
