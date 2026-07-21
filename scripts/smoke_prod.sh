#!/usr/bin/env bash
# =============================================
# SMOKE TEST de PRODUCCIÓN — 511 Events
# Ejercita TODOS los flujos contra prod (sandbox de pagos).
# Uso:  bash scripts/smoke_prod.sh   (desde Pull-API-v2/, necesita .env.prod.local)
# Crea órdenes de prueba — límpialas antes del evento real.
# =============================================
set -u
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
TOKEN=$(curl -s -X POST "$API/auth/login-staff" -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget token)
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
