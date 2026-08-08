-- =============================================================================
-- FASE 0 — paridad de esquema para la migración a dLocal Go.
-- BLOQUEANTE de todo lo demás. Aplicar en la BD de CADA VENUE y en la CENTRAL.
--
-- Qué arregla (según auditoría 2026-08-07):
--   1. `payment_gateway_type` no admite 'dlocal' en NINGÚN entorno → escribir
--      ese valor daría 22P02 y tumbaría el UPDATE entero (cobro hecho, ticket
--      nunca emitido: el peor escenario, ya nos pasó una vez con NeoNet).
--   2. Producción no tiene los estados nuevos del flujo privado, ni algunos
--      que staging sí tiene (drift).
--   3. Producción NO tiene las funciones de aforo → cada rechazo/caducidad
--      perdería una plaza para siempre, y no hay garantía anti-sobreventa.
--
-- Todo es ADITIVO e idempotente: no borra nada ni cambia datos existentes.
-- Las órdenes históricas con gateway 'neonet' se conservan intactas.
-- =============================================================================

-- ── 1. Catálogo de pasarelas: admitir 'dlocal' ──────────────────────────────
-- (existe tanto en la BD del venue como en la central)
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_type WHERE typname = 'payment_gateway_type')
     AND NOT EXISTS (
       SELECT 1 FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid
       WHERE t.typname = 'payment_gateway_type' AND e.enumlabel = 'dlocal'
     ) THEN
    ALTER TYPE payment_gateway_type ADD VALUE 'dlocal';
  END IF;
END $$;

-- ── 2. Estados de orden del flujo privado nuevo ─────────────────────────────
DO $$
DECLARE v text;
BEGIN
  IF EXISTS (SELECT 1 FROM pg_type WHERE typname = 'order_status') THEN
    FOREACH v IN ARRAY ARRAY['awaiting_approval','approved_unpaid','payment_failed','checked_in','expired','refunded']
    LOOP
      IF NOT EXISTS (
        SELECT 1 FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid
        WHERE t.typname = 'order_status' AND e.enumlabel = v
      ) THEN
        EXECUTE format('ALTER TYPE order_status ADD VALUE %L', v);
      END IF;
    END LOOP;
  END IF;
END $$;

-- ── 3. Aforo atómico (anti-sobreventa) — solo en la BD del VENUE ────────────
-- Si la tabla ticket_types no existe (estamos en la central), no hace nada.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema='public' AND table_name='ticket_types') THEN

    EXECUTE $fn$
    CREATE OR REPLACE FUNCTION reserve_ticket_type(p_id uuid, p_qty int)
    RETURNS int
    LANGUAGE plpgsql
    AS $body$
    DECLARE remaining int;
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
        RETURN -1;
      END IF;
      RETURN remaining;
    END;
    $body$;
    $fn$;

    EXECUTE $fn$
    CREATE OR REPLACE FUNCTION release_ticket_type(p_id uuid, p_qty int)
    RETURNS void
    LANGUAGE plpgsql
    AS $body$
    BEGIN
      IF p_qty IS NULL OR p_qty <= 0 THEN
        RETURN;
      END IF;
      UPDATE ticket_types
         SET quantity_reserved = GREATEST(0, COALESCE(quantity_reserved, 0) - p_qty)
       WHERE id = p_id;
    END;
    $body$;
    $fn$;

  END IF;
END $$;

-- ── 4. Verificación ─────────────────────────────────────────────────────────
SELECT 'order_status' AS enum, string_agg(e.enumlabel, ', ' ORDER BY e.enumsortorder) AS valores
FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid WHERE t.typname='order_status'
UNION ALL
SELECT 'payment_gateway_type', string_agg(e.enumlabel, ', ' ORDER BY e.enumsortorder)
FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid WHERE t.typname='payment_gateway_type'
UNION ALL
SELECT 'rpc_aforo', string_agg(proname, ', ')
FROM pg_proc WHERE proname IN ('reserve_ticket_type','release_ticket_type');
