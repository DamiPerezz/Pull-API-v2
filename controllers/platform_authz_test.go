package controllers

import "testing"

func TestPlatformRoleAllowed(t *testing.T) {
	cases := []struct {
		role    string
		allowed []string
		want    bool
		why     string
	}{
		// EL BUG QUE BLOQUEABA EL PANEL: super_admin debe pasar siempre.
		{"super_admin", []string{"admin", "analyst"}, true, "super_admin en lecturas"},
		{"super_admin", []string{"admin"}, true, "super_admin en escrituras"},
		{"super_admin", []string{"super_admin"}, true, "super_admin en borrado"},
		// Roles normales.
		{"admin", []string{"admin", "analyst"}, true, "admin lee"},
		{"admin", []string{"admin"}, true, "admin escribe"},
		{"admin", []string{"super_admin"}, false, "admin NO borra venues"},
		{"analyst", []string{"admin", "analyst"}, true, "analyst lee"},
		{"analyst", []string{"admin"}, false, "analyst NO escribe"},
		{"support", []string{"admin", "analyst", "support"}, true, "support ve transacciones"},
		{"support", []string{"admin", "analyst"}, false, "support NO ve ingresos"},
		{"viewer", []string{"admin", "analyst"}, false, "viewer no lee el panel"},
		{"viewer", []string{"admin"}, false, "viewer NO escribe"},
		{"viewer", []string{"super_admin"}, false, "viewer NO borra"},
		// Rol vacío = token sin claim: nunca pasa.
		{"", []string{"admin", "analyst"}, false, "rol vacio"},
		{"", []string{}, false, "rol vacio sin lista"},
		// Rol desconocido / typo.
		{"superadmin", []string{"admin"}, false, "typo sin guion bajo no es comodin"},
		{"Admin", []string{"admin"}, false, "distingue mayusculas"},
	}
	for _, c := range cases {
		if got := platformRoleAllowed(c.role, c.allowed...); got != c.want {
			t.Errorf("platformRoleAllowed(%q, %v) = %v, se esperaba %v (%s)",
				c.role, c.allowed, got, c.want, c.why)
		}
	}
}
