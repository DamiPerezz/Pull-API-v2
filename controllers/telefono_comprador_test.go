package controllers

import (
	"encoding/json"
	"testing"
)

// =============================================================================
// De dónde sale el teléfono que viaja a Cybersource.
//
// Los casos de este test NO son inventados: son la forma EXACTA del metadata
// de órdenes reales de producción, leídas de la base del venue el 2026-08-22.
// Se fija aquí porque el arreglo del teléfono depende de esa forma, y si algún
// día el formulario cambia las claves, la consecuencia sería silenciosa: no
// petaría nada, simplemente dejaríamos de mandar teléfono otra vez.
// =============================================================================

func metadataDe(raw string) map[string]interface{} {
	var md map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &md); err != nil {
		panic(err)
	}
	return map[string]interface{}{"metadata": md}
}

func TestTelefonoDelCompradorConDatosRealesDeProduccion(t *testing.T) {
	casos := []struct {
		nombre string
		orden  string // metadata tal cual está en la tabla `orders`
		quiero string
	}{
		{
			// ORD-20260822-5371BB — comprador con teléfono español
			nombre: "prefijo internacional +34",
			orden: `{"tickets_data":[{"owner_name":"Damian","owner_last_name":"Perez",
			         "owner_email":"dpmcyber@pm.me","owner_phone":"687120072",
			         "owner_phone_prefix":"+34","ticket_type_name":"Geberal"}],
			         "payment_link_code":"abc123"}`,
			quiero: "34687120072",
		},
		{
			// ORD-20260822-D5892A — comprador guatemalteco
			nombre: "prefijo +502",
			orden: `{"tickets_data":[{"owner_name":"Verificacion","owner_last_name":"Version",
			         "owner_email":"verif@pullevents.com","owner_phone":"55512345",
			         "owner_phone_prefix":"+502"}],"payment_link_code":"x"}`,
			quiero: "50255512345",
		},
		{
			nombre: "el comprador escribe espacios y guiones",
			orden: `{"tickets_data":[{"owner_phone":"5551 23-45","owner_phone_prefix":"+502"}]}`,
			quiero: "50255512345",
		},
		{
			nombre: "sin prefijo: se manda el número tal cual",
			orden:  `{"tickets_data":[{"owner_phone":"55512345"}]}`,
			quiero: "55512345",
		},
		{
			// El caso peligroso: antes aquí se mandaba "50200000000" inventado.
			// Ahora no se manda NADA, y Sale() omite la clave phoneNumber.
			nombre: "sin teléfono: vacío, NUNCA un número inventado",
			orden:  `{"tickets_data":[{"owner_name":"Sin","owner_last_name":"Telefono"}]}`,
			quiero: "",
		},
		{
			nombre: "metadata sin tickets_data",
			orden:  `{"payment_link_code":"x"}`,
			quiero: "",
		},
		{
			nombre: "tickets_data vacío",
			orden:  `{"tickets_data":[]}`,
			quiero: "",
		},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			got := telefonoDelComprador(metadataDe(c.orden))
			if got != c.quiero {
				t.Errorf("teléfono = %q, quiero %q", got, c.quiero)
			}
		})
	}
}

// TestTelefonoNuncaEsElInventado es la red de seguridad contra la regresión
// concreta: que vuelva a colarse un literal compartido por todos los
// compradores. Cybersource lo marcaba como MORPH-P, "Same phone number with
// multiple customer identities".
func TestTelefonoNuncaEsElInventado(t *testing.T) {
	const elViejo = "50200000000"

	// Ninguna entrada razonable puede producirlo, ni siquiera la vacía.
	entradas := []string{
		`{"tickets_data":[{}]}`,
		`{"tickets_data":[{"owner_phone":""}]}`,
		`{"tickets_data":[{"owner_phone":"","owner_phone_prefix":"+502"}]}`,
		`{}`,
	}
	for _, e := range entradas {
		if got := telefonoDelComprador(metadataDe(e)); got == elViejo {
			t.Errorf("volvió el teléfono inventado con metadata %s", e)
		}
	}
}

// TestOrdenSinMetadata: una orden a la que no se le pidió el select de
// metadata no puede reventar el cobro. Devuelve vacío y sigue.
func TestOrdenSinMetadata(t *testing.T) {
	if got := telefonoDelComprador(map[string]interface{}{"id": "x"}); got != "" {
		t.Errorf("sin metadata debería dar vacío, dio %q", got)
	}
	if got := telefonoDelComprador(map[string]interface{}{"metadata": nil}); got != "" {
		t.Errorf("metadata nula debería dar vacío, dio %q", got)
	}
}
