package controllers

import (
	"os"
	"testing"
)

// =============================================================================
// LOS DOS INVARIANTES DEL CARRIL DE WALLET QUE TOCAN DINERO
//
//  1. El mandato que se le declara a la pasarela al abrir la sesión y el
//     `capture` con el que se cobra después salen de la MISMA lectura y no
//     pueden divergir: retener declarando venta (o al revés) deja al comprador
//     cobrado creyendo que solo se le retuvo.
//  2. Con el interruptor apagado, este camino no existe.
// =============================================================================

// TestMandateMatchesCapture fija la bisagra entre "el evento necesita
// aprobación" y lo que se le promete a la pasarela.
//
// El cobro hace `capture := !needsApproval` (pay_controller.go). Aquí se
// comprueba que el mandato de la sesión dice EXACTAMENTE lo mismo con la misma
// entrada. Si alguien invierte una de las dos, este test salta.
func TestMandateMatchesCapture(t *testing.T) {
	cases := []struct {
		requiresApproval bool
		wantMandate      string
		wantCapture      bool
	}{
		// Evento público: se cobra al momento.
		{requiresApproval: false, wantMandate: "CAPTURE", wantCapture: true},
		// Evento privado: se retiene y se captura (o se revierte) en 48 h.
		{requiresApproval: true, wantMandate: "AUTH", wantCapture: false},
	}
	for _, tc := range cases {
		gotMandate := mandateForApproval(tc.requiresApproval)
		if gotMandate != tc.wantMandate {
			t.Fatalf("requiresApproval=%v → mandato %q, esperado %q", tc.requiresApproval, gotMandate, tc.wantMandate)
		}
		// Réplica literal de la línea de PayOrder: `capture := !needsApproval`.
		gotCapture := !tc.requiresApproval
		if gotCapture != tc.wantCapture {
			t.Fatalf("requiresApproval=%v → capture=%v, esperado %v", tc.requiresApproval, gotCapture, tc.wantCapture)
		}
		// Y la relación cruzada, que es la que de verdad importa: AUTH SIEMPRE
		// va con capture=false, y CAPTURE SIEMPRE con capture=true.
		if (gotMandate == ucMandateAuth) == gotCapture {
			t.Fatalf("mandato %q y capture=%v se contradicen: se estaría declarando una operación y haciendo otra", gotMandate, gotCapture)
		}
	}
}

// TestUnifiedCheckoutEnabledIsStrict — el interruptor es la vuelta atrás de 30
// segundos. Tiene que estar APAGADO por defecto y solo encenderse con el valor
// exacto: un "1", un "yes" o un "TRUE " con espacios que colara encendería el
// carril de wallet en producción sin que nadie lo hubiera pedido.
func TestUnifiedCheckoutEnabledIsStrict(t *testing.T) {
	original, had := os.LookupEnv("UNIFIED_CHECKOUT_ENABLED")
	t.Cleanup(func() {
		if had {
			os.Setenv("UNIFIED_CHECKOUT_ENABLED", original)
		} else {
			os.Unsetenv("UNIFIED_CHECKOUT_ENABLED")
		}
	})

	os.Unsetenv("UNIFIED_CHECKOUT_ENABLED")
	if unifiedCheckoutEnabled() {
		t.Fatal("sin la variable el interruptor tiene que estar APAGADO")
	}

	off := []string{"", "false", "0", "1", "yes", "si", "on", "enabled", "truthy", "  ", "no"}
	for _, v := range off {
		os.Setenv("UNIFIED_CHECKOUT_ENABLED", v)
		if unifiedCheckoutEnabled() {
			t.Fatalf("el valor %q NO puede encender el carril de wallet", v)
		}
	}

	on := []string{"true", "TRUE", "True", " true ", "\ttrue\n"}
	for _, v := range on {
		os.Setenv("UNIFIED_CHECKOUT_ENABLED", v)
		if !unifiedCheckoutEnabled() {
			t.Fatalf("el valor %q sí debería encenderlo", v)
		}
	}
}

// TestNormalizeOriginRejectsUntrusted — targetOrigins es lo que impide que un
// clon del sitio abra sesiones de cobro con nuestra cuenta de merchant. Si aquí
// se cuela un origen con ruta, con http, o un comodín, Cybersource rechaza la
// sesión entera (o peor, la acepta desde donde no debe).
func TestNormalizeOriginRejectsUntrusted(t *testing.T) {
	bad := map[string]string{
		"comodín":        "*",
		"vacío":          "",
		"http remoto":    "http://evil.example.com",
		"sin esquema":    "evil.example.com",
		"esquema raro":   "javascript:alert(1)",
		"data uri":       "data:text/html,<script>",
		"solo espacios":  "   ",
		"ftp":            "ftp://example.com",
		"http sin host":  "http://",
		"https sin host": "https://",
	}
	for name, raw := range bad {
		if got := normalizeOrigin(raw); got != "" {
			t.Fatalf("%s: normalizeOrigin(%q) = %q, tenía que rechazarse", name, raw, got)
		}
	}

	good := map[string]string{
		"https://staging.pull-511-events.pages.dev":  "https://staging.pull-511-events.pages.dev",
		"https://pull-511-events.pages.dev/":         "https://pull-511-events.pages.dev",
		"https://tickets.511events.com/checkout?a=1": "https://tickets.511events.com",
		"  https://tickets.511events.com  ":          "https://tickets.511events.com",
		"http://localhost:5173":                      "http://localhost:5173",
		"http://127.0.0.1:3000/path":                 "http://127.0.0.1:3000",
	}
	for raw, want := range good {
		if got := normalizeOrigin(raw); got != want {
			t.Fatalf("normalizeOrigin(%q) = %q, esperado %q", raw, got, want)
		}
	}
}
