#!/usr/bin/env bash
# =============================================
# SETUP STAGING — 511 Events (pull-api-v2-staging)
# Monta el entorno staging COMPLETO contra los 2 proyectos Supabase de
# staging recién creados (central-staging + venue-staging):
#
#   1. Esquema:  sql/central_schema.sql → central staging,
#                sql/venue_template.sql → venue staging  (Management API)
#                + DDL compat (payment_gateway_credentials y columnas
#                supabase_*_encrypted que el backend lee pero que el
#                central_schema.sql del repo no trae — drift conocido).
#   2. Seed central: venues (slug 511-events) + venue_database_configs
#                (credenciales del venue staging cifradas con APP_KEY
#                de STAGING via cmd/recrypt -encrypt).
#   3. Pasarela: copia la fila payment_gateway_credentials SANDBOX del
#                venue de PROD (solo LECTURA de prod), re-cifra el
#                secret con cmd/recrypt (OLD=APP_KEY prod → NEW=APP_KEY
#                staging) e inserta con environment=test.
#   4. Seed venue: admin staff + evento público "Staging Test Night"
#                con ticket General Q100.
#   5. Sube los 6 SUPABASE_* como secrets a Fly (--stage).
#
# Uso (desde Pull-API-v2/):
#   SUPABASE_ACCESS_TOKEN="sbp_..." bash scripts/setup_staging.sh
#
# Requiere: .env.staging.local con los 6 SUPABASE_* rellenos,
#           .env.prod.local presente, go y python en PATH.
# Idempotente razonable: si una fila ya existe (venue por slug, etc.)
# la reutiliza con aviso en vez de duplicar.
# NUNCA imprime secretos en claro, con UNA excepción deliberada: la
# password del admin recién generada SÍ se muestra una única vez en el
# resumen final (además de persistirse en .env.staging.local) para que
# el operador la guarde en su gestor de contraseñas.
# =============================================
set -euo pipefail
cd "$(dirname "$0")/.."

ENV_STG=".env.staging.local"
ENV_PROD=".env.prod.local"
IDS_FILE=".staging.ids"
FLY_APP="pull-api-v2-staging"
PROD_VENUE_ID="5d3a4758-fabb-46a2-9dc3-86b2ee8bcafa"   # venue 511 en central PROD
ADMIN_EMAIL="damian.perez@greenlock.tech"
ORG_SLUG="511-staging"
VENUE_SLUG="511-events"    # NO cambiar: la web staging usa VITE_DEFAULT_VENUE_SLUG=511-events
VENUE_NAME="511 STAGING"
EVENT_SLUG="staging-test-night"
EVENT_NAME="Staging Test Night"

TMP_WORK=$(mktemp -d)
trap 'rm -rf "$TMP_WORK"' EXIT

env_get() { grep "^${2}=" "$1" 2>/dev/null | head -1 | cut -d= -f2- | tr -d '\r'; }

PYBIN=$(command -v python || command -v python3 || true)

# ── 0. Precondiciones ────────────────────────────────────────────────
echo "== 0. Precondiciones =="
fail=0
[ -f "$ENV_STG" ]  || { echo "  ✘ falta $ENV_STG"; fail=1; }
[ -f "$ENV_PROD" ] || { echo "  ✘ falta $ENV_PROD (necesario para migrar la pasarela sandbox)"; fail=1; }
command -v go >/dev/null 2>&1   || { echo "  ✘ go no está en PATH"; fail=1; }
command -v curl >/dev/null 2>&1 || { echo "  ✘ curl no está en PATH"; fail=1; }
[ -n "$PYBIN" ] || { echo "  ✘ python no está en PATH"; fail=1; }
[ -n "${SUPABASE_ACCESS_TOKEN:-}" ] || {
  echo "  ✘ falta SUPABASE_ACCESS_TOKEN (token sbp_... de la Management API)."
  echo "    Genéralo en https://supabase.com/dashboard/account/tokens y ejecuta:"
  echo "    SUPABASE_ACCESS_TOKEN=\"sbp_...\" bash scripts/setup_staging.sh"
  fail=1
}
[ "$fail" -eq 0 ] || exit 1

STG_APP_KEY=$(env_get "$ENV_STG" APP_KEY)
STG_CENTRAL_URL=$(env_get "$ENV_STG" CENTRAL_SUPABASE_URL)
STG_CENTRAL_KEY=$(env_get "$ENV_STG" CENTRAL_SERVICE_KEY)
STG_CENTRAL_ANON=$(env_get "$ENV_STG" CENTRAL_ANON_KEY)
STG_VENUE_URL=$(env_get "$ENV_STG" DEFAULT_SUPABASE_URL)
STG_VENUE_KEY=$(env_get "$ENV_STG" DEFAULT_SERVICE_KEY)
STG_VENUE_ANON=$(env_get "$ENV_STG" DEFAULT_ANON_KEY)
PROD_APP_KEY=$(env_get "$ENV_PROD" APP_KEY)
PROD_CENTRAL_URL=$(env_get "$ENV_PROD" CENTRAL_SUPABASE_URL)
PROD_CENTRAL_KEY=$(env_get "$ENV_PROD" CENTRAL_SERVICE_KEY)

for pair in \
  "APP_KEY:$STG_APP_KEY" \
  "CENTRAL_SUPABASE_URL:$STG_CENTRAL_URL" "CENTRAL_SERVICE_KEY:$STG_CENTRAL_KEY" \
  "CENTRAL_ANON_KEY:$STG_CENTRAL_ANON" \
  "DEFAULT_SUPABASE_URL:$STG_VENUE_URL" "DEFAULT_SERVICE_KEY:$STG_VENUE_KEY" \
  "DEFAULT_ANON_KEY:$STG_VENUE_ANON"; do
  [ -n "${pair#*:}" ] || { echo "  ✘ ${pair%%:*} vacío en $ENV_STG — rellénalo (Supabase → Settings → API)"; fail=1; }
done
for pair in \
  "APP_KEY:$PROD_APP_KEY" \
  "CENTRAL_SUPABASE_URL:$PROD_CENTRAL_URL" "CENTRAL_SERVICE_KEY:$PROD_CENTRAL_KEY"; do
  [ -n "${pair#*:}" ] || { echo "  ✘ ${pair%%:*} vacío en $ENV_PROD"; fail=1; }
done
[ "$fail" -eq 0 ] || exit 1
[ ${#STG_APP_KEY} -eq 64 ] || { echo "  ✘ APP_KEY de staging debe tener 64 hex chars (tiene ${#STG_APP_KEY})"; exit 1; }

ref_of() { printf '%s\n' "$1" | sed -E 's#^https?://([^./]+)\.supabase\.co/?.*$#\1#'; }
CREF=$(ref_of "$STG_CENTRAL_URL")
VREF=$(ref_of "$STG_VENUE_URL")
for r in "$CREF" "$VREF"; do
  case "$r" in
    ""|*[!a-z0-9]*) echo "  ✘ no pude extraer un project ref válido de las URLs de $ENV_STG (obtuve: '$r')"; exit 1 ;;
  esac
done
[ "$CREF" != "$VREF" ] || { echo "  ✘ CENTRAL_SUPABASE_URL y DEFAULT_SUPABASE_URL apuntan al MISMO proyecto ($CREF) — deben ser 2 proyectos distintos"; exit 1; }
echo "  ✔ precondiciones OK (central staging: $CREF, venue staging: $VREF)"

# ── Helpers ──────────────────────────────────────────────────────────

# apply_sql <project_ref> <sql_file> <label>
# Aplica un .sql vía Management API (POST /v1/projects/{ref}/database/query).
# Primero intenta el archivo ENTERO en una sola query; si la API lo rechaza
# (tamaño u otro error) lo trocea POR STATEMENT con un splitter que respeta
# dollar-quoting ($$...$$), strings y comentarios, tolerando los errores de
# "ya existe" (re-runs; cada statement tolerado se loggea a stderr).
# Limitaciones del splitter: los comentarios de bloque ANIDADOS
# (/* /* */ */) NO están soportados; los statements que quedan vacíos
# tras quitar comentarios y espacios se descartan (p.ej. el bloque final
# de solo-comentarios de central_schema.sql).
# Fallback final documentado: pegar el archivo en el SQL Editor del
# dashboard.
apply_sql() {
  local ref="$1" file="$2" label="$3"
  echo "  → aplicando $file al proyecto $ref ($label)"
  SB_REF="$ref" SB_SQL_FILE="$file" "$PYBIN" - <<'PY'
import json, os, re, sys, urllib.request, urllib.error

ref  = os.environ["SB_REF"]
path = os.environ["SB_SQL_FILE"]
tok  = os.environ["SUPABASE_ACCESS_TOKEN"]
url  = "https://api.supabase.com/v1/projects/%s/database/query" % ref
sql  = open(path, encoding="utf-8").read()

def run(q):
    body = json.dumps({"query": q}).encode()
    req = urllib.request.Request(url, data=body, method="POST", headers={
        "Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            return r.status, r.read().decode(errors="replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")
    except Exception as e:
        return 0, str(e)

def split_sql(text):
    stmts, buf = [], []
    i, n = 0, len(text)
    in_str = in_line = in_block = False
    dollar = None
    while i < n:
        ch = text[i]; two = text[i:i+2]
        if in_line:
            buf.append(ch)
            if ch == "\n": in_line = False
            i += 1; continue
        if in_block:
            if two == "*/": buf.append("*/"); i += 2; in_block = False
            else: buf.append(ch); i += 1
            continue
        if dollar:
            if text.startswith(dollar, i):
                buf.append(dollar); i += len(dollar); dollar = None
            else:
                buf.append(ch); i += 1
            continue
        if in_str:
            buf.append(ch)
            if ch == "'":
                if text[i+1:i+2] == "'": buf.append("'"); i += 2; continue
                in_str = False
            i += 1; continue
        if two == "--": in_line = True; buf.append(ch); i += 1; continue
        if two == "/*": in_block = True; buf.append(ch); i += 1; continue
        if ch == "'": in_str = True; buf.append(ch); i += 1; continue
        if ch == "$":
            m = re.match(r"\$[A-Za-z_0-9]*\$", text[i:])
            if m:
                dollar = m.group(0); buf.append(dollar); i += len(dollar); continue
        if ch == ";":
            s = "".join(buf).strip()
            if s: stmts.append(s)
            buf = []; i += 1; continue
        buf.append(ch); i += 1
    s = "".join(buf).strip()
    if s: stmts.append(s)
    return stmts

def comment_only(stmt):
    # True si el statement queda vacio tras quitar comentarios y espacios.
    # Suficiente para descartar bloques de solo-comentarios; los
    # comentarios de bloque anidados NO estan soportados.
    s = re.sub(r"/\*.*?\*/", "", stmt, flags=re.S)
    s = re.sub(r"--[^\n]*", "", s)
    return not s.strip()

status, body = run(sql)
if 200 <= status < 300:
    print("    OK (archivo entero, 1 query)")
    sys.exit(0)

print("    la query entera fallo (HTTP %s) — reintento por statement..." % status)
# OJO: nada de 'duplicate' generico — matchearia el 23505 de un seed
# fallido ('duplicate key value violates unique constraint') y lo
# tragariamos en silencio. Solo errores DDL de "ya existe".
tolerable = ("already exists", "42710", "42p07", "42701", "42p16", "duplicate object")
stmts = [s for s in split_sql(sql) if not comment_only(s)]
skipped = 0
for k, st in enumerate(stmts, 1):
    status, body = run(st)
    if 200 <= status < 300:
        continue
    low = body.lower()
    if any(t in low for t in tolerable):
        skipped += 1
        first = st.splitlines()[0][:120] if st.splitlines() else ""
        print("    tolerado (ya existia) stmt %d/%d: %s" % (k, len(stmts), first),
              file=sys.stderr)
        continue
    if status == 401:
        print("    ERROR: SUPABASE_ACCESS_TOKEN invalido o expirado (401).")
        sys.exit(1)
    print("    ERROR en statement %d/%d (HTTP %s):" % (k, len(stmts), status))
    print("      " + st[:160].replace("\n", " "))
    print("      respuesta: " + body[:300])
    print("    FALLBACK manual: pega el contenido de %s en el SQL Editor:" % path)
    print("      https://supabase.com/dashboard/project/%s/sql/new" % ref)
    sys.exit(1)
print("    OK (%d statements, %d ya existian)" % (len(stmts), skipped))
PY
}

# rest <base_url> <service_key> <method> <path> [json|@file]
# PostgREST. Deja el HTTP code en REST_CODE y el body en $REST_BODY (archivo).
REST_BODY="$TMP_WORK/rest_body.json"
rest() {
  local base="$1" key="$2" method="$3" path="$4" data="${5:-}"
  if [ -n "$data" ]; then
    REST_CODE=$(curl -s -o "$REST_BODY" -w '%{http_code}' -X "$method" \
      "$base/rest/v1/$path" \
      -H "apikey: $key" -H "Authorization: Bearer $key" \
      -H "Content-Type: application/json" -H "Prefer: return=representation" \
      -d "$data")
  else
    REST_CODE=$(curl -s -o "$REST_BODY" -w '%{http_code}' -X "$method" \
      "$base/rest/v1/$path" \
      -H "apikey: $key" -H "Authorization: Bearer $key")
  fi
}

# jfield <field> — extrae el campo de la primera fila de $REST_BODY (o '')
jfield() {
  "$PYBIN" - "$REST_BODY" "$1" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print(""); raise SystemExit
if isinstance(d, list):
    d = d[0] if d else {}
v = d.get(sys.argv[2], "") if isinstance(d, dict) else ""
print("" if v is None else v)
PY
}

# jcount — número de filas del array JSON en $REST_BODY (-1 si no parsea)
jcount() {
  "$PYBIN" - "$REST_BODY" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print(-1); raise SystemExit
print(len(d) if isinstance(d, list) else -1)
PY
}

gen_uuid() { { uuidgen 2>/dev/null || "$PYBIN" -c "import uuid;print(uuid.uuid4())"; } | tr 'A-Z' 'a-z'; }

# persist_admin_creds — escribe STAFF_ADMIN_EMAIL/STAFF_ADMIN_PASSWORD en
# $ENV_STG (mismo convenio que .env.prod.local). Se llama INMEDIATAMENTE
# tras crear/rehashear el admin para que un fallo posterior (p.ej. flyctl)
# no pierda la password para siempre. ADMIN_PASS es token_urlsafe
# ([A-Za-z0-9_-]) — seguro para sed sin escapar.
persist_admin_creds() {
  if grep -q '^STAFF_ADMIN_EMAIL=' "$ENV_STG"; then
    sed -i "s|^STAFF_ADMIN_EMAIL=.*|STAFF_ADMIN_EMAIL=$ADMIN_EMAIL|" "$ENV_STG"
  else
    {
      echo ""
      echo "# Staff admin de staging (generado por scripts/setup_staging.sh)"
      echo "STAFF_ADMIN_EMAIL=$ADMIN_EMAIL"
    } >> "$ENV_STG"
  fi
  if grep -q '^STAFF_ADMIN_PASSWORD=' "$ENV_STG"; then
    sed -i "s|^STAFF_ADMIN_PASSWORD=.*|STAFF_ADMIN_PASSWORD=$ADMIN_PASS|" "$ENV_STG"
  else
    echo "STAFF_ADMIN_PASSWORD=$ADMIN_PASS" >> "$ENV_STG"
  fi
  echo "  ✔ STAFF_ADMIN_* persistidos en $ENV_STG"
}

# ── 1. Esquemas via Management API ───────────────────────────────────
echo "== 1. Esquema SQL (Management API) =="
# OJO: sql/venue_database_template.sql esta DEPRECADO — NO usar.
apply_sql "$CREF" "sql/central_schema.sql" "central staging"

# DDL compat: el central_schema.sql del repo esta desfasado respecto a lo
# que lee el codigo (services/database_router.go espera columnas
# supabase_*_encrypted en venue_database_configs, y services/payment_router.go
# lee una tabla payment_gateway_credentials que el schema no crea).
COMPAT_SQL="$TMP_WORK/central_compat.sql"
cat > "$COMPAT_SQL" <<'SQL'
-- Compat con el backend real (drift conocido de sql/central_schema.sql):
-- 1) venue_database_configs: el codigo lee supabase_service_key_encrypted /
--    supabase_anon_key (ver services/database_router.go y
--    sql/register_pull_plus.sql); service_key/anon_key del schema no se usan.
ALTER TABLE venue_database_configs ALTER COLUMN service_key DROP NOT NULL;
ALTER TABLE venue_database_configs ALTER COLUMN anon_key DROP NOT NULL;
ALTER TABLE venue_database_configs ADD COLUMN IF NOT EXISTS supabase_service_key_encrypted TEXT;
ALTER TABLE venue_database_configs ADD COLUMN IF NOT EXISTS supabase_anon_key TEXT;
ALTER TABLE venue_database_configs ADD COLUMN IF NOT EXISTS migration_status TEXT DEFAULT 'completed';

-- 2) payment_gateway_credentials: tabla que lee services/payment_router.go
--    (loadPaymentConfig). gateway como TEXT a proposito: el valor viene de
--    la fila de prod y no queremos atarnos al enum del schema.
CREATE TABLE IF NOT EXISTS payment_gateway_credentials (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    venue_id UUID NOT NULL REFERENCES venues(id) ON DELETE CASCADE,
    gateway TEXT NOT NULL DEFAULT 'neonet',
    gateway_name TEXT,
    is_active BOOLEAN DEFAULT true,
    is_primary BOOLEAN DEFAULT true,
    priority INT DEFAULT 1,
    environment TEXT NOT NULL DEFAULT 'test',
    platform_fee_percent DECIMAL(5,2) DEFAULT 0,
    platform_fee_fixed DECIMAL(10,2) DEFAULT 0,
    gateway_fee_percent DECIMAL(5,2) DEFAULT 0,
    gateway_fee_fixed DECIMAL(10,2) DEFAULT 0,
    default_currency TEXT DEFAULT 'GTQ',
    profile_id TEXT,
    access_key TEXT,
    merchant_id TEXT,
    terminal_id TEXT,
    secret_key_encrypted TEXT,
    stripe_account_id TEXT,
    stripe_publishable_key TEXT,
    stripe_webhook_secret_encrypted TEXT,
    mercadopago_public_key TEXT,
    mercadopago_access_token_encrypted TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(venue_id, gateway)
);
ALTER TABLE payment_gateway_credentials ENABLE ROW LEVEL SECURITY;
DO $$ BEGIN
    CREATE POLICY "Service role full access" ON payment_gateway_credentials
        FOR ALL USING (true) WITH CHECK (true);
EXCEPTION WHEN duplicate_object THEN NULL; END $$;
SQL
apply_sql "$CREF" "$COMPAT_SQL" "compat central staging"
apply_sql "$VREF" "sql/venue_template.sql" "venue staging"

# ── 2. Seed de la central staging (PostgREST) ────────────────────────
echo "== 2. Seed central staging (venues + venue_database_configs) =="

rest "$STG_CENTRAL_URL" "$STG_CENTRAL_KEY" GET "organizations?slug=eq.$ORG_SLUG&select=id"
ORG_ID=$(jfield id)
if [ -n "$ORG_ID" ]; then
  echo "  ⚠ organización $ORG_SLUG ya existe — reutilizo ($ORG_ID)"
else
  ORG_ID=$(gen_uuid)
  rest "$STG_CENTRAL_URL" "$STG_CENTRAL_KEY" POST "organizations" \
    "{\"id\":\"$ORG_ID\",\"name\":\"511 STAGING org\",\"slug\":\"$ORG_SLUG\",\"contact_email\":\"$ADMIN_EMAIL\",\"default_currency\":\"GTQ\",\"is_active\":true}"
  [ "$REST_CODE" = "201" ] || { echo "  ✘ ERROR creando organización (HTTP $REST_CODE):"; cat "$REST_BODY"; exit 1; }
  echo "  ✔ organización creada ($ORG_ID)"
fi

rest "$STG_CENTRAL_URL" "$STG_CENTRAL_KEY" GET "venues?slug=eq.$VENUE_SLUG&select=id"
VENUE_ID=$(jfield id)
if [ -n "$VENUE_ID" ]; then
  echo "  ⚠ venue $VENUE_SLUG ya existe — reutilizo ($VENUE_ID)"
else
  VENUE_ID=$(gen_uuid)
  rest "$STG_CENTRAL_URL" "$STG_CENTRAL_KEY" POST "venues" \
    "{\"id\":\"$VENUE_ID\",\"organization_id\":\"$ORG_ID\",\"name\":\"$VENUE_NAME\",\"slug\":\"$VENUE_SLUG\",\"description\":\"Entorno staging de 511 Events (dinero de mentira, Cybersource sandbox)\",\"city\":\"Guatemala City\",\"country\":\"GT\",\"currency\":\"GTQ\",\"timezone\":\"America/Guatemala\",\"platform_fee_percent\":8,\"platform_fee_fixed\":0,\"primary_payment_gateway\":\"neonet\",\"use_vip_list_flow\":false,\"use_guest_list\":true,\"use_group_reservations\":true,\"is_active\":true}"
  [ "$REST_CODE" = "201" ] || { echo "  ✘ ERROR creando venue (HTTP $REST_CODE):"; cat "$REST_BODY"; exit 1; }
  echo "  ✔ venue creado: $VENUE_NAME ($VENUE_ID, slug $VENUE_SLUG, fee 8%)"
fi

rest "$STG_CENTRAL_URL" "$STG_CENTRAL_KEY" GET "venue_database_configs?venue_id=eq.$VENUE_ID&select=id"
DBCFG_ID=$(jfield id)
if [ -n "$DBCFG_ID" ]; then
  echo "  ⚠ venue_database_configs ya existe para el venue — lo dejo como está"
else
  echo "  → cifrando credenciales del Supabase venue staging con la APP_KEY de STAGING (cmd/recrypt -encrypt)"
  ENC_SVC=$(printf '%s' "$STG_VENUE_KEY"  | NEW_KEY="$STG_APP_KEY" go run ./cmd/recrypt -encrypt)
  ENC_ANON=$(printf '%s' "$STG_VENUE_ANON" | NEW_KEY="$STG_APP_KEY" go run ./cmd/recrypt -encrypt)
  [ -n "$ENC_SVC" ] && [ -n "$ENC_ANON" ] || { echo "  ✘ cmd/recrypt no devolvió ciphertext"; exit 1; }
  rest "$STG_CENTRAL_URL" "$STG_CENTRAL_KEY" POST "venue_database_configs" \
    "{\"venue_id\":\"$VENUE_ID\",\"supabase_url\":\"$STG_VENUE_URL\",\"supabase_service_key_encrypted\":\"$ENC_SVC\",\"supabase_anon_key\":\"$ENC_ANON\",\"is_active\":true,\"migration_status\":\"completed\"}"
  [ "$REST_CODE" = "201" ] || { echo "  ✘ ERROR creando venue_database_configs (HTTP $REST_CODE):"; cat "$REST_BODY"; exit 1; }
  echo "  ✔ venue_database_configs creado (service/anon keys cifradas)"
fi

# ── 3. Migrar la pasarela Cybersource SANDBOX desde prod ─────────────
echo "== 3. Pasarela sandbox (payment_gateway_credentials prod → staging) =="

# Guard espejo EXACTO de services/payment_router.go:loadPaymentConfig
# (venue_id + is_active=true + is_primary=true): hay que validar la MISMA
# fila que el backend va a cargar, no una cualquiera (con >1 fila, d[0]
# sin filtrar podía ser otra).
rest "$STG_CENTRAL_URL" "$STG_CENTRAL_KEY" GET "payment_gateway_credentials?venue_id=eq.$VENUE_ID&select=id"
PGC_TOTAL=$(jcount)
[ "$PGC_TOTAL" -ge 0 ] 2>/dev/null || { echo "  ✘ no pude leer payment_gateway_credentials del central staging (HTTP $REST_CODE)"; exit 1; }
rest "$STG_CENTRAL_URL" "$STG_CENTRAL_KEY" GET "payment_gateway_credentials?venue_id=eq.$VENUE_ID&is_primary=eq.true&is_active=eq.true&select=id,environment"
PGC_N=$(jcount)
if [ "$PGC_N" = "1" ]; then
  PGC_ENV=$(jfield environment)
  echo "  ⚠ ya existe fila primaria activa de pasarela para el venue staging (environment=$PGC_ENV) — salto"
  if [ "$PGC_ENV" != "test" ] && [ "$PGC_ENV" != "sandbox" ]; then
    echo "  ✘ PELIGRO: esa fila NO está en test/sandbox. Staging jamás debe cobrar dinero real. Corrígela a mano."
    exit 1
  fi
elif [ "$PGC_N" != "0" ]; then
  echo "  ✘ hay $PGC_N filas is_primary=true/is_active=true de pasarela para el venue staging."
  echo "    Con más de una, el backend cargaría una al azar (limit 1 sin order) — imposible"
  echo "    garantizar sandbox. Deja EXACTAMENTE una fila primaria activa y re-ejecuta."
  exit 1
elif [ "$PGC_TOTAL" != "0" ]; then
  echo "  ✘ hay $PGC_TOTAL fila(s) de pasarela para el venue staging pero NINGUNA is_primary=true/is_active=true."
  echo "    El backend caería al default config (Stripe). Marca la fila sandbox como primaria"
  echo "    y activa (o bórralas todas para que este script la re-cree) y re-ejecuta."
  exit 1
else
  # Solo LECTURA contra la central de prod. Filtramos environment
  # in.(test,sandbox): tras el cutover a producción la fila PRIMARIA de
  # prod será la REAL, y copiarla a staging sería un desastre. A propósito
  # NO exigimos is_primary (la sandbox deja de ser primaria tras el
  # cutover) — solo sandbox + activa.
  PROD_ROW="$TMP_WORK/pgc_prod.json"
  code=$(curl -s -o "$PROD_ROW" -w '%{http_code}' \
    "$PROD_CENTRAL_URL/rest/v1/payment_gateway_credentials?venue_id=eq.$PROD_VENUE_ID&environment=in.(test,sandbox)&is_active=eq.true&limit=1" \
    -H "apikey: $PROD_CENTRAL_KEY" -H "Authorization: Bearer $PROD_CENTRAL_KEY")
  [ "$code" = "200" ] || { echo "  ✘ ERROR leyendo la fila de prod (HTTP $code)"; exit 1; }

  PROD_N=$("$PYBIN" - "$PROD_ROW" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print(-1); raise SystemExit
print(len(d) if isinstance(d, list) else -1)
PY
)
  if [ "$PROD_N" != "1" ]; then
    echo "  ✘ NO hay fila de pasarela SANDBOX (environment in test/sandbox, is_active=true) en la"
    echo "    central de PROD para el venue $PROD_VENUE_ID."
    echo "    Este script SOLO copia credenciales sandbox — jamás las de producción. Crea o"
    echo "    reactiva la fila sandbox en la central de prod (aunque ya no sea is_primary tras"
    echo "    el cutover) e inténtalo de nuevo, o inserta la fila de staging a mano con"
    echo "    environment=test."
    exit 1
  fi

  OLD_ENC=$("$PYBIN" - "$PROD_ROW" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print(d[0].get("secret_key_encrypted", "") if d else "")
PY
)
  [ -n "$OLD_ENC" ] || { echo "  ✘ la fila sandbox de prod no tiene secret_key_encrypted (¿venue id correcto?)"; exit 1; }

  echo "  → re-cifrando secret: APP_KEY prod → APP_KEY staging (cmd/recrypt; el secreto nunca se imprime)"
  NEW_ENC=$(printf '%s' "$OLD_ENC" | OLD_KEY="$PROD_APP_KEY" NEW_KEY="$STG_APP_KEY" go run ./cmd/recrypt)
  [ -n "$NEW_ENC" ] || { echo "  ✘ cmd/recrypt falló re-cifrando el secret de la pasarela"; exit 1; }

  # Construir el INSERT copiando los campos no-secretos de la fila prod,
  # forzando environment=test y el secret re-cifrado. Sin imprimir nada.
  PGC_PAYLOAD="$TMP_WORK/pgc_insert.json"
  SB_VENUE_ID="$VENUE_ID" SB_NEW_ENC="$NEW_ENC" "$PYBIN" - "$PROD_ROW" "$PGC_PAYLOAD" <<'PY'
import json, os, sys
rows = json.load(open(sys.argv[1]))
src = rows[0] if rows else {}
keep = ["gateway", "gateway_name", "priority",
        "platform_fee_percent", "platform_fee_fixed",
        "gateway_fee_percent", "gateway_fee_fixed", "default_currency",
        "profile_id", "access_key", "merchant_id", "terminal_id",
        "stripe_account_id", "stripe_publishable_key", "mercadopago_public_key"]
out = {k: src[k] for k in keep if src.get(k) not in (None, "")}
out["venue_id"] = os.environ["SB_VENUE_ID"]
out["secret_key_encrypted"] = os.environ["SB_NEW_ENC"]
out["environment"] = "test"     # staging es SIEMPRE sandbox
out["is_active"] = True
out["is_primary"] = True
json.dump(out, open(sys.argv[2], "w"))
PY
  rest "$STG_CENTRAL_URL" "$STG_CENTRAL_KEY" POST "payment_gateway_credentials" "@$PGC_PAYLOAD"
  [ "$REST_CODE" = "201" ] || { echo "  ✘ ERROR insertando pasarela staging (HTTP $REST_CODE):"; cat "$REST_BODY"; exit 1; }
  GW=$(jfield gateway); GW_ENV=$(jfield environment)
  echo "  ✔ pasarela migrada (gateway=$GW, environment=$GW_ENV)"
  [ "$GW_ENV" = "test" ] || { echo "  ✘ environment != test tras el insert — revisar a mano"; exit 1; }
fi

# ── 4. Seed del venue staging (PostgREST) ────────────────────────────
echo "== 4. Seed venue staging (admin + evento de prueba) =="

# Roles: venue_template.sql ya siembra los 6 por defecto — solo resolver id.
rest "$STG_VENUE_URL" "$STG_VENUE_KEY" GET "roles?name=eq.admin&select=id"
ADMIN_ROLE_ID=$(jfield id)
[ -n "$ADMIN_ROLE_ID" ] || { echo "  ✘ no existe el rol admin en el venue staging — ¿se aplicó sql/venue_template.sql?"; exit 1; }

ADMIN_PASS=""
rest "$STG_VENUE_URL" "$STG_VENUE_KEY" GET "organization_workers?email=eq.$ADMIN_EMAIL&select=id"
ADMIN_ID=$(jfield id)
if [ -n "$ADMIN_ID" ]; then
  if [ -n "$(env_get "$ENV_STG" STAFF_ADMIN_PASSWORD)" ]; then
    echo "  ⚠ admin $ADMIN_EMAIL ya existe ($ADMIN_ID) — conservo su password actual (ya está en $ENV_STG)"
  else
    # Re-run tras un fallo intermedio: el admin existe pero la password se
    # perdió (no llegó a persistirse). Regenerar y re-hashear para no dejar
    # smoke_staging.sh bloqueado.
    echo "  ⚠ admin $ADMIN_EMAIL ya existe ($ADMIN_ID) pero $ENV_STG no tiene STAFF_ADMIN_PASSWORD"
    echo "    → regenero password y actualizo su password_hash"
    ADMIN_PASS=$("$PYBIN" -c "import secrets;print(secrets.token_urlsafe(18))")
    ADMIN_HASH=$(printf '%s' "$ADMIN_PASS" | go run ./cmd/hashpwd)
    [ -n "$ADMIN_HASH" ] || { echo "  ✘ cmd/hashpwd no devolvió hash"; exit 1; }
    rest "$STG_VENUE_URL" "$STG_VENUE_KEY" PATCH "organization_workers?id=eq.$ADMIN_ID" \
      "{\"password_hash\":\"$ADMIN_HASH\"}"
    { [ "$REST_CODE" = "200" ] || [ "$REST_CODE" = "204" ]; } || { echo "  ✘ ERROR actualizando password_hash del admin (HTTP $REST_CODE):"; cat "$REST_BODY"; exit 1; }
    persist_admin_creds
    echo "  ✔ password del admin regenerada"
  fi
else
  ADMIN_PASS=$("$PYBIN" -c "import secrets;print(secrets.token_urlsafe(18))")
  ADMIN_HASH=$(printf '%s' "$ADMIN_PASS" | go run ./cmd/hashpwd)
  [ -n "$ADMIN_HASH" ] || { echo "  ✘ cmd/hashpwd no devolvió hash"; exit 1; }
  rest "$STG_VENUE_URL" "$STG_VENUE_KEY" POST "organization_workers" \
    "{\"email\":\"$ADMIN_EMAIL\",\"first_name\":\"Damian\",\"last_name\":\"Perez\",\"password_hash\":\"$ADMIN_HASH\",\"role_id\":\"$ADMIN_ROLE_ID\",\"is_active\":true}"
  [ "$REST_CODE" = "201" ] || { echo "  ✘ ERROR creando admin (HTTP $REST_CODE):"; cat "$REST_BODY"; exit 1; }
  ADMIN_ID=$(jfield id)
  # Persistir INMEDIATAMENTE: si algo posterior falla (flyctl, evento...)
  # la password no se pierde y el re-run no bloquea smoke_staging.sh.
  persist_admin_creds
  echo "  ✔ admin staff creado ($ADMIN_EMAIL, rol admin)"
fi

rest "$STG_VENUE_URL" "$STG_VENUE_KEY" GET "events?slug=eq.$EVENT_SLUG&select=id"
EVENT_ID=$(jfield id)
if [ -n "$EVENT_ID" ]; then
  echo "  ⚠ evento $EVENT_SLUG ya existe — reutilizo ($EVENT_ID)"
else
  EVENT_START=$("$PYBIN" -c "from datetime import datetime,timedelta;print((datetime.now()+timedelta(days=30)).strftime('%Y-%m-%dT21:00:00-06:00'))")
  EVENT_END=$("$PYBIN" -c "from datetime import datetime,timedelta;print((datetime.now()+timedelta(days=31)).strftime('%Y-%m-%dT03:00:00-06:00'))")
  EVENT_ID=$(gen_uuid)
  rest "$STG_VENUE_URL" "$STG_VENUE_KEY" POST "events" \
    "{\"id\":\"$EVENT_ID\",\"name\":\"$EVENT_NAME\",\"slug\":\"$EVENT_SLUG\",\"description\":\"Evento permanente de prueba del entorno staging. Compras con tarjeta 4111 1111 1111 1111 (sandbox).\",\"start_datetime\":\"$EVENT_START\",\"end_datetime\":\"$EVENT_END\",\"location\":\"511 STAGING\",\"address\":\"Ciudad de Guatemala\",\"capacity\":500,\"status\":\"published\",\"is_private\":false,\"min_age\":18,\"use_guest_list\":true}"
  [ "$REST_CODE" = "201" ] || { echo "  ✘ ERROR creando evento (HTTP $REST_CODE):"; cat "$REST_BODY"; exit 1; }
  echo "  ✔ evento creado: $EVENT_NAME ($EVENT_ID, published, $EVENT_START)"
fi

rest "$STG_VENUE_URL" "$STG_VENUE_KEY" GET "ticket_types?event_id=eq.$EVENT_ID&name=eq.General&select=id"
TICKET_ID=$(jfield id)
if [ -n "$TICKET_ID" ]; then
  echo "  ⚠ ticket General ya existe — reutilizo ($TICKET_ID)"
else
  TICKET_ID=$(gen_uuid)
  rest "$STG_VENUE_URL" "$STG_VENUE_KEY" POST "ticket_types" \
    "{\"id\":\"$TICKET_ID\",\"event_id\":\"$EVENT_ID\",\"name\":\"General\",\"description\":\"Entrada general de prueba (staging)\",\"price\":100,\"currency\":\"GTQ\",\"quantity_total\":100,\"max_per_order\":10,\"is_active\":true,\"is_visible\":true}"
  [ "$REST_CODE" = "201" ] || { echo "  ✘ ERROR creando ticket type (HTTP $REST_CODE):"; cat "$REST_BODY"; exit 1; }
  echo "  ✔ ticket type creado: General Q100 ($TICKET_ID)"
fi

# ── IDs locales — ANTES de Fly, para que un fallo de flyctl no los pierda ─
cat > "$IDS_FILE" <<EOF
# Generado por scripts/setup_staging.sh — lo leen smoke_staging.sh y otros.
VENUE_ID=$VENUE_ID
EVENT_ID=$EVENT_ID
TICKET_TYPE_ID=$TICKET_ID
ADMIN_EMAIL=$ADMIN_EMAIL
EOF
grep -qxF "$IDS_FILE" .gitignore 2>/dev/null || echo "$IDS_FILE" >> .gitignore
echo "IDs escritos en $IDS_FILE (gitignored)"

# ── 5. Secrets SUPABASE_* a Fly ──────────────────────────────────────
echo "== 5. Secrets SUPABASE_* → Fly ($FLY_APP, --stage) =="
command -v flyctl >/dev/null 2>&1 || export PATH="$PATH:$HOME/.fly/bin"
if command -v flyctl >/dev/null 2>&1; then
  flyctl secrets set -a "$FLY_APP" --stage \
    CENTRAL_SUPABASE_URL="$STG_CENTRAL_URL" \
    CENTRAL_SERVICE_KEY="$STG_CENTRAL_KEY" \
    CENTRAL_ANON_KEY="$STG_CENTRAL_ANON" \
    DEFAULT_SUPABASE_URL="$STG_VENUE_URL" \
    DEFAULT_SERVICE_KEY="$STG_VENUE_KEY" \
    DEFAULT_ANON_KEY="$STG_VENUE_ANON" \
    && echo "  ✔ 6 secrets staged (se aplican con el próximo deploy)" \
    || { echo "  ✘ flyctl secrets set falló — reintenta a mano con los valores de $ENV_STG"; exit 1; }
else
  echo "  ⚠ flyctl no está en PATH. Sube los 6 SUPABASE_* a mano (valores en $ENV_STG):"
  echo "    flyctl secrets set -a $FLY_APP --stage CENTRAL_SUPABASE_URL=... CENTRAL_SERVICE_KEY=... \\"
  echo "      CENTRAL_ANON_KEY=... DEFAULT_SUPABASE_URL=... DEFAULT_SERVICE_KEY=... DEFAULT_ANON_KEY=..."
fi

# ── Resumen ──────────────────────────────────────────────────────────
echo
echo "===== STAGING MONTADO ====="
echo "  venue:   $VENUE_NAME  id=$VENUE_ID  (slug $VENUE_SLUG)"
echo "  evento:  $EVENT_NAME  id=$EVENT_ID"
echo "  ticket:  General Q100  id=$TICKET_ID"
echo "  admin:   $ADMIN_EMAIL"
if [ -n "$ADMIN_PASS" ]; then
  echo "  password (SE MUESTRA SOLO ESTA VEZ — guárdala en tu gestor de contraseñas;"
  echo "            queda también en $ENV_STG como STAFF_ADMIN_PASSWORD):"
  echo "      $ADMIN_PASS"
else
  echo "  password: la existente (no se regeneró)"
fi
echo
echo "SIGUIENTES PASOS:"
echo "  1. bash scripts/deploy_staging.sh   (aplica los secrets staged y deploya)"
echo "  2. Cloudflare Pages (proyecto pull-511-events) → variables de PREVIEW:"
echo "       VITE_API_URL=/api/v1"
echo "       VITE_DEFAULT_VENUE_SLUG=$VENUE_SLUG"
echo "       (y PROXY_SHARED_SECRET / API upstream del proxy apuntando a"
echo "        https://pull-api-v2-staging.fly.dev — ver functions/api/[[path]].js)"
echo "  3. bash scripts/smoke_staging.sh"
