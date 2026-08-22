package services

import "strings"

// =============================================================================
// MENSAJES DE RECHAZO PARA EL COMPRADOR
// =============================================================================
//
// Hasta el 2026-08-22 el mensaje de Cybersource se le enseñaba al comprador
// TAL CUAL. Un cliente de Guatemala, comprando una entrada un viernes por la
// noche desde el móvil, llegó a ver esto:
//
//	"The order has been rejected by Decision Manager"
//
// En inglés, nombrando un sistema interno del que no ha oído hablar, sin
// decirle qué hacer, y redactado como una acusación. Ese comprador no vuelve
// a intentarlo: se va.
//
// Estas son las reglas con las que están escritos los mensajes de aquí:
//
//  1. EN ESPAÑOL Y SIN JERGA. Ni "Decision Manager", ni "procesador", ni
//     "gateway", ni códigos. El comprador no sabe qué es nada de eso y
//     nombrárselo solo le hace sentir que el problema es suyo.
//
//  2. SIEMPRE DICEN QUÉ HACER. Un mensaje que solo dice "rechazado" deja a la
//     persona sin salida. Cada uno termina con una acción concreta: otra
//     tarjeta, llamar al banco, escribirnos.
//
//  3. NUNCA CULPAN AL COMPRADOR. "Tu banco no autorizó el pago" y no "tu
//     tarjeta ha sido rechazada". Casi nunca es culpa suya, y aunque lo fuera,
//     restregárselo no vende una entrada.
//
//  4. EN FRAUDE, VAGOS A PROPÓSITO. Cuando el rechazo viene del filtro
//     antifraude NO se explica el motivo. Por dos razones: al comprador
//     honrado no le sirve de nada, y al que no lo es le estaríamos diciendo
//     qué ajustar para el siguiente intento. Es la única categoría donde ser
//     impreciso es lo correcto.
//
//  5. NUNCA SE INVENTA UN DIAGNÓSTICO. Si el código no se reconoce, el
//     mensaje es genérico y honesto. Decirle "sin saldo" a alguien que sí
//     tenía saldo hace que llame al banco para nada.
//
// El código y el mensaje crudo SÍ se registran en el log, que es donde tienen
// que estar: para quien depura, no para quien compra.

// DeclineMessage traduce el motivo de rechazo de la pasarela a algo que una
// persona pueda leer y usar. `reason` es el código de Cybersource (p. ej.
// "INSUFFICIENT_FUND"); `raw` es su mensaje original, que solo se usa como
// último recurso y nunca se muestra si lleva jerga interna.
func DeclineMessage(reason, raw string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {

	// --- Filtro antifraude ---------------------------------------------
	// Deliberadamente sin detalle. Ver regla 4.
	case "DECISION_PROFILE_REJECT", "DECISION_PROFILE_REVIEW",
		"BLACKLISTED_CUSTOMER", "SCORE_EXCEEDS_THRESHOLD", "DENIED":
		return "No pudimos completar el pago con esta tarjeta. Prueba con otra " +
			"o escríbenos y lo resolvemos contigo."

	// --- El banco dice que no, y sabemos por qué ------------------------
	case "INSUFFICIENT_FUND", "EXCEEDS_CREDIT_LIMIT":
		return "Tu tarjeta no tiene saldo suficiente para este pago. Prueba con " +
			"otra tarjeta."

	case "EXPIRED_CARD":
		return "La tarjeta está vencida. Revisa la fecha o usa otra tarjeta."

	case "INVALID_ACCOUNT", "INVALID_CARD", "INVALID_CC_NUMBER", "STOLEN_LOST_CARD":
		return "Los datos de la tarjeta no son correctos. Revísalos e intenta " +
			"de nuevo."

	case "CV_FAILED", "INVALID_CVN":
		return "El código de seguridad (los 3 dígitos del reverso) no coincide. " +
			"Vuelve a escribirlo."

	case "AVS_FAILED":
		return "La dirección de facturación no coincide con la de tu tarjeta. " +
			"Revísala e intenta de nuevo."

	// --- El banco dice que no y no nos dice por qué ---------------------
	// Es el caso más común y el más frustrante. Aquí lo honesto es admitir
	// que no sabemos y mandarle a quien sí lo sabe.
	case "PROCESSOR_DECLINED", "GENERAL_DECLINE", "CONTACT_PROCESSOR",
		"CARD_TYPE_NOT_ACCEPTED", "DEBIT_CARD_USE_NOT_ALLOWED":
		return "Tu banco no autorizó el pago. Suele resolverse llamándoles o " +
			"probando con otra tarjeta."

	// --- Cosas que se arreglan reintentando -----------------------------
	case "PROCESSOR_UNAVAILABLE", "SYSTEM_ERROR", "TIMEOUT", "SERVICE_UNAVAILABLE":
		return "No pudimos contactar con el banco en este momento. Espera unos " +
			"segundos e inténtalo otra vez."

	case "DUPLICATE_REQUEST":
		return "Este pago ya se estaba procesando. Espera unos segundos y revisa " +
			"tu correo antes de volver a intentarlo."
	}

	// --- Desconocido -----------------------------------------------------
	// Regla 5: no inventar. Y regla 1: el mensaje crudo de la pasarela viene
	// en inglés y con jerga, así que NO se enseña aunque exista.
	_ = raw
	return "No pudimos completar el pago. Prueba con otra tarjeta o escríbenos " +
		"y lo resolvemos contigo."
}
