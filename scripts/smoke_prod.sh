#!/usr/bin/env bash
# =============================================
# SMOKE TEST de PRODUCCIÓN — 511 Events
# Ejercita TODOS los flujos contra prod (sandbox de pagos).
# Uso:  bash scripts/smoke_prod.sh   (desde Pull-API-v2/, necesita .env.prod.local)
# Crea órdenes de prueba — límpialas antes del evento real.
# =============================================
set -u
cd "$(dirname "$0")/.."
[ -f .env.prod.local ] || { echo "ABORTADO: no encuentro .env.prod.local (¿repo equivocado?)"; exit 1; }
command -v python >/dev/null 2>&1 || { echo "ABORTADO: falta python en PATH"; exit 1; }
API="https://pull-511-events.pages.dev/api/v1"
FLY="https://pull-api-v2-prod.fly.dev"
VENUE="5d3a4758-fabb-46a2-9dc3-86b2ee8bcafa"
EVENT_PUB="9c53689a-0df4-4fcd-8cfa-44f8368b5580"        # 511 Test Night
TT_PUB="7a039563-3c08-495a-8d70-402289a3872b"           # General Q100
EVENT_PRIV="6633ffa8-4cf8-4594-8a90-76a2f5d2ac60"       # Evento privado 511 test
TT_PRIV="ec51881b-0c1c-4eb2-a242-1ffc63dfcfea"          # General Q1000
CARD='{"number":"4111111111111111","exp_month":"12","exp_year":"28","cvv":"123"}'
EMAIL="damian.perez@greenlock.tech"

VKEY=$(grep "^DEFAULT_SERVICE_KEY=" .env.prod.local | cut -d= -f2-)
PASS=$(grep "^STAFF_ADMIN_PASSWORD=" .env.prod.local | cut -d= -f2-)
VURL="https://faioqaaaonucbnxpmpxx.supabase.co/rest/v1"
CKEY=$(grep "^CENTRAL_SERVICE_KEY=" .env.prod.local | cut -d= -f2-)
[ -n "$CKEY" ] || { echo "ABORTADO: CENTRAL_SERVICE_KEY no encontrada en .env.prod.local"; exit 1; }
CURL_CENTRAL="https://mwuppgpmlynfxyghkpzv.supabase.co/rest/v1"

# ── GUARD POST-CUTOVER ───────────────────────────────────────────────
# Este script pasa una tarjeta por /orders/pay. Con la pasarela del venue
# en environment=production eso sería un COBRO REAL. La query espeja
# EXACTAMENTE el filtro del backend (payment_router.go: is_active=true AND
# is_primary=true) y exige UNA sola fila — cualquier ambigüedad o fallo de
# lectura aborta (fail-closed).
# ⚠️ TOCTOU: el backend cachea la config de pasarela 5 MINUTOS. Tras cambiar
# la fila (cutover o rollback), espera >5 min o haz
#   flyctl apps restart pull-api-v2-prod
# antes de fiarte de este guard.
GW_ENV=$(curl -s "$CURL_CENTRAL/payment_gateway_credentials?select=environment&venue_id=eq.$VENUE&is_active=eq.true&is_primary=eq.true" \
  -H "apikey: $CKEY" -H "Authorization: Bearer $CKEY" \
  | python -c "import sys,json
try:
    d=json.load(sys.stdin)
except Exception:
    print('READ_ERROR'); raise SystemExit
if not isinstance(d,list) or len(d)==0: print('READ_ERROR')
elif len(d)>1: print('AMBIGUOUS')
else: print(d[0].get('environment','READ_ERROR'))" 2>/dev/null)
[ -n "$GW_ENV" ] || GW_ENV="READ_ERROR"
case "$GW_ENV" in
  test|sandbox)
    echo "(pasarela venue en environment=$GW_ENV — OK para smoke con tarjeta test)";;
  AMBIGUOUS)
    echo "ABORTADO: hay MÁS de una fila activa+primaria en payment_gateway_credentials"
    echo "para el venue — estado ambiguo; arregla las filas antes de correr el smoke."
    exit 1;;
  READ_ERROR)
    echo "ABORTADO: no pude leer la fila de la pasarela (red, clave o respuesta"
    echo "inesperada) — fail-closed: sin confirmación de sandbox no se pasa tarjeta."
    exit 1;;
  *)
    echo "ABORTADO: la pasarela del venue está en environment='$GW_ENV' (no test/sandbox)."
    echo "Tras el cutover a Cybersource PRODUCCIÓN este smoke ya no debe correr:"
    echo "usa scripts/smoke_staging.sh para pagos, y verifica prod con los pasos"
    echo "no-monetarios (health, login, listados) a mano."
    exit 1;;
esac

PASS_N=0; FAIL_N=0
ok()   { PASS_N=$((PASS_N+1)); echo "  ✔ $1"; }
fail() { FAIL_N=$((FAIL_N+1)); echo "  ✘ $1"; }
check() { # check <desc> <esperado> <obtenido>
  if [ "$2" = "$3" ]; then ok "$1"; else fail "$1 (esperaba $2, fue $3)"; fi
}
jget() { python -c "import sys,json;
d=json.load(sys.stdin)
for k in '$1'.split('.'): d=d.get(k,{}) if isinstance(d,dict) else {}
print(d if not isinstance(d,dict) else '')" 2>/dev/null; }

echo "== 1. Infra =="
check "health backend" "200" "$(curl -s -o /dev/null -w '%{http_code}' $FLY/health)"
check "web viva"       "200" "$(curl -s -o /dev/null -w '%{http_code}' https://pull-511-events.pages.dev)"
check "CORS origin hostil bloqueado" "Origin not allowed" "$(curl -s -X POST $FLY/api/v1/orders/create-pending-order -H 'Origin: https://evil.example.com' -H 'Content-Type: application/json' -d '{}' | jget error)"

echo "== 2. Staff =="
LOGIN_BODY=$(E="$EMAIL" P="$PASS" python -c "import json,os;print(json.dumps({'email':os.environ['E'],'password':os.environ['P']}))")
TOKEN=$(printf '%s' "$LOGIN_BODY" | curl -s -X POST "$API/auth/login-staff" -H 'Content-Type: application/json' -d @- | jget token)
[ -n "$TOKEN" ] && ok "login staff" || fail "login staff"
check "eventos" "200" "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$API/event/upcoming-events/$VENUE")"
PAG=$(curl -s -H "Authorization: Bearer $TOKEN" "$API/orders/venue/$VENUE?status=All&limit=5" | jget pagination.totalCount)
[ -n "$PAG" ] && ok "orders con count exacto ($PAG)" || fail "orders pagination"

echo "== 3. Compra PÚBLICA completa =="
R=$(curl -s -X POST "$API/orders/create-pending-order" -H 'Content-Type: application/json' -d "{
  \"event_id\":\"$EVENT_PUB\",\"ticket_type_id\":\"$TT_PUB\",\"quantity\":1,
  \"user_name\":\"Smoke Test\",\"user_email\":\"$EMAIL\",
  \"tickets_data\":[{\"owner_name\":\"Smoke\",\"owner_last_name\":\"Test\",\"owner_email\":\"$EMAIL\",\"owner_phone\":\"50200000000\",\"owner_gender\":\"male\",\"owner_birthdate\":\"1995-01-01\"}]}")
ORDER=$(echo "$R" | jget order_id); CODE=$(echo "$R" | jget payment_link_code)
[ -n "$ORDER" ] && ok "orden creada" || fail "crear orden"
check "anti-carding: pay SIN code → 403" "403" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/orders/pay" -H 'Content-Type: application/json' -d "{\"order_id\":\"$ORDER\",\"card\":$CARD}")"
check "pago con code → 200" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/orders/pay" -H 'Content-Type: application/json' -d "{\"order_id\":\"$ORDER\",\"payment_link_code\":\"$CODE\",\"card\":$CARD}")"
QR=$(curl -s "$VURL/tickets?select=qr_token&order_id=eq.$ORDER" -H "apikey: $VKEY" -H "Authorization: Bearer $VKEY" | python -c "import sys,json;d=json.load(sys.stdin);print(d[0]['qr_token'] if d else '')")
[ -n "$QR" ] && ok "ticket emitido" || fail "ticket emitido"

echo "== 4. Escaneo en puerta =="
V1=$(curl -s -X POST "$API/ticket-validation/validate-ticket" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "{\"qr_token\":\"$QR\",\"venue_id\":\"$VENUE\"}" | jget valid)
check "primer escaneo válido" "True" "$V1"
V2=$(curl -s -X POST "$API/ticket-validation/validate-ticket" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "{\"qr_token\":\"$QR\",\"venue_id\":\"$VENUE\"}" | jget already_used)
check "re-escaneo duplicado" "True" "$V2"

echo "== 5. Flujo PRIVADO completo (hold→approve→captura) =="
R=$(curl -s -X POST "$API/orders/create-pending-order" -H 'Content-Type: application/json' -d "{
  \"event_id\":\"$EVENT_PRIV\",\"ticket_type_id\":\"$TT_PRIV\",\"quantity\":1,
  \"user_name\":\"Smoke Priv\",\"user_email\":\"$EMAIL\",
  \"tickets_data\":[{\"owner_name\":\"Smoke\",\"owner_last_name\":\"Priv\",\"owner_email\":\"$EMAIL\",\"owner_phone\":\"50200000000\",\"owner_gender\":\"male\",\"owner_birthdate\":\"1995-01-01\"}]}")
ORDERP=$(echo "$R" | jget order_id); CODEP=$(echo "$R" | jget payment_link_code)
PA=$(curl -s -X POST "$API/orders/pay" -H 'Content-Type: application/json' -d "{\"order_id\":\"$ORDERP\",\"payment_link_code\":\"$CODEP\",\"card\":$CARD}" | jget pending_approval)
check "pago retenido (pending_approval)" "True" "$PA"
ST=$(curl -s "$VURL/orders?select=status&id=eq.$ORDERP" -H "apikey: $VKEY" -H "Authorization: Bearer $VKEY" | python -c "import sys,json;print(json.load(sys.stdin)[0]['status'])")
check "estado payment_authorized" "payment_authorized" "$ST"
AP=$(curl -s -X POST "$API/orders/$ORDERP/approve" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{}' | jget success)
check "aprobar → captura + ticket" "True" "$AP"

echo "== 6. Login cliente + wallet =="
# Resolver el user_id del email de prueba y leer SU código (no el global más
# reciente — con actividad concurrente se colaba el código de otro usuario).
UID_TEST=$(curl -s "$VURL/public_users?select=id&email=eq.$EMAIL" -H "apikey: $VKEY" -H "Authorization: Bearer $VKEY" | python -c "import sys,json;d=json.load(sys.stdin);print(d[0]['id'] if d else '')")
curl -s -o /dev/null -X POST "$API/user-auth/request-code" -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\"}"
UCODE=$(curl -s "$VURL/verification_codes?select=code&used=eq.false&user_id=eq.$UID_TEST&order=created_at.desc&limit=1" -H "apikey: $VKEY" -H "Authorization: Bearer $VKEY" | python -c "import sys,json;d=json.load(sys.stdin);print(d[0]['code'] if d else '')")
CJ=$(mktemp)
VS=$(curl -s -c "$CJ" -X POST "$API/user-auth/verify-code" -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"code\":\"$UCODE\"}" | jget success)
check "verify-code + cookie" "True" "$VS"
check "profile con cookie" "True" "$(curl -s -b "$CJ" "$API/user-auth/profile" | jget success)"
NT=$(curl -s -b "$CJ" "$API/tickets/my-tickets" | python -c "import sys,json;print(len(json.load(sys.stdin).get('tickets',[])))")
[ "$NT" -ge 1 ] 2>/dev/null && ok "wallet con $NT tickets" || fail "wallet tickets ($NT)"
SP=$(curl -s -b "$CJ" "$API/users/spending/venues" | python -c "import sys,json;s=json.load(sys.stdin).get('spending',[]);print(s[0]['total_spent'] if s else '')")
[ -n "$SP" ] && ok "spending ($SP)" || fail "spending"
rm -f "$CJ"

echo
echo "===== RESULTADO: $PASS_N OK / $FAIL_N FALLOS ====="
[ "$FAIL_N" -eq 0 ] && echo "TODO VERDE" || echo "REVISAR FALLOS ARRIBA"
exit $FAIL_N
