package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// =============================================
// AUTORIZACIÓN DEL PANEL CENTRAL (platform staff)
// =============================================
//
// El enum `platform_staff_role` (sql/central_schema.sql:38) es:
//
//	viewer | analyst | support | admin | super_admin
//
// Los handlers del panel comprobaban a mano `role != "admin" && role !=
// "analyst"` y NUNCA contemplaban `super_admin`, que es el rol más alto y el
// único que existe hoy en staging y en producción. Resultado: el usuario que
// debería poder verlo todo recibía 403 en el panel entero. Estos dos helpers
// centralizan la regla para que no vuelva a divergir handler a handler.

// platformRoleAllowed indica si `role` está autorizado.
//
// `super_admin` es comodín: por definición del enum es el rol superior, así
// que pasa cualquier comprobación sin necesidad de listarlo en cada llamada.
// Un rol vacío nunca pasa (token sin claim `role` = sin permisos).
func platformRoleAllowed(role string, allowed ...string) bool {
	if role == "" {
		return false
	}
	if role == platformRoleSuperAdmin {
		return true
	}
	for _, a := range allowed {
		if role == a {
			return true
		}
	}
	return false
}

// requirePlatformRole comprueba el rol del contexto y, si no está autorizado,
// responde 403 y devuelve false. El handler debe hacer `return` inmediato:
//
//	if !requirePlatformRole(c, platformRoleAdmin, platformRoleAnalyst) {
//	    return
//	}
func requirePlatformRole(c *gin.Context, allowed ...string) bool {
	role := c.GetString("role")
	if platformRoleAllowed(role, allowed...) {
		return true
	}
	c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
	return false
}

// Nombres de rol del enum platform_staff_role. Constantes para que un typo sea
// un error de compilación y no un 403 silencioso en producción.
//
// platformRoleViewer no lo pasa ningún handler como permitido: es el rol por
// defecto del schema y no debe poder hacer nada del panel. Se declara para
// tener el enum completo en un sitio.
const (
	platformRoleViewer     = "viewer"
	platformRoleAnalyst    = "analyst"
	platformRoleSupport    = "support"
	platformRoleAdmin      = "admin"
	platformRoleSuperAdmin = "super_admin"
)
