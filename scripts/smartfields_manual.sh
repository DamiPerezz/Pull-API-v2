#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# SmartFields de dLocal Go — LAS PETICIONES, A MANO
#
# Reproduce el "Transparent Checkout" de docs.dlocalgo.com paso a paso, sin
# pasar por nuestro backend. Sirve para separar "falla dLocal" de "fallamos
# nosotros": si esto falla, el problema NO es nuestro código.
#
#   Uso:  bash scripts/smartfields_manual.sh            # paso 1 (crear cobro)
#         bash scripts/smartfields_manual.sh <token> <cardToken>   # paso 4
#
# Las claves salen de .env.dlocal.local — NUNCA se imprimen.
# ---------------------------------------------------------------------------
set -uo pipefail
cd "$(dirname "$0")/.."

if [ -f .env.dlocal.local ]; then set -a; . ./.env.dlocal.local; set +a; fi
: "${DLOCAL_API_KEY:?falta DLOCAL_API_KEY}"
: "${DLOCAL_SECRET_KEY:?falta DLOCAL_SECRET_KEY}"

API="https://api.dlocalgo.com"
AUTH="Authorization: Bearer ${DLOCAL_API_KEY}:${DLOCAL_SECRET_KEY}"
JSON="Content-Type: application/json"

pretty() { python -m json.tool 2>/dev/null || cat; }

# ---------------------------------------------------------------------------
# PASO 4 — confirmar el cobro con el token de la tarjeta
# ---------------------------------------------------------------------------
if [ $# -eq 2 ]; then
  CHECKOUT_TOKEN="$1"; CARD_TOKEN="$2"
  echo "== PASO 4: POST /v1/payments/confirm/${CHECKOUT_TOKEN}"
  echo "   (el cuerpo va en camelCase: 'cardToken', no 'card_token')"
  echo
  curl -sS -X POST "${API}/v1/payments/confirm/${CHECKOUT_TOKEN}" \
    -H "$AUTH" -H "$JSON" \
    -d "{
          \"cardToken\": \"${CARD_TOKEN}\",
          \"clientFirstName\": \"Damian\",
          \"clientLastName\": \"Perez\",
          \"clientEmail\": \"damian@511events.com\"
        }" | pretty
  exit 0
fi

# ---------------------------------------------------------------------------
# PASO 1 — crear el cobro con allow_transparent
# ---------------------------------------------------------------------------
echo "== PASO 1: POST /v1/payments  (allow_transparent: true)"
echo
RESP=$(curl -sS -X POST "${API}/v1/payments" -H "$AUTH" -H "$JSON" -d '{
  "amount": 5,
  "currency": "GTQ",
  "country": "GT",
  "order_id": "PRUEBA-MANUAL-001",
  "description": "Prueba SmartFields",
  "allow_transparent": true,
  "notification_url": "https://api.511events.com/api/v1/webhooks/dlocal",
  "success_url": "https://511events.pullevents.com/pago/ok",
  "back_url": "https://511events.pullevents.com/pago"
}')
echo "$RESP" | pretty

TOKEN=$(echo "$RESP" | python -c "import sys,json;print(json.load(sys.stdin).get('merchant_checkout_token',''))" 2>/dev/null)
[ -z "$TOKEN" ] && { echo; echo "!! sin merchant_checkout_token — para aqui"; exit 1; }

# ---------------------------------------------------------------------------
# EXTRA — qué métodos de pago tiene ESE cobro
# (endpoint NO documentado, sacado del JS de su checkout; no pide auth)
# ---------------------------------------------------------------------------
echo
echo "== EXTRA: GET /v1/checkout/${TOKEN}   -> metodos disponibles"
curl -sS "${API}/v1/checkout/${TOKEN}" | python -c "
import sys,json,collections
d=json.load(sys.stdin); ms=d.get('paymentMethods') or []
c=collections.Counter(m.get('type','?') for m in ms)
print('   %d metodos: %s' % (len(ms), dict(c)))
for m in ms: print('     - %-6s %-28s %s' % (m.get('id'), m.get('name'), m.get('type')))
" 2>/dev/null

cat <<EOF

-------------------------------------------------------------------
  merchant_checkout_token = ${TOKEN}
-------------------------------------------------------------------

PASOS 2 y 3 — en el NAVEGADOR (la tarjeta la lee un iframe de dLocal,
no se puede hacer con curl). Abre:

  scripts/smartfields_manual.html?token=${TOKEN}

Te dara un cardToken. Luego vuelve aqui:

  bash scripts/smartfields_manual.sh ${TOKEN} <cardToken>

EOF
