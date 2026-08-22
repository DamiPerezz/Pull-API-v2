package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// =============================================================================
// SEÑALES DE DISPOSITIVO — que lleguen, que lleguen BIEN, y que la basura no
//
// El 22-ago-2026 Decision Manager rechazó pagos reales en producción
// (DECISION_PROFILE_REJECT) con las columnas IP Address, IP Country y Device
// Fingerprint VACÍAS en el panel: no le estábamos mandando ni una señal de
// dispositivo. Estos tests fijan las tres cosas que hay que garantizar ahora:
//
//	1. que las señales viajan, y con los NOMBRES exactos del manual de NeoNet
//	   (equivocarse de campo es tan malo como no mandarlo: el dato acaba en una
//	   columna que la regla de velocidad no mira);
//	2. que un campo inválido se DESCARTA en vez de viajar — al antifraude no se
//	   le dan datos falsos;
//	3. que nada de esto toca el resto del cuerpo del pago.
//
// La identidad byte a byte cuando NO hay señales vive en el otro fichero
// (cybersource_card_payload_test.go), que es donde se defiende el carril que
// hoy cobra dinero real.
// =============================================================================

func testDevice() CybsDeviceInfo {
	return CybsDeviceInfo{
		IPAddress:            "190.56.100.7", // IP guatemalteca de ejemplo
		FingerprintSessionID: "ea5f708f-0a77-4623-bdf8-62bb656c4a1f",
		UserAgent:            "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15",
		AcceptHeader:         "application/json, text/plain, */*",
		Language:             "es-GT,es;q=0.9,en;q=0.8",
	}
}

// deviceBlockOf ejecuta un Sale() y devuelve el bloque `deviceInformation` que
// se habría enviado (nil si no se envió ninguno).
func deviceBlockOf(t *testing.T, device CybsDeviceInfo) map[string]interface{} {
	t.Helper()
	_, sent, _ := captureSaleBody(t, testCard(), testBillTo(), true, "", device,
		`{"id":"tx1","status":"AUTHORIZED","_links":{"void":{"href":"/x"}}}`)
	var body map[string]interface{}
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatalf("cuerpo no serializable: %v", err)
	}
	if body["deviceInformation"] == nil {
		return nil
	}
	dev, ok := body["deviceInformation"].(map[string]interface{})
	if !ok {
		t.Fatalf("deviceInformation no es un objeto: %s", sent)
	}
	return dev
}

// TestSaleSendsDeviceInformation — LA PRUEBA CENTRAL DEL ARREGLO.
// Con señales válidas tienen que viajar todas, con el nombre que les da el
// "REST API Field Reference" de NeoNet. Los nombres están escritos a mano aquí:
// si alguien los cambia en el código, este test salta.
func TestSaleSendsDeviceInformation(t *testing.T) {
	dev := deviceBlockOf(t, testDevice())
	if dev == nil {
		t.Fatal("NO viajó deviceInformation — es justo el agujero que este cambio arregla")
	}

	want := map[string]string{
		"ipAddress":            "190.56.100.7",
		"fingerprintSessionId": "ea5f708f-0a77-4623-bdf8-62bb656c4a1f",
		// La cabecera User-Agent ENTERA va en userAgentBrowserValue (2048), no
		// en userAgent (40, que es el TIPO de navegador).
		"userAgentBrowserValue":  "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15",
		"httpAcceptBrowserValue": "application/json, text/plain, */*",
		// Solo la etiqueta principal: el campo admite 8 caracteres.
		"httpBrowserLanguage": "es-GT",
	}
	for k, v := range want {
		if got, _ := dev[k].(string); got != v {
			t.Fatalf("deviceInformation.%s = %q, esperado %q", k, got, v)
		}
	}
	if len(dev) != len(want) {
		t.Fatalf("deviceInformation trae campos de más o de menos: %v", dev)
	}
	// El campo de 40 caracteres NO se usa: mandar ahí la cadena entera la
	// truncaría y ensuciaría el dato.
	if _, ok := dev["userAgent"]; ok {
		t.Fatal("no se debe usar deviceInformation.userAgent (40 chars) para la cadena User-Agent entera")
	}
}

// TestDeviceInformationNoTocaElRestoDelCuerpo — las señales se AÑADEN, no
// reemplazan ni mueven nada. El importe, la moneda, el billTo, el capture y la
// tarjeta tienen que quedar exactamente como en el cuerpo de referencia.
func TestDeviceInformationNoTocaElRestoDelCuerpo(t *testing.T) {
	_, sent, _ := captureSaleBody(t, testCard(), testBillTo(), true, "", testDevice(),
		`{"id":"tx1","status":"AUTHORIZED","_links":{"void":{"href":"/x"}}}`)
	var body map[string]interface{}
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatalf("cuerpo no serializable: %v", err)
	}
	legacy := legacySalePayload("ORD-1-VENUE", 350.75, "GTQ", testCard(), testBillTo(), true)
	for _, key := range []string{"clientReferenceInformation", "processingInformation", "orderInformation", "paymentInformation"} {
		wantJSON, _ := json.Marshal(legacy[key])
		gotJSON, _ := json.Marshal(body[key])
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("%s cambió al añadir las señales de dispositivo.\n esperado: %s\n obtenido: %s", key, wantJSON, gotJSON)
		}
	}
	// Y no aparece ninguna clave nueva aparte de deviceInformation.
	if len(body) != len(legacy)+1 {
		t.Fatalf("el cuerpo ganó claves inesperadas: %s", sent)
	}
}

// TestDeviceInformationDescartaLoInvalido — un campo malo se cae solo; los
// buenos que lo acompañan siguen viajando. Nunca se reenvía sin validar.
func TestDeviceInformationDescartaLoInvalido(t *testing.T) {
	dev := deviceBlockOf(t, CybsDeviceInfo{
		IPAddress:            "999.999.999.999", // no parsea
		FingerprintSessionID: "tiene espacios y símbolos $$",
		UserAgent:            "Mozilla/5.0",
		Language:             "no-es-un-idioma-valido-porque-es-larguisimo",
	})
	if dev == nil {
		t.Fatal("los campos válidos que quedaban tenían que viajar igual")
	}
	for _, k := range []string{"ipAddress", "fingerprintSessionId", "httpBrowserLanguage"} {
		if _, ok := dev[k]; ok {
			t.Fatalf("viajó un campo inválido: %s = %v", k, dev[k])
		}
	}
	if dev["userAgentBrowserValue"] != "Mozilla/5.0" {
		t.Fatalf("el campo válido no viajó: %v", dev)
	}
}

// TestDeviceInformationLimitesYFormas — los límites del manual y el saneado de
// cabeceras. Una cabecera con saltos de línea NO puede llegar entera a un JSON
// que va a un motor de reglas.
func TestDeviceInformationLimitesYFormas(t *testing.T) {
	t.Run("IPv6 vale", func(t *testing.T) {
		dev := deviceBlockOf(t, CybsDeviceInfo{IPAddress: "2001:db8::8a2e:370:7334"})
		if dev["ipAddress"] != "2001:db8::8a2e:370:7334" {
			t.Fatalf("IPv6 rechazada: %v", dev)
		}
	})

	t.Run("User-Agent se recorta a 2048", func(t *testing.T) {
		dev := deviceBlockOf(t, CybsDeviceInfo{UserAgent: strings.Repeat("A", 5000)})
		got, _ := dev["userAgentBrowserValue"].(string)
		if len(got) != 2048 {
			t.Fatalf("longitud %d, esperada 2048", len(got))
		}
	})

	t.Run("Accept se recorta a 255", func(t *testing.T) {
		dev := deviceBlockOf(t, CybsDeviceInfo{AcceptHeader: strings.Repeat("b", 900)})
		got, _ := dev["httpAcceptBrowserValue"].(string)
		if len(got) != 255 {
			t.Fatalf("longitud %d, esperada 255", len(got))
		}
	})

	t.Run("los caracteres de control se van", func(t *testing.T) {
		dev := deviceBlockOf(t, CybsDeviceInfo{UserAgent: "Mozilla\r\n/5.0\x00 (X11)"})
		got, _ := dev["userAgentBrowserValue"].(string)
		if strings.ContainsAny(got, "\r\n\x00") {
			t.Fatalf("se coló un carácter de control: %q", got)
		}
		if got != "Mozilla/5.0 (X11)" {
			t.Fatalf("saneado inesperado: %q", got)
		}
	})

	t.Run("el recorte no parte un carácter multibyte", func(t *testing.T) {
		// 300 acentos = 600 bytes: el recorte a 255 tiene que caer en frontera
		// de carácter, no dejar medio byte suelto.
		dev := deviceBlockOf(t, CybsDeviceInfo{AcceptHeader: strings.Repeat("é", 300)})
		got, _ := dev["httpAcceptBrowserValue"].(string)
		if !json.Valid([]byte(`"` + got + `"`)) {
			t.Fatalf("el recorte produjo UTF-8 roto: %q", got)
		}
		if len(got) > 255 {
			t.Fatalf("longitud %d supera el límite del campo", len(got))
		}
	})

	t.Run("Accept-Language se reduce a la etiqueta principal", func(t *testing.T) {
		cases := map[string]string{
			"es-GT,es;q=0.9,en;q=0.8": "es-GT",
			"en-US":                   "en-US",
			"es":                      "es",
			"*":                       "", // comodín: no es un idioma
			"":                        "",
			"es_GT":                   "", // guion bajo no es BCP47
			"esto-es-demasiado-largo": "",
		}
		for in, want := range cases {
			dev := deviceBlockOf(t, CybsDeviceInfo{Language: in})
			got := ""
			if dev != nil {
				got, _ = dev["httpBrowserLanguage"].(string)
			}
			if got != want {
				t.Fatalf("Accept-Language %q → %q, esperado %q", in, got, want)
			}
		}
	})
}

// TestLooksLikeFingerprintSessionIDRechazaBasura — la huella entra por la
// petición de pago, o sea que la escribe el navegador y se reenvía a la
// pasarela. Misma puerta que la del transient token.
func TestLooksLikeFingerprintSessionIDRechazaBasura(t *testing.T) {
	bad := []string{
		"", "corta", "con espacios aqui", "punto.punto.punto",
		"barra/inversa", "<script>alert(1)</script>", "comilla'simple",
		"salto\nlinea", strings.Repeat("a", 65),
	}
	for _, s := range bad {
		if LooksLikeFingerprintSessionID(s) {
			t.Fatalf("aceptó basura: %q", s)
		}
	}
	good := []string{
		"ea5f708f-0a77-4623-bdf8-62bb656c4a1f", // el formato que usa Cybersource
		"visanetgt_pull_00000001",
		"abcd1234",
	}
	for _, s := range good {
		if !LooksLikeFingerprintSessionID(s) {
			t.Fatalf("rechazó un id válido: %q", s)
		}
	}
}

// =============================================================================
// CAPTURE CONTEXT — completeMandate.decisionManager
//
// Es el interruptor documentado que hace que Unified Checkout ejecute Decision
// Manager Y el perfilado del dispositivo. Sin él, la doc de NeoNet dice que
// "Decision Manager and device fingerprinting services do not run" — o sea que
// el carril de wallet también iba sin huella.
// =============================================================================

// captureContextBody ejecuta un CaptureContext() contra un transporte falso y
// devuelve el cuerpo que se habría enviado. No abre ningún puerto.
func captureContextBody(t *testing.T, p CaptureContextParams) map[string]interface{} {
	t.Helper()
	var sent []byte
	cli := NewCybersourceClient("merchant-test", "key-id-test", "c2hhcmVkLXNlY3JldA==", "test")
	cli.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sent, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("aaa.bbb.ccc")),
				Header:     http.Header{"Content-Type": []string{"application/jwt"}},
			}, nil
		}),
	}
	if _, err := cli.CaptureContext(context.Background(), p); err != nil {
		t.Fatalf("CaptureContext devolvió error: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatalf("cuerpo no serializable: %v", err)
	}
	return body
}

func baseCaptureContextParams() CaptureContextParams {
	return CaptureContextParams{
		TargetOrigins:       []string{"https://511events.com"},
		Amount:              350.75,
		Currency:            "GTQ",
		Country:             "GT",
		Locale:              "es_GT",
		AllowedPaymentTypes: []string{"APPLEPAY", "GOOGLEPAY"},
		AllowedCardNetworks: []string{"VISA", "MASTERCARD"},
		ClientVersion:       "0.24",
		ReferenceCode:       "ORD-1-UC",
	}
}

func TestCaptureContextDecisionManager(t *testing.T) {
	t.Run("encendido añade decisionManager junto al mandato", func(t *testing.T) {
		p := baseCaptureContextParams()
		p.CompleteMandateType = "CAPTURE"
		p.DecisionManager = true

		cm, ok := captureContextBody(t, p)["completeMandate"].(map[string]interface{})
		if !ok {
			t.Fatal("falta completeMandate")
		}
		if cm["type"] != "CAPTURE" {
			t.Fatalf("el mandato cambió: %v", cm)
		}
		if cm["decisionManager"] != true {
			t.Fatalf("sin decisionManager NO se genera la huella: %v", cm)
		}
	})

	t.Run("apagado deja el cuerpo como estaba", func(t *testing.T) {
		p := baseCaptureContextParams()
		p.CompleteMandateType = "AUTH"
		p.DecisionManager = false

		cm, _ := captureContextBody(t, p)["completeMandate"].(map[string]interface{})
		if _, ok := cm["decisionManager"]; ok {
			t.Fatalf("apagado no debe emitir la clave: %v", cm)
		}
		if len(cm) != 1 || cm["type"] != "AUTH" {
			t.Fatalf("completeMandate debía quedar solo con type: %v", cm)
		}
	})

	t.Run("sin mandato no se emite decisionManager suelto", func(t *testing.T) {
		// El manual condiciona decisionManager a que exista completeMandate.type.
		// Emitirlo solo produciría un cuerpo que la pasarela no acepta.
		p := baseCaptureContextParams()
		p.CompleteMandateType = ""
		p.DecisionManager = true

		if _, ok := captureContextBody(t, p)["completeMandate"]; ok {
			t.Fatal("sin type no puede viajar ningún completeMandate")
		}
	})
}
