#!/usr/bin/env bash
# =============================================
# SMOKE TEST de STAGING — 511 Events
# Ejercita los flujos contra staging (Cybersource SIEMPRE sandbox).
# Uso:  bash scripts/smoke_staging.sh   (desde Pull-API-v2/)
# Necesita: .env.staging.local (secretos) y .staging.ids (IDs — los
# genera scripts/setup_staging.sh).
# Crea órdenes de prueba en las BDs de staging — no pasa nada.
# =============================================
set -u
cd "$(dirname "$0")/.."

# En máquinas solo-python3 un 'python' a pelo revienta con mensajes
# engañosos (p.ej. el guard sandbox abortaría con environment='').
PYBIN=$(command -v python || command -v python3 || true)
[ -n "$PYBIN" ] || { echo "ABORTADO: ni python ni python3 están en PATH"; exit 1; }

API="https://staging.pull-511-events.pages.dev/api/v1"
WEB="https://staging.pull-511-events.pages.dev"
FLY="https://pull-api-v2-staging.fly.dev"
ENV_FILE=".env.staging.local"
IDS_FILE=".staging.ids"
CARD='{"number":"4111111111111111","exp_month":"12","exp_year":"28","cvv":"123"}'

[ -f "$ENV_FILE" ] || { echo "ABORTADO: falta $ENV_FILE"; exit 1; }
[ -f "$IDS_FILE" ] || { echo "ABORTADO: falta $IDS_FILE — ejecuta antes: bash scripts/setup_staging.sh"; exit 1; }

idget()  { grep "^${1}=" "$IDS_FILE" | head -1 | cut -d= -f2- | tr -d '\r'; }
envget() { grep "^${1}=" "$ENV_FILE" | head -1 | cut -d= -f2- | tr -d '\r'; }

VENUE=$(idget VENUE_ID)
EVENT_PUB=$(idget EVENT_ID)             # Staging Test Night
TT_PUB=$(idget TICKET_TYPE_ID)          # General Q100
EMAIL=$(idget ADMIN_EMAIL)
EVENT_PRIV=$(idget EVENT_PRIV_ID)       # opcional: solo si se sembró un evento privado
TT_PRIV=$(idget TICKET_PRIV_ID)

PASS=$(envget STAFF_ADMIN_PASSWORD)
VKEY=$(envget DEFAULT_SERVICE_KEY)
VURL="$(envget DEFAULT_SUPABASE_URL)/rest/v1"
CKEY=$(envget CENTRAL_SERVICE_KEY)
CURL_CENTRAL="$(envget CENTRAL_SUPABASE_URL)/rest/v1"

for pair in "VENUE_ID:$VENUE" "EVENT_ID:$EVENT_PUB" "TICKET_TYPE_ID:$TT_PUB" "ADMIN_EMAIL:$EMAIL"; do
  [ -n "${pair#*:}" ] || { echo "ABORTADO: falta ${pair%%:*} en $IDS_FILE — re-ejecuta setup_staging.sh"; exit 1; }
done
[ -n "$PASS" ] || { echo "ABORTADO: falta STAFF_ADMIN_PASSWORD en $ENV_FILE (la genera setup_staging.sh)"; exit 1; }
[ -n "$VKEY" ] && [ -n "$CKEY" ] || { echo "ABORTADO: faltan SERVICE_KEYs en $ENV_FILE"; exit 1; }

# ── GUARD SANDBOX ────────────────────────────────────────────────────
# Staging debe estar SIEMPRE en environment=test. Si no lo está, alguien
# metió credenciales de producción donde no tocaba: aborta y NO pases
# tarjetas hasta corregirlo (cada intento sería una transacción real).
# Query espejo EXACTO de services/payment_router.go:loadPaymentConfig
# (venue_id + is_active=true + is_primary=true): validamos la MISMA fila
# que el backend carga. Debe haber EXACTAMENTE 1; con 0 o >1 abortamos.
GW_ROWS=$(curl -s "$CURL_CENTRAL/payment_gateway_credentials?select=environment&venue_id=eq.$VENUE&is_primary=eq.true&is_active=eq.true" \
  -H "apikey: $CKEY" -H "Authorization: Bearer $CKEY")
GW_N=$(printf '%s' "$GW_ROWS" | "$PYBIN" -c "import sys,json
try: d=json.load(sys.stdin)
except Exception: d=None
print(len(d) if isinstance(d,list) else -1)" 2>/dev/null)
if [ "$GW_N" != "1" ]; then
  echo "ABORTADO: esperaba EXACTAMENTE 1 fila is_primary=true/is_active=true en"
  echo "payment_gateway_credentials para el venue staging y hay '$GW_N'."
  echo "Con 0 el backend cae al default config; con >1 este guard podría validar una fila"
  echo "distinta de la que usa el backend. Corrige la central staging antes del smoke."
  exit 1
fi
GW_ENV=$(printf '%s' "$GW_ROWS" | "$PYBIN" -c "import sys,json;d=json.load(sys.stdin);print(d[0].get('environment','') if d else '')" 2>/dev/null)
if [ "$GW_ENV" != "test" ] && [ "$GW_ENV" != "sandbox" ]; then
  echo "ABORTADO: la pasarela del venue STAGING está en environment='$GW_ENV' (no test/sandbox)."
  echo "Esto NUNCA debería pasar en staging. Revisa payment_gateway_credentials en la"
  echo "central staging y corrígela a environment=test antes de correr este smoke."
  exit 1
fi
echo "(pasarela staging en environment=$GW_ENV — OK para smoke con tarjeta test)"

PASS_N=0; FAIL_N=0
ok()   { PASS_N=$((PASS_N+1)); echo "  ✔ $1"; }
fail() { FAIL_N=$((FAIL_N+1)); echo "  ✘ $1"; }
check() { # check <desc> <esperado> <obtenido>
  if [ "$2" = "$3" ]; then ok "$1"; else fail "$1 (esperaba $2, fue $3)"; fi
}
jget() { "$PYBIN" -c "import sys,json;
d=json.load(sys.stdin)
for k in '$1'.split('.'): d=d.get(k,{}) if isinstance(d,dict) else {}
print(d if not isinstance(d,dict) else '')" 2>/dev/null; }

# La máquina de staging se auto-apaga: despiértala antes de medir nada.
curl -s --max-time 60 "$FLY/health" >/dev/null 2>&1 || true

echo "== 1. Infra =="
check "health backend" "200" "$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 $FLY/health)"
check "web staging viva" "200" "$(curl -s -o /dev/null -w '%{http_code}' $WEB)"
check "CORS origin hostil bloqueado" "Origin not allowed" "$(curl -s -X POST $FLY/api/v1/orders/create-pending-order -H 'Origin: https://evil.example.com' -H 'Content-Type: application/json' -d '{}' | jget error)"

echo "== 2. Staff =="
TOKEN=$(curl -s -X POST "$API/auth/login-staff" -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget token)
[ -n "$TOKEN" ] && ok "login staff" || fail "login staff"
check "eventos" "200" "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$API/event/upcoming-events/$VENUE")"
PAG=$(curl -s -H "Authorization: Bearer $TOKEN" "$API/orders/venue/$VENUE?status=All&limit=5" | jget pagination.totalCount)
[ -n "$PAG" ] && ok "orders con count exacto ($PAG)" || fail "orders pagination"

echo "== 3. Compra PÚBLICA completa =="
R=$(curl -s -X POST "$API/orders/create-pending-order" -H 'Content-Type: application/json' -d "{
  \"event_id\":\"$EVENT_PUB\",\"ticket_type_id\":\"$TT_PUB\",\"quantity\":1,
  \"user_name\":\"Smoke Staging\",\"user_email\":\"$EMAIL\",
  \"tickets_data\":[{\"owner_name\":\"Smoke\",\"owner_last_name\":\"Staging\",\"owner_email\":\"$EMAIL\",\"owner_phone\":\"50200000000\",\"owner_gender\":\"male\",\"owner_birthdate\":\"1995-01-01\"}]}")
ORDER=$(echo "$R" | jget order_id); CODE=$(echo "$R" | jget payment_link_code)
[ -n "$ORDER" ] && ok "orden creada" || fail "crear orden"
check "anti-carding: pay SIN code → 403" "403" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/orders/pay" -H 'Content-Type: application/json' -d "{\"order_id\":\"$ORDER\",\"card\":$CARD}")"
check "pago con code → 200" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/orders/pay" -H 'Content-Type: application/json' -d "{\"order_id\":\"$ORDER\",\"payment_link_code\":\"$CODE\",\"card\":$CARD}")"
QR=$(curl -s "$VURL/tickets?select=qr_token&order_id=eq.$ORDER" -H "apikey: $VKEY" -H "Authorization: Bearer $VKEY" | "$PYBIN" -c "import sys,json;d=json.load(sys.stdin);print(d[0]['qr_token'] if d else '')")
[ -n "$QR" ] && ok "ticket emitido" || fail "ticket emitido"

echo "== 4. Escaneo en puerta =="
V1=$(curl -s -X POST "$API/ticket-validation/validate-ticket" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "{\"qr_token\":\"$QR\",\"venue_id\":\"$VENUE\"}" | jget valid)
check "primer escaneo válido" "True" "$V1"
V2=$(curl -s -X POST "$API/ticket-validation/validate-ticket" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "{\"qr_token\":\"$QR\",\"venue_id\":\"$VENUE\"}" | jget already_used)
check "re-escaneo duplicado" "True" "$V2"

if [ -n "$EVENT_PRIV" ] && [ -n "$TT_PRIV" ]; then
  echo "== 5. Flujo PRIVADO completo (hold→approve→captura) =="
  R=$(curl -s -X POST "$API/orders/create-pending-order" -H 'Content-Type: application/json' -d "{
    \"event_id\":\"$EVENT_PRIV\",\"ticket_type_id\":\"$TT_PRIV\",\"quantity\":1,
    \"user_name\":\"Smoke Priv\",\"user_email\":\"$EMAIL\",
    \"tickets_data\":[{\"owner_name\":\"Smoke\",\"owner_last_name\":\"Priv\",\"owner_email\":\"$EMAIL\",\"owner_phone\":\"50200000000\",\"owner_gender\":\"male\",\"owner_birthdate\":\"1995-01-01\"}]}")
  ORDERP=$(echo "$R" | jget order_id); CODEP=$(echo "$R" | jget payment_link_code)
  PA=$(curl -s -X POST "$API/orders/pay" -H 'Content-Type: application/json' -d "{\"order_id\":\"$ORDERP\",\"payment_link_code\":\"$CODEP\",\"card\":$CARD}" | jget pending_approval)
  check "pago retenido (pending_approval)" "True" "$PA"
  ST=$(curl -s "$VURL/orders?select=status&id=eq.$ORDERP" -H "apikey: $VKEY" -H "Authorization: Bearer $VKEY" | "$PYBIN" -c "import sys,json;print(json.load(sys.stdin)[0]['status'])")
  check "estado payment_authorized" "payment_authorized" "$ST"
  AP=$(curl -s -X POST "$API/orders/$ORDERP/approve" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{}' | jget success)
  check "aprobar → captura + ticket" "True" "$AP"
else
  echo "== 5. Flujo PRIVADO — SALTADO (no hay EVENT_PRIV_ID/TICKET_PRIV_ID en $IDS_FILE) =="
fi

echo "== 6. Login cliente + wallet =="
curl -s -o /dev/null -X POST "$API/user-auth/request-code" -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\"}"
# Resolver el user_id DESPUÉS del request-code (si el paso 3 falló puede no
# existir aún el public_user) y leer SU código (no el global más reciente —
# con actividad concurrente se colaría el código de otro usuario).
UID_TEST=$(curl -s "$VURL/public_users?select=id&email=eq.$EMAIL" -H "apikey: $VKEY" -H "Authorization: Bearer $VKEY" | "$PYBIN" -c "import sys,json;d=json.load(sys.stdin);print(d[0]['id'] if d else '')")
if [ -z "$UID_TEST" ]; then
  fail "no hay public_user para $EMAIL (¿falló el paso 3?) — paso 6 SALTADO"
else
  UCODE=$(curl -s "$VURL/verification_codes?select=code&used=eq.false&user_id=eq.$UID_TEST&order=created_at.desc&limit=1" -H "apikey: $VKEY" -H "Authorization: Bearer $VKEY" | "$PYBIN" -c "import sys,json;d=json.load(sys.stdin);print(d[0]['code'] if d else '')")
  CJ=$(mktemp)
  VS=$(curl -s -c "$CJ" -X POST "$API/user-auth/verify-code" -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"code\":\"$UCODE\"}" | jget success)
  check "verify-code + cookie" "True" "$VS"
  check "profile con cookie" "True" "$(curl -s -b "$CJ" "$API/user-auth/profile" | jget success)"
  NT=$(curl -s -b "$CJ" "$API/tickets/my-tickets" | "$PYBIN" -c "import sys,json;print(len(json.load(sys.stdin).get('tickets',[])))")
  [ "$NT" -ge 1 ] 2>/dev/null && ok "wallet con $NT tickets" || fail "wallet tickets ($NT)"
  SP=$(curl -s -b "$CJ" "$API/users/spending/venues" | "$PYBIN" -c "import sys,json;s=json.load(sys.stdin).get('spending',[]);print(s[0]['total_spent'] if s else '')")
  [ -n "$SP" ] && ok "spending ($SP)" || fail "spending"
  rm -f "$CJ"
fi

echo
echo "===== RESULTADO: $PASS_N OK / $FAIL_N FALLOS ====="
[ "$FAIL_N" -eq 0 ] && echo "TODO VERDE" || echo "REVISAR FALLOS ARRIBA"
exit $FAIL_N
