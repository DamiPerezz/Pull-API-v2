package services

import (
	"encoding/json"
	"testing"
)

// De esta lectura depende que una orden de evento privado se anote como
// RETENIDA o como COBRADA, y de esa anotación depende que aprobar capture (o
// no) y que rechazar reverse (o devuelva). Equivocarse aquí deja al comprador
// cobrado y sin entrada, así que las dos formas reales de respuesta de
// Cybersource se fijan en un test.
//
// Las muestras salen del Payments Developer Guide (REST API, Visa Platform
// Connect): la API enlaza en `_links` SOLO las operaciones que la transacción
// todavía admite — una autorización viva se puede capturar y reversar; una
// venta ya liquidada, solo anular/devolver.
func TestCaptureStateFromResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want CybsCaptureState
	}{
		{
			name: "autorizacion sola (capture=false): capturable y reversable",
			body: `{"status":"AUTHORIZED","_links":{
				"self":{"href":"/pts/v2/payments/123"},
				"capture":{"href":"/pts/v2/payments/123/captures"},
				"authReversal":{"href":"/pts/v2/payments/123/reversals"}}}`,
			want: CybsCaptureHeld,
		},
		{
			name: "venta liquidada (capture=true): ya no se puede capturar",
			body: `{"status":"AUTHORIZED","_links":{
				"self":{"href":"/pts/v2/payments/123"},
				"void":{"href":"/pts/v2/payments/123/voids"}}}`,
			want: CybsCaptureSettled,
		},
		{
			name: "solo reversable: sigue siendo una autorizacion viva",
			body: `{"status":"AUTHORIZED","_links":{
				"authReversal":{"href":"/pts/v2/payments/123/reversals"}}}`,
			want: CybsCaptureHeld,
		},
		{
			name: "sin _links: no se puede afirmar nada",
			body: `{"status":"AUTHORIZED","id":"123"}`,
			want: CybsCaptureUnknown,
		},
		{
			name: "_links sin señales utiles",
			body: `{"status":"AUTHORIZED","_links":{"self":{"href":"/pts/v2/payments/123"}}}`,
			want: CybsCaptureUnknown,
		},
		{
			name: "señales contradictorias: preferimos no saber a mentir",
			body: `{"status":"AUTHORIZED","_links":{
				"capture":{"href":"/pts/v2/payments/123/captures"},
				"void":{"href":"/pts/v2/payments/123/voids"}}}`,
			want: CybsCaptureUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp map[string]interface{}
			if err := json.Unmarshal([]byte(tc.body), &resp); err != nil {
				t.Fatalf("muestra inválida: %v", err)
			}
			got, evidence := captureStateFromResponse(resp)
			if got != tc.want {
				t.Fatalf("estado = %q, esperado %q (evidencia: %s)", got, tc.want, evidence)
			}
			if evidence == "" {
				t.Fatal("la evidencia no puede ir vacía: es lo único que queda en los logs para conciliar")
			}
		})
	}
}
