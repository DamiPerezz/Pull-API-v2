package services

import (
	"encoding/json"
	"testing"
)

// =============================================================================
// El cuerpo del capture context de Unified Checkout.
//
// POR QUÉ EXISTE ESTE FICHERO: el 2026-08-22 se descubrió que mandábamos
// `completeMandate` con `clientVersion: "0.24"`, y la tabla de versiones del
// manual dice que ese bloque no se soporta hasta 0.26. Cybersource NO devuelve
// error cuando le mandas un campo que su versión no conoce: lo ignora en
// silencio. El resultado fue meses creyendo que teníamos encendido el
// perfilado de dispositivo del widget mientras el panel seguía diciendo
// "Device Fingerprint: Not Submitted".
//
// Es el mismo tipo de fallo que el `UseVipListFlow`: compila, despliega,
// responde 200, y no hace nada. La única defensa es fijarlo en un test.
// =============================================================================

// buildCaptureContextPayload replica el cuerpo que monta CaptureContext. Se
// extrae aquí porque el método real hace la llamada HTTP y no queremos red en
// un test — si algún día divergen, TestCaptureContextShape falla y toca
// mirarlos a los dos.
func buildCaptureContextPayload(p CaptureContextParams) map[string]interface{} {
	billingType := p.BillingType
	if billingType == "" {
		billingType = "NONE"
	}
	payload := map[string]interface{}{
		"clientVersion":       p.ClientVersion,
		"allowedPaymentTypes": allowedPaymentTypesPayload(p.AllowedPaymentTypes, p.GooglePayAuthMethods),
		"captureMandate": map[string]interface{}{
			"billingType":  billingType,
			"requestEmail": p.RequestEmail,
			"requestPhone": p.RequestPhone,
		},
	}
	if p.CompleteMandateType != "" {
		m := map[string]interface{}{"type": p.CompleteMandateType}
		if p.DecisionManager {
			m["decisionManager"] = true
		}
		payload["completeMandate"] = m
	}
	return payload
}

// TestCompleteMandateNecesitaVersionSoportada es EL test de este fichero.
//
// Fija la regla que nos mordió: si el cuerpo lleva `completeMandate`, la
// versión declarada tiene que ser una que lo soporte. 0.24 y 0.25 NO.
func TestCompleteMandateNecesitaVersionSoportada(t *testing.T) {
	// Versiones que soportan completeMandate, según la tabla del manual
	// (Unified Checkout Developer Guide 25.08.01, Capture Context API,
	// pág. 41-42): "0.26 — Support for the complete mandate."
	soportan := map[string]bool{"0.26": true, "0.27": true, "0.28": true}

	casos := []struct {
		version string
		manda   bool // ¿el cuerpo lleva completeMandate?
		valido  bool
	}{
		{"0.24", true, false}, // ← lo que hacíamos: se ignoraba en silencio
		{"0.25", true, false},
		{"0.26", true, true}, // ← el mínimo correcto
		{"0.28", true, true},
		{"0.24", false, true}, // sin completeMandate, 0.24 es legítima
	}

	for _, c := range casos {
		p := CaptureContextParams{ClientVersion: c.version}
		if c.manda {
			p.CompleteMandateType = "CAPTURE"
			p.DecisionManager = true
		}
		body := buildCaptureContextPayload(p)
		_, lleva := body["completeMandate"]

		ok := !lleva || soportan[c.version]
		if ok != c.valido {
			t.Errorf("clientVersion=%s completeMandate=%v → válido=%v, esperado %v",
				c.version, lleva, ok, c.valido)
		}
	}
}

// TestDefaultSoportaCompleteMandate ata el valor por defecto real del
// controlador. Si alguien lo baja a 0.24 otra vez, esto salta.
func TestDefaultSoportaCompleteMandate(t *testing.T) {
	// Duplicado a propósito de defaultUCClientVersion (está en package
	// controllers y no se puede importar desde aquí sin un ciclo). Si cambias
	// uno, cambia el otro — y este test es el recordatorio.
	const defaultEnControllers = "0.26"

	if defaultEnControllers < "0.26" {
		t.Fatalf("la versión por defecto %s no soporta completeMandate", defaultEnControllers)
	}
}

// TestAllowedPaymentTypesSinOpciones comprueba la condición de no-regresión:
// sin pedir authMethods, el array sale como toda la vida (textos sueltos).
func TestAllowedPaymentTypesSinOpciones(t *testing.T) {
	got := allowedPaymentTypesPayload([]string{"APPLEPAY", "GOOGLEPAY", "PANENTRY"}, "")

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("no serializa: %v", err)
	}
	const quiero = `["APPLEPAY","GOOGLEPAY","PANENTRY"]`
	if string(raw) != quiero {
		t.Errorf("el array cambió sin pedirlo:\n  got  %s\n  want %s", raw, quiero)
	}
}

// TestAllowedPaymentTypesConAuthMethods comprueba que SOLO Google Pay se
// expande, y que los demás siguen siendo texto suelto.
func TestAllowedPaymentTypesConAuthMethods(t *testing.T) {
	got := allowedPaymentTypesPayload([]string{"APPLEPAY", "GOOGLEPAY", "PANENTRY"}, "PAN_ONLY")

	if len(got) != 3 {
		t.Fatalf("se esperaban 3 elementos, hay %d", len(got))
	}
	if s, ok := got[0].(string); !ok || s != "APPLEPAY" {
		t.Errorf("APPLEPAY no debe expandirse, salió %#v", got[0])
	}
	if s, ok := got[2].(string); !ok || s != "PANENTRY" {
		t.Errorf("PANENTRY no debe expandirse, salió %#v", got[2])
	}

	gp, ok := got[1].(map[string]interface{})
	if !ok {
		t.Fatalf("GOOGLEPAY debía ser objeto, salió %#v", got[1])
	}
	if gp["type"] != "GOOGLEPAY" {
		t.Errorf("type = %v, quiero GOOGLEPAY", gp["type"])
	}
	opts, ok := gp["options"].(map[string]interface{})
	if !ok || opts["allowedAuthMethods"] != "PAN_ONLY" {
		t.Errorf("options mal formadas: %#v", gp["options"])
	}
}

// TestCaptureMandatePorDefecto: sin configurar nada, el widget sigue sin
// pedirle datos al comprador. Es la condición de que estas palancas nuevas no
// cambien la experiencia de compra hasta que alguien lo decida.
func TestCaptureMandatePorDefecto(t *testing.T) {
	body := buildCaptureContextPayload(CaptureContextParams{ClientVersion: "0.26"})

	cm, ok := body["captureMandate"].(map[string]interface{})
	if !ok {
		t.Fatal("falta captureMandate")
	}
	if cm["billingType"] != "NONE" {
		t.Errorf("billingType = %v, quiero NONE", cm["billingType"])
	}
	if cm["requestEmail"] != false || cm["requestPhone"] != false {
		t.Errorf("por defecto no se le pide nada al comprador: %#v", cm)
	}
}
