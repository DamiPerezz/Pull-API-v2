-- =============================================================================
-- CIERRE DE RLS — versión MÍNIMA y autónoma (2026-07-22)
-- SOLO quita las policies permisivas ("Service role full access" = roles
-- {public} USING(true), que dejan leer a la llave anon) y fuerza RLS en todo
-- el esquema public. NO crea tablas → nada que pueda dar error y hacer que el
-- editor de Supabase revierta el bloque entero (por eso la versión anterior
-- no tomó: el CREATE TABLE transactions falló y arrastró el DO block).
--
-- EJECUTAR EN LOS DOS PROYECTOS DE PRODUCCIÓN por separado:
--   central: supabase.com/dashboard/project/mwuppgpmlynfxyghkpzv/sql/new
--   venue:   supabase.com/dashboard/project/faioqaaaonucbnxpmpxx/sql/new
--
-- Seguro: el backend usa service_role (bypasea RLS) → sigue leyendo todo.
-- Idempotente: se puede correr varias veces. Verificado E2E en staging.
-- =============================================================================
DO $$
DECLARE r RECORD;
BEGIN
  FOR r IN SELECT policyname, tablename FROM pg_policies WHERE schemaname = 'public'
  LOOP
    EXECUTE format('DROP POLICY IF EXISTS %I ON public.%I', r.policyname, r.tablename);
  END LOOP;
  FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public'
  LOOP
    EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', r.tablename);
  END LOOP;
END $$;

-- Comprobación rápida (debe devolver 0 filas = ninguna policy permisiva viva):
SELECT tablename, policyname FROM pg_policies WHERE schemaname = 'public';
