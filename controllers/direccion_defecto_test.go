package controllers

import "testing"

// Cybersource EXIGE address1 y locality. Vacios dan INVALID_DATA y ausentes
// dan MISSING_FIELD: los dos son HTTP 400. Este test fija que el bloque sale
// siempre completo, y coherente con el pais declarado.
func TestDireccionSiempreCompleta(t *testing.T) {
	for _, pais := range []string{"GT", "ES", "MX", "US", "SV", "XX", ""} {
		d := direccionPorDefecto(pais)
		if d.calle == "" || d.ciudad == "" || d.region == "" || d.postal == "" {
			t.Errorf("pais %q deja un campo vacio: %+v", pais, d)
		}
	}
	if got := direccionPorDefecto("XX"); got.ciudad != "Guatemala" {
		t.Errorf("un pais desconocido debe caer a Guatemala, dio %q", got.ciudad)
	}
	if got := direccionPorDefecto("ES"); got.ciudad != "Madrid" {
		t.Errorf("ES deberia dar Madrid, dio %q", got.ciudad)
	}
}
