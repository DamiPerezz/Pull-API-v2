-- =============================================================================
-- Estados nuevos para el flujo privado con dLocal Go (opción B):
-- "solicitar sin pagar → aprobar → pagar con enlace".
--
--   awaiting_approval : solicitud enviada, el staff aún no decide. SIN dinero.
--   approved_unpaid   : aprobada; esperando que el cliente pague su enlace.
--
-- dLocal Go NO soporta retener dinero sin cobrarlo, así que el estado viejo
-- `payment_authorized` (la retención de NeoNet) deja de usarse para órdenes
-- nuevas. NO se borra: las órdenes históricas cobradas con Cybersource lo
-- siguen teniendo y deben poder consultarse/reembolsarse.
--
-- Aplicar en la BD de CADA VENUE (staging y producción).
-- Idempotente: se puede ejecutar varias veces sin error.
-- =============================================================================

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_enum e
    JOIN pg_type t ON t.oid = e.enumtypid
    WHERE t.typname = 'order_status' AND e.enumlabel = 'awaiting_approval'
  ) THEN
    ALTER TYPE order_status ADD VALUE 'awaiting_approval';
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_enum e
    JOIN pg_type t ON t.oid = e.enumtypid
    WHERE t.typname = 'order_status' AND e.enumlabel = 'approved_unpaid'
  ) THEN
    ALTER TYPE order_status ADD VALUE 'approved_unpaid';
  END IF;
END $$;

-- Comprobación: lista los valores del enum tras aplicar.
SELECT enumlabel AS estados_order_status
FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid
WHERE t.typname = 'order_status'
ORDER BY e.enumsortorder;
