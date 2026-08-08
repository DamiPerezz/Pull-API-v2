package services

import (
	"context"
	"strings"
	"testing"

	"pull-api-v2/models"
)

// =============================================================================
// Smoke test del procesador de dLocal. No toca la red: solo fija las tres cosas
// que, si se rompen, se rompen en silencio y con dinero de por medio.
// =============================================================================

// El session id es el hilo que une la orden con el pago: se guarda en
// `orders.stripe_session_id` al crear el checkout y es con lo que el webhook
// encuentra la orden. Si construirlo y deshacerlo dejan de ser simétricos, el
// cobro entra y el ticket no sale.
func TestDLocalSessionIDRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		session string
		payment string
	}{
		{"id desnudo", "123456", "dlocal_123456", "123456"},
		{"ya prefijado (idempotente)", "dlocal_123456", "dlocal_123456", "123456"},
		{"con espacios (se normaliza)", "  789  ", "dlocal_789", "789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DLocalSessionID(tc.input); got != tc.session {
				t.Fatalf("DLocalSessionID(%q) = %q, se esperaba %q", tc.input, got, tc.session)
			}
			if got := DLocalPaymentIDFromSession(tc.session); got != tc.payment {
				t.Fatalf("DLocalPaymentIDFromSession(%q) = %q, se esperaba %q", tc.session, got, tc.payment)
			}
		})
	}

	// Un id vacío no puede generar una sesión "dlocal_" a secas: colisionaría
	// entre órdenes distintas.
	if got := DLocalSessionID(""); got != "" {
		t.Fatalf("DLocalSessionID(\"\") = %q, se esperaba cadena vacía", got)
	}
}

// dLocal Go NO acepta el PAN. Si alguien añade ChargeCard a DLocalProcessor,
// controllers.PayOrder empezaría a mandarle tarjetas crudas a una pasarela que
// no las admite — y el fallo aparecería en producción, con la tarjeta del
// comprador ya en nuestro servidor. Este test lo impide.
func TestDLocalProcessorNoAceptaTarjetaCruda(t *testing.T) {
	var p PaymentProcessor = NewDLocalProcessor(&models.VenuePaymentConfig{})
	if _, esCharger := p.(DirectCardCharger); esCharger {
		t.Fatal("DLocalProcessor implementa DirectCardCharger: dLocal Go no acepta tarjeta cruda, el cobro va por checkout alojado")
	}
	if p.GetGateway() != models.GatewayDLocal {
		t.Fatalf("GetGateway() = %q, se esperaba %q", p.GetGateway(), models.GatewayDLocal)
	}
	if !models.GatewayDLocal.IsValid() {
		t.Fatal("models.GatewayDLocal no pasa IsValid(): el router lo rechazaría")
	}
}

// Sin credenciales NO se cobra: se falla con un error que dice qué falta.
// Antes el router caía a Stripe por defecto, que es peor que fallar.
func TestDLocalSinCredencialesFalla(t *testing.T) {
	t.Setenv("DLOCAL_API_KEY", "")
	t.Setenv("DLOCAL_SECRET_KEY", "")

	p := NewDLocalProcessor(&models.VenuePaymentConfig{
		VenueID:     "venue-de-prueba",
		Gateway:     models.GatewayDLocal,
		Environment: "test",
		Credentials: &models.GatewayCredentials{},
	})

	_, err := p.CreateCheckout(context.Background(), models.CheckoutParams{
		OrderID: "orden-1", Amount: 100, Currency: "GTQ",
	})
	if err == nil {
		t.Fatal("CreateCheckout sin credenciales devolvió nil: tiene que fallar")
	}
	if !strings.Contains(err.Error(), "credenciales") {
		t.Fatalf("el error no explica que faltan credenciales: %v", err)
	}

	if _, err := p.ConfirmPayment(context.Background(), DLocalSessionID("1")); err == nil {
		t.Fatal("ConfirmPayment sin credenciales devolvió nil: 'no lo sé' nunca puede parecer 'pagó'")
	}
	if err := p.ProcessRefund(context.Background(), DLocalSessionID("1"), 10); err == nil {
		t.Fatal("ProcessRefund sin credenciales devolvió nil: sería mentir diciendo que se devolvió el dinero")
	}
}

// Un importe con más de 2 decimales descuadra la conciliación con dLocal.
func TestDLocalRound2(t *testing.T) {
	cases := map[float64]float64{
		100.004: 100.00,
		100.005: 100.01,
		99.999:  100.00,
		0:       0,
		-5:      0,
	}
	for in, want := range cases {
		if got := dlocalRound2(in); got != want {
			t.Fatalf("dlocalRound2(%v) = %v, se esperaba %v", in, got, want)
		}
	}
}

// El router tiene que fallar en seco ante una pasarela desconocida (antes caía
// a Stripe, o sea: cobrar por un carril que el venue no tiene contratado).
func TestGetProcessorGatewayDesconocido(t *testing.T) {
	_, err := processorForGateway(&models.VenuePaymentConfig{
		VenueID: "v1", Gateway: models.PaymentGateway("pasarela-inventada"),
	}, "v1")
	if err == nil {
		t.Fatal("una pasarela desconocida devolvió procesador: debe fallar en seco")
	}
	if !strings.Contains(err.Error(), "no soportado") {
		t.Fatalf("el error no dice que la pasarela no está soportada: %v", err)
	}

	proc, err := processorForGateway(&models.VenuePaymentConfig{
		VenueID: "v1", Gateway: models.GatewayDLocal,
	}, "v1")
	if err != nil {
		t.Fatalf("la pasarela dlocal debería resolverse: %v", err)
	}
	if proc.GetGateway() != models.GatewayDLocal {
		t.Fatalf("el router devolvió el procesador de %q para gateway=dlocal", proc.GetGateway())
	}
}
