#!/usr/bin/env bash
# =============================================================================
# DIAGNÓSTICO dLOCAL — ¿por qué no cobra con tarjeta?
#
# Para qué sirve: comprobar, SIN tocar la web ni la app, si el problema está en
# nuestro código o en la cuenta de dLocal. Te deja pruebas que puedes copiar y
# pegarle al soporte.
#
# Cómo se usa:
#     bash scripts/diag_dlocal.sh
#     bash scripts/diag_dlocal.sh CV-xxxxxxxx-xxxx-...   # con un token real
#
# El token real (CV-...) se saca así, en 30 segundos:
#   1. Abre https://511events.pullevents.com y empieza una compra.
#   2. F12 → pestaña Network.
#   3. Rellena la tarjeta y pulsa Pagar.
#   4. Busca la petición "temporal" (a ppmcc.dlocal.com/cvault/...).
#   5. Pestaña Response: sale {"token":"CV-..."}. Ese es.
#
# NO mueve dinero: solo crea pagos PENDING e intenta confirmarlos.
# =============================================================================
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

for f in .env.dlocal.local .env.prod.local; do
  [ -f "$f" ] && { set -a; . "./$f"; set +a; }
done

: "${DLOCAL_API_KEY:?falta DLOCAL_API_KEY (mira .env.dlocal.local)}"
: "${DLOCAL_SECRET_KEY:?falta DLOCAL_SECRET_KEY}"
AUTH="Authorization: Bearer ${DLOCAL_API_KEY}:${DLOCAL_SECRET_KEY}"
API="https://api.dlocalgo.com"
CARD="${1:-}"

linea() { printf '%s\n' "------------------------------------------------------------"; }

echo "== 1. ¿Quién soy para dLocal? =="
curl -s "$API/v1/me" -H "$AUTH" | python -m json.tool 2>/dev/null || echo "   (sin respuesta)"
echo "   merchant_id debe coincidir con el de tu panel."
linea

echo "== 2. Crear un pago transparente (esto NO cobra) =="
PAY=$(curl -s -X POST "$API/v1/payments" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "amount": 10.80, "currency": "GTQ", "country": "GT",
  "description": "diagnostico", "allow_transparent": true,
  "success_url": "https://511events.pullevents.com/es"
}')
echo "$PAY" | python -m json.tool 2>/dev/null | head -20
CT=$(echo "$PAY" | python -c "import sys,json;print(json.load(sys.stdin).get('merchant_checkout_token',''))" 2>/dev/null)
echo "   checkout_token: ${CT:-(NINGUNO)}"
[ -z "$CT" ] && { echo "   Sin token no se puede seguir."; exit 1; }
linea

if [ -z "$CARD" ]; then
  echo "== 3. Sin token de tarjeta: solo se puede probar el rechazo =="
  echo "   Vuelve a lanzarlo con un CV-... real para la prueba de verdad."
  curl -s -X POST "$API/v1/payments/confirm/$CT" -H "$AUTH" -H 'Content-Type: application/json' \
    -d '{"cardToken":"NO_EXISTE","clientFirstName":"A","clientLastName":"B","clientEmail":"a@b.com"}'
  echo ""
  exit 0
fi

echo "== 3. Confirmar con el token REAL de una tarjeta =="
echo "   token: ${CARD:0:12}..."
for PAIS in "GT GTQ 10.80" "UY UYU 500" "AR ARS 2500" "MX MXN 200"; do
  set -- $PAIS
  T=$(curl -s -X POST "$API/v1/payments" -H "$AUTH" -H 'Content-Type: application/json' \
      -d "{\"amount\":$3,\"currency\":\"$2\",\"country\":\"$1\",\"description\":\"diag\",\"allow_transparent\":true,\"success_url\":\"https://511events.pullevents.com/es\"}" \
      | python -c "import sys,json;print(json.load(sys.stdin).get('merchant_checkout_token',''))" 2>/dev/null)
  [ -z "$T" ] && { printf "   %-3s -> no se pudo crear el pago\n" "$1"; continue; }
  R=$(curl -s -X POST "$API/v1/payments/confirm/$T" -H "$AUTH" -H 'Content-Type: application/json' \
      -d "{\"cardToken\":\"$CARD\",\"clientFirstName\":\"Damian\",\"clientLastName\":\"Perez\",\"clientEmail\":\"dpmcyber@pm.me\",\"clientDocumentType\":\"CI\",\"clientDocument\":\"1234567890101\"}")
  printf "   %-3s -> %s\n" "$1" "$(echo "$R" | head -c 130)"
done
linea

cat <<'FIN'
CÓMO LEER EL RESULTADO

  "Missing payment method" en TODOS los países
      -> La cuenta no tiene habilitado el cobro con tarjeta. No es el código:
         el token es válido y el pago existe. Hay que hablar con dLocal.

  Errores DISTINTOS según el país
      -> Es cobertura por país. La cuenta sí hace tarjeta, pero no en GT.

  "invalid card" / "rejected" / algo del banco
      -> ¡La integración funciona! Ese error ya es del cobro en sí.

QUÉ PEDIRLE A dLOCAL (que sea una PERSONA, no el bot del chat)

  1. ¿Está habilitado el cobro con TARJETA para el merchant de la salida 1?
  2. La SmartFields API Key que tenemos, ¿pertenece a ESE MISMO merchant?
     (Si es de otra cuenta, el token se guarda en una bóveda y el cobro se
     intenta en otra: eso da exactamente "Missing payment method".)
  3. ¿Está la cuenta en modo producción con tarjetas activas, o solo efectivo?
FIN
