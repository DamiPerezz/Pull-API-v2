-- =============================================================================
-- Tabla `transactions` (BD CENTRAL) — el ledger de plataforma de Pull.
-- El código (services/database_router.go RecordTransaction + analytics) usa
-- la tabla `transactions`, pero central_schema.sql definía una
-- `platform_transactions` que nada lee: el ledger nunca persistió (el insert
-- es fire-and-forget y fallaba en silencio). Descubierto 2026-07-22 en la
-- limpieza pre-evento. Aplicar en la central de CADA entorno (staging + prod).
-- Columnas = exactamente las que escribe RecordTransaction; ids de referencia
-- sin FK (apuntan a la BD del venue, otro proyecto) y user/organization como
-- text (el código puede mandar cadena vacía en compras anónimas).
-- =============================================================================
CREATE TABLE IF NOT EXISTS transactions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  transaction_number text,
  transaction_type text NOT NULL,
  status text NOT NULL,
  gross_amount numeric NOT NULL DEFAULT 0,
  currency text NOT NULL DEFAULT 'GTQ',
  platform_fee_percent numeric DEFAULT 0,
  platform_fee_amount numeric DEFAULT 0,
  gateway_fee_percent numeric DEFAULT 0,
  gateway_fee_fixed numeric DEFAULT 0,
  gateway_fee_amount numeric DEFAULT 0,
  net_to_venue numeric DEFAULT 0,
  venue_id uuid NOT NULL,
  organization_id text,
  event_id uuid,
  user_id text,
  order_id uuid,
  group_reservation_id uuid,
  group_guest_id uuid,
  vip_list_id uuid,
  vip_list_guest_id uuid,
  ticket_id uuid,
  payment_gateway text,
  stripe_payment_intent text,
  stripe_charge_id text,
  stripe_session_id text,
  stripe_transfer_id text,
  stripe_balance_transaction text,
  stripe_refund_id text,
  neonet_transaction_id text,
  neonet_authorization_code text,
  neonet_reference text,
  neonet_reason_code text,
  mercadopago_payment_id text,
  mercadopago_preference_id text,
  payer_name text,
  payer_email text,
  payer_phone text,
  card_last4 text,
  card_brand text,
  card_type text,
  metadata jsonb DEFAULT '{}'::jsonb,
  internal_notes text,
  original_transaction_id uuid,
  refund_reason text,
  refunded_amount numeric DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  captured_at timestamptz,
  refunded_at timestamptz,
  failed_at timestamptz
);

CREATE INDEX IF NOT EXISTS idx_transactions_venue_created
  ON transactions (venue_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_transactions_type
  ON transactions (transaction_type);

-- RLS activado SIN policies: el backend usa service_role (bypasea RLS) y
-- así la llave pública anon no puede leer/escribir el ledger.
ALTER TABLE transactions ENABLE ROW LEVEL SECURITY;

-- =============================================================================
-- BLINDAJE RLS DE TODO EL ESQUEMA (2026-07-22)
-- Las plantillas central_schema.sql / venue_template.sql traían policies
-- "Service role full access" MAL DEFINIDAS: roles={public} USING(true) — o
-- sea, dejaban leer TODO a la llave anon (PII de clientes, hashes de staff,
-- venue_database_configs, payment_gateway_credentials). En esta arquitectura
-- NADA usa la anon (el backend entra con service_role, que bypasea RLS), así
-- que se quitan todas las policies y se fuerza RLS = anon denegada, backend
-- intacto. Verificado E2E en staging (smoke 16/16 con RLS cerrado).
-- Ejecutar en la central Y en cada BD de venue.
-- =============================================================================
DO $$
DECLARE r RECORD;
BEGIN
  FOR r IN SELECT policyname, tablename FROM pg_policies WHERE schemaname='public'
  LOOP
    EXECUTE format('DROP POLICY IF EXISTS %I ON public.%I', r.policyname, r.tablename);
  END LOOP;
  FOR r IN SELECT tablename FROM pg_tables WHERE schemaname='public'
  LOOP
    EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', r.tablename);
  END LOOP;
END $$;
