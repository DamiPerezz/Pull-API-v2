package services

import (
	"encoding/json"
	"strings"
	"testing"
)

// =============================================================================
// NINGÚN CAMPO VACÍO EN billTo.
//
// El 2026-08-24 esto tumbó los cobros en producción. Cybersource devuelve
// HTTP 400 INVALID_REQUEST si le llega `"address1": ""`, y lo hace SIN decir
// qué campo: orderInformation null, processorInformation null y reason en
// blanco. Tres compras seguidas rechazadas.
//
// Lo más engañoso del fallo: Decision Manager PASABA ("Early Success") y la
// que salía "Failed" era la autorización, así que parecía cosa del banco. No
// hubo banco: la petición murió en la puerta de Cybersource.
//
// La causa: el día antes se hizo que la dirección guatemalteca solo se
// rellenara para compradores de Guatemala, y que la pusiera el widget cuando
// billingType != NONE. Las dos cosas correctas — pero los campos quedaban como
// CADENA VACÍA en vez de desaparecer.
// =============================================================================

// billToDelCuerpo replica cómo Sale() monta el bloque billTo.
func billToDelCuerpo(b CybsBillTo) map[string]interface{} {
	bill := map[string]interface{}{}
	poner := func(clave, valor string) {
		if v := strings.TrimSpace(valor); v != "" {
			bill[clave] = v
		}
	}
	poner("firstName", b.FirstName)
	poner("lastName", b.LastName)
	poner("email", b.Email)
	poner("address1", b.Address1)
	poner("locality", b.Locality)
	poner("administrativeArea", b.AdminArea)
	poner("postalCode", b.PostalCode)
	poner("country", b.Country)
	poner("phoneNumber", b.Phone)
	return bill
}

func TestBillToNuncaMandaCamposVacios(t *testing.T) {
	casos := []struct {
		nombre string
		billTo CybsBillTo
		// claves que TIENEN que estar; las demás no deben aparecer
		quiero []string
	}{
		{
			nombre: "comprador guatemalteco: va todo",
			billTo: CybsBillTo{
				FirstName: "DAMIÁN", LastName: "PÉREZ", Email: "d@x.com",
				Phone: "50255512345", Address1: "Ciudad de Guatemala",
				Locality: "Guatemala", AdminArea: "GT", PostalCode: "01001",
				Country: "GT",
			},
			quiero: []string{"firstName", "lastName", "email", "phoneNumber",
				"address1", "locality", "administrativeArea", "postalCode", "country"},
		},
		{
			// EL CASO QUE ROMPIÓ PRODUCCIÓN: comprador no guatemalteco, sin
			// dirección inventada.
			nombre: "comprador de fuera: sin direccion, y sin claves vacias",
			billTo: CybsBillTo{
				FirstName: "Damian", LastName: "Perez", Email: "d@x.com",
				Phone: "34687120072", Country: "ES",
			},
			quiero: []string{"firstName", "lastName", "email", "phoneNumber", "country"},
		},
		{
			// La direccion la recoge el widget (billingType PARTIAL/FULL) y
			// viaja dentro del transient token, no aqui.
			nombre: "la direccion la pone el widget",
			billTo: CybsBillTo{
				FirstName: "Ana", LastName: "Lopez", Email: "a@x.com", Country: "GT",
			},
			quiero: []string{"firstName", "lastName", "email", "country"},
		},
		{
			nombre: "sin telefono: la clave no aparece",
			billTo: CybsBillTo{FirstName: "Ana", LastName: "Lopez", Email: "a@x.com", Country: "GT"},
			quiero: []string{"firstName", "lastName", "email", "country"},
		},
		{
			nombre: "espacios en blanco cuentan como vacio",
			billTo: CybsBillTo{
				FirstName: "Ana", LastName: "Lopez", Email: "a@x.com",
				Address1: "   ", PostalCode: "\t", Country: "GT",
			},
			quiero: []string{"firstName", "lastName", "email", "country"},
		},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			bill := billToDelCuerpo(c.billTo)

			// 1. NINGUNA clave con valor vacío. Es la regla que importa.
			for k, v := range bill {
				s, _ := v.(string)
				if strings.TrimSpace(s) == "" {
					t.Errorf("la clave %q viaja VACÍA — Cybersource devuelve 400", k)
				}
			}
			// 2. Están exactamente las que se esperan, ni una más.
			if len(bill) != len(c.quiero) {
				b, _ := json.Marshal(bill)
				t.Errorf("claves = %d, esperaba %d: %s", len(bill), len(c.quiero), b)
			}
			for _, k := range c.quiero {
				if _, ok := bill[k]; !ok {
					t.Errorf("falta la clave %q", k)
				}
			}
		})
	}
}

// TestBillToVacioDelTodo: si no supiéramos NADA del comprador, el bloque sale
// vacío en vez de con nueve cadenas vacías. Un billTo ausente lo acepta
// Cybersource; uno lleno de "" no.
func TestBillToVacioDelTodo(t *testing.T) {
	bill := billToDelCuerpo(CybsBillTo{})
	if len(bill) != 0 {
		b, _ := json.Marshal(bill)
		t.Errorf("con billTo vacío no debería viajar ninguna clave, viajan: %s", b)
	}
}
