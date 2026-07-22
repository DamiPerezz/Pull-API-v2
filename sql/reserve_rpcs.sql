-- =============================================================================
-- RESERVA ATÓMICA DE AFORO (anti-oversell) — BD de cada VENUE.
-- El aforo lo lleva ticket_types.quantity_reserved (quantity_sold no se usa).
-- Antes, la reserva era un read-modify-write en un goroutine → dos compradores
-- simultáneos leían el mismo "quedan N" y ambos reservaban = SOBREVENTA.
-- Estas RPC hacen el check+incremento en UNA sentencia: el UPDATE toma el lock
-- de la fila, el 2º concurrente re-evalúa el WHERE contra el valor ya
-- actualizado (READ COMMITTED de Postgres) → imposible pasar del límite.
-- Aplicar en la BD de CADA venue (staging + prod) y añadido a venue_template.
-- =============================================================================

-- Reserva p_qty entradas de forma atómica. Devuelve las disponibles restantes
-- tras reservar, o -1 si no hay stock suficiente (o el ticket no existe/inactivo).
CREATE OR REPLACE FUNCTION reserve_ticket_type(p_id uuid, p_qty int)
RETURNS int
LANGUAGE plpgsql
AS $$
DECLARE
  remaining int;
BEGIN
  IF p_qty IS NULL OR p_qty <= 0 THEN
    RETURN -1;
  END IF;
  UPDATE ticket_types
     SET quantity_reserved = COALESCE(quantity_reserved, 0) + p_qty
   WHERE id = p_id
     AND COALESCE(is_active, true) = true
     AND (quantity_total - COALESCE(quantity_sold, 0) - COALESCE(quantity_reserved, 0)) >= p_qty
  RETURNING (quantity_total - COALESCE(quantity_sold, 0) - COALESCE(quantity_reserved, 0))
       INTO remaining;
  IF NOT FOUND THEN
    RETURN -1;   -- sin stock suficiente / inactivo / inexistente
  END IF;
  RETURN remaining;
END;
$$;

-- Libera p_qty entradas reservadas (carrito abandonado/expirado, cancelación).
-- Nunca baja de 0.
CREATE OR REPLACE FUNCTION release_ticket_type(p_id uuid, p_qty int)
RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
  IF p_qty IS NULL OR p_qty <= 0 THEN
    RETURN;
  END IF;
  UPDATE ticket_types
     SET quantity_reserved = GREATEST(0, COALESCE(quantity_reserved, 0) - p_qty)
   WHERE id = p_id;
END;
$$;
