package controllers

// =============================================================================
// QUÉ ESTADOS SIGNIFICAN "EL DINERO YA ESTÁ COBRADO"
//
// Esto existe porque el mismo error se repitió en cinco sitios distintos, y en
// todos era invisible hasta la noche de un evento.
//
// EL ERROR: filtrar las órdenes por `status = "confirmed"` a secas para sumar
// dinero o contar ventas.
//
// POR QUÉ FALLA: `confirmed` NO es el estado final de una compra pagada.
// Cuando el portero escanea la última entrada de esa compra, la orden pasa a
// `checked_in` (mobile_compat_controller.go). El dinero es exactamente el
// mismo —ya se cobró— pero la orden deja de coincidir con el filtro.
//
// CÓMO SE VE DESDE FUERA: la noche del evento, según entra la gente, las
// cifras de dinero van BAJANDO. Con el local lleno, el panel de la dueña
// marcaba "Cobrado Q0,00" mientras el contador de "ya están dentro" subía. La
// pantalla se contradice a sí misma, y el histórico del evento queda falseado
// para siempre — con lo que el aforo y el precio del evento siguiente se
// deciden sobre un dato falso.
//
// Se detectó el 2026-08-23 auditando el panel del local, y estaba en:
//   event_stats_controller.go   las cifras del panel y de la app
//   staff_controller.go   ×2    ingresos de hoy / semana / mes
//   analytics_controller.go     la serie de ventas por día
//   wallet_controller.go        lo que ha gastado un cliente
//
// ⚠️ NO vale para todo. Úsalo solo cuando la pregunta sea "¿este dinero está
// cobrado?". Hay sitios donde `confirmed` a secas es lo correcto:
//
//   - Las ESCRITURAS condicionales (`UPDATE ... WHERE status = 'confirmed'`),
//     que son reclamaciones atómicas de una transición concreta.
//   - `group_reservations`, que usa OTRO enum (`reservation_status`:
//     pending|confirmed|closed|completed|cancelled). Ahí no existe checked_in.
//   - El reparador de órdenes confirmadas sin entradas (approval_expiry.go):
//     una orden escaneada tuvo por fuerza una entrada que escanear.
// =============================================================================

// estadosCobrados filtra las órdenes cuyo dinero YA ENTRÓ, en el formato de
// filtro de PostgREST.
//
//	confirmed   pagó y todavía no ha llegado al local
//	checked_in  pagó y ya está dentro
//
// El dinero es el mismo en los dos. Lo único que cambia es que alguien escaneó
// su QR en la puerta.
const estadosCobrados = "in.(confirmed,checked_in)"
