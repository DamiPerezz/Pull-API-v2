package controllers

import (
	"encoding/json"
	"net/http"
	"testing"

	"pull-api-v2/models"
	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// Smoke tests del webhook de dLocal. Solo las piezas puras (parseo del body,
// registro de pagos verificados, plumbing del carril compartido) — nada que
// toque red ni base de datos.

// CONTRATO con services/dlocal_processor_impl.go: el webhook re-consulta el
// estado del pago a través del procesador para usar las credenciales de ESE
// venue (multi-tenant). Si esta aserción deja de compilar, la asercion de tipo
// de dlocalGetPayment fallaría EN SILENCIO y el webhook caería a las
// credenciales de entorno sin que nadie se entere.
var _ dlocalPaymentReader = (*services.DLocalProcessor)(nil)

// El id del pago tiene que salir de cualquiera de los shapes que manda dLocal,
// y NUNCA de un valor con caracteres que se puedan inyectar en la URL de la API
// o en un filtro de PostgREST.
func TestDLocalPaymentIDFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"id plano", `{"id":"D-4-abc123","status":"PAID"}`, "D-4-abc123"},
		{"id numérico", `{"id":123456789}`, "123456789"},
		{"payment_id gana", `{"payment_id":"PAY1","other":"x"}`, "PAY1"},
		{"anidado en data", `{"data":{"id":"NESTED1"}}`, "NESTED1"},
		{"anidado en payment", `{"payment":{"payment_id":"NESTED2"}}`, "NESTED2"},
		{"vacío", `{}`, ""},
		{"inyección PostgREST", `{"id":"abc.not.is.null"}`, ""},
		{"inyección path", `{"id":"1/../../v1/me"}`, ""},
		{"demasiado largo", `{"id":"` + string(make([]byte, 0)) + `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]interface{}
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatalf("body de test inválido: %v", err)
			}
			if got := dlocalPaymentIDFromBody(body); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Una notificación de REEMBOLSO no puede confundirse con un pago: el pago
// original sigue diciendo PAID y confirmaríamos una orden reembolsada.
func TestDLocalRefundNotificationDetection(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"id":"P1","status":"PAID"}`, false},
		{`{"payment_id":"P1","status":"PAID"}`, false},
		{`{"id":"R1","payment_id":"P1","status":"SUCCESS"}`, true},
		{`{"refund_id":"R1","status":"SUCCESS"}`, true},
		{`{"type":"REFUND","id":"R1"}`, true},
	}
	for _, tc := range cases {
		var body map[string]interface{}
		if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
			t.Fatal(err)
		}
		if got := isDLocalRefundNotification(body); got != tc.want {
			t.Fatalf("%s → %v, want %v", tc.body, got, tc.want)
		}
	}
}

// El registro de pagos verificados es de un solo uso: si se consumiera dos
// veces, un reintento podría emitir tickets sin volver a preguntar a dLocal.
func TestVerifiedPaymentIsSingleUse(t *testing.T) {
	sessionID := services.DLocalSessionID("TESTPAY1")
	registerVerifiedPayment(sessionID, &models.PaymentResult{Success: true, TransactionID: "TESTPAY1"})

	got, ok := takeVerifiedPayment(sessionID)
	if !ok || got.TransactionID != "TESTPAY1" {
		t.Fatalf("primera lectura falló: %v %v", got, ok)
	}
	if _, ok := takeVerifiedPayment(sessionID); ok {
		t.Fatal("el pago verificado se pudo consumir dos veces")
	}
	if _, ok := takeVerifiedPayment(services.DLocalSessionID("NOEXISTE")); ok {
		t.Fatal("devolvió un pago que nadie registró")
	}
}

// El carril compartido se invoca fuera de una request real: hay que recoger el
// código de estado y TIRAR el cuerpo (esa respuesta era para el navegador del
// comprador, no para dLocal).
func TestDiscardResponseWriterCapturesStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, want := range []int{http.StatusOK, http.StatusConflict, http.StatusInternalServerError} {
		rec := &discardResponseWriter{header: make(http.Header), status: http.StatusOK}
		c, _ := gin.CreateTestContext(rec)
		req, err := http.NewRequest(http.MethodGet, "/internal?session_id=s&venue_id=v", nil)
		if err != nil {
			t.Fatal(err)
		}
		c.Request = req
		c.JSON(want, gin.H{"hola": "mundo"})
		if rec.status != want {
			t.Fatalf("status %d, want %d", rec.status, want)
		}
	}
}

// dLocal reintenta la MISMA notificación. Un rechazo repetido no puede contar
// como cinco intentos distintos: cerraría la orden de alguien que solo falló
// una vez el CVV.
func TestDLocalAlreadySeen(t *testing.T) {
	orderWith := func(paymentID, status string) map[string]interface{} {
		return map[string]interface{}{"metadata": map[string]interface{}{
			"dlocal": map[string]interface{}{"payment_id": paymentID, "status": status},
		}}
	}
	rejected := &services.DLocalPayment{ID: "P1", Status: services.DLocalStatusRejected}

	if dlocalAlreadySeen(map[string]interface{}{}, rejected) {
		t.Fatal("orden sin rastro marcada como ya vista")
	}
	if !dlocalAlreadySeen(orderWith("P1", services.DLocalStatusRejected), rejected) {
		t.Fatal("el mismo pago en el mismo estado debería estar ya visto")
	}
	// Mismo pago, estado NUEVO (rechazado → pagado): hay que procesarlo.
	if dlocalAlreadySeen(orderWith("P1", services.DLocalStatusRejected), &services.DLocalPayment{ID: "P1", Status: services.DLocalStatusPaid}) {
		t.Fatal("un cambio de estado del mismo pago no puede ignorarse")
	}
	// Pago distinto (reintento con otra tarjeta): hay que procesarlo.
	if dlocalAlreadySeen(orderWith("P1", services.DLocalStatusRejected), &services.DLocalPayment{ID: "P2", Status: services.DLocalStatusRejected}) {
		t.Fatal("un pago nuevo no puede ignorarse")
	}
}

// Tras un intento fallido, una solicitud privada aprobada vuelve a
// `approved_unpaid` (puede reintentar dentro del plazo); todo lo demás vuelve a
// `pending`. Mandarla al estado equivocado la dejaría impagable.
func TestResumableStatusFor(t *testing.T) {
	future := "2999-01-01T00:00:00Z"
	past := "2000-01-01T00:00:00Z"

	if got := resumableStatusFor(map[string]interface{}{}); got != "pending" {
		t.Fatalf("sin metadata → %q", got)
	}
	withDeadline := func(v string) map[string]interface{} {
		return map[string]interface{}{"metadata": map[string]interface{}{"payment_deadline": v}}
	}
	if got := resumableStatusFor(withDeadline(future)); got != "approved_unpaid" {
		t.Fatalf("plazo vigente → %q", got)
	}
	if got := resumableStatusFor(withDeadline(past)); got != "pending" {
		t.Fatalf("plazo vencido → %q", got)
	}
}
