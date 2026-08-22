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

// TestPaisDelComprador: el país sale del prefijo que el comprador eligió, no
// de una constante. Antes aquí iba "GT" fijo para todo el mundo, y eso hacía
// saltar MM-BIN y MM-EMBCO cuando el comprador no era guatemalteco.
func TestPaisDelComprador(t *testing.T) {
	casos := []struct {
		nombre string
		orden  string
		quiero string
	}{
		{"guatemalteco (el 99% de los compradores)",
			`{"tickets_data":[{"owner_phone":"55512345","owner_phone_prefix":"+502"}]}`, "GT"},
		{"español — el caso que provocaba los desajustes",
			`{"tickets_data":[{"owner_phone":"687120072","owner_phone_prefix":"+34"}]}`, "ES"},

		// El emparejado va de más largo a más corto. Si fuera al revés, +502
		// caería en el "5" de algo y +52 se comería a +502.
		{"502 no se confunde con 50 ni con 5",
			`{"tickets_data":[{"owner_phone":"55512345","owner_phone_prefix":"502"}]}`, "GT"},
		{"mexicano: +52, no se lo come +502",
			`{"tickets_data":[{"owner_phone":"5512345678","owner_phone_prefix":"+52"}]}`, "MX"},
		{"salvadoreño",
			`{"tickets_data":[{"owner_phone":"71234567","owner_phone_prefix":"+503"}]}`, "SV"},
		{"estadounidense: +1",
			`{"tickets_data":[{"owner_phone":"2125551234","owner_phone_prefix":"+1"}]}`, "US"},

		// Respaldo: el local es guatemalteco, así que ante la duda GT.
		{"prefijo desconocido → GT",
			`{"tickets_data":[{"owner_phone":"123","owner_phone_prefix":"+999"}]}`, "GT"},
		{"sin prefijo → GT",
			`{"tickets_data":[{"owner_phone":"55512345"}]}`, "GT"},
		{"sin tickets_data → GT", `{"payment_link_code":"x"}`, "GT"},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			if got := paisDelComprador(metadataDe(c.orden)); got != c.quiero {
				t.Errorf("país = %q, quiero %q", got, c.quiero)
			}
		})
	}
}

// TestPaisNuncaVacio: el país es obligatorio en billTo. Pase lo que pase con
// el metadata, siempre sale algo.
func TestPaisNuncaVacio(t *testing.T) {
	for _, e := range []string{`{}`, `{"tickets_data":[]}`, `{"tickets_data":[{}]}`,
		`{"tickets_data":[{"owner_phone_prefix":""}]}`} {
		if got := paisDelComprador(metadataDe(e)); got == "" {
			t.Errorf("país vacío con metadata %s", e)
		}
	}
	if got := paisDelComprador(map[string]interface{}{"id": "x"}); got != "GT" {
		t.Errorf("sin metadata debería dar GT, dio %q", got)
	}
}
