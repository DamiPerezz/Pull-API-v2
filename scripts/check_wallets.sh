#!/usr/bin/env bash
# =============================================================================
# ¿QUÉ MÉTODOS DE PAGO ESTÁ DEVOLVIENDO CYBERSOURCE?
#
#   bash scripts/check_wallets.sh              → producción
#   bash scripts/check_wallets.sh staging      → staging
#
# PARA QUÉ SIRVE:
# Nosotros PEDIMOS unos métodos (UNIFIED_CHECKOUT_PAYMENT_TYPES) pero
# Cybersource devuelve los que de verdad tiene habilitados para el comercio, y
# QUITA EN SILENCIO los que no. Ahí se ve, por ejemplo, que Apple Pay no está
# registrado: lo pedimos y no vuelve. Sin esto solo se puede comprobar con un
# iPhone en la mano, mirando si sale el botón.
#
# QUÉ HACE: crea una orden pendiente de prueba (NO paga nada), abre una sesión
# de Unified Checkout y descifra el JWT que devuelve la pasarela.
#
# La orden de prueba usa siempre el MISMO correo, así que cada ejecución expira
# la anterior y no se acumulan reservas de aforo.
# =============================================================================
set -u
cd "$(dirname "$0")/.."

ENTORNO="${1:-prod}"
case "$ENTORNO" in
  prod)
    API="https://511events.pullevents.com/api/v1"
    ORIGIN="https://511events.pullevents.com"
    EVENTO="902fd5b4-e996-4c8a-ab27-dfc93f6ab5c4"   # Evento publico pay (Q3)
    TICKET="2a499a67-9102-4e4a-8755-d40ec059abec"   # Geberal
    ;;
  staging)
    API="https://pull-511-staging.pages.dev/api/v1"
    ORIGIN="https://pull-511-staging.pages.dev"
    EVENTO="${CHECK_EVENT_ID:-}"
    TICKET="${CHECK_TICKET_TYPE_ID:-}"
    if [ -z "$EVENTO" ] || [ -z "$TICKET" ]; then
      echo "En staging hace falta indicar el evento:"
      echo "  CHECK_EVENT_ID=... CHECK_TICKET_TYPE_ID=... bash scripts/check_wallets.sh staging"
      exit 1
    fi
    ;;
  *)
    echo "Entorno desconocido: $ENTORNO (usa 'prod' o 'staging')"; exit 1 ;;
esac

command -v python >/dev/null 2>&1 || { echo "Falta python en el PATH"; exit 1; }

CORREO="check-wallets@pullevents.com"

echo "== Comprobando métodos de pago en $ENTORNO =="
echo

# ---- 1. Orden de prueba (pendiente, sin pagar) ------------------------------
RESP=$(curl -s -X POST "$API/orders/create-pending-order" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -d "{
  \"event_id\":\"$EVENTO\",\"ticket_type_id\":\"$TICKET\",\"quantity\":1,
  \"user_name\":\"Check Wallets\",\"user_email\":\"$CORREO\",
  \"tickets_data\":[{\"owner_name\":\"Check\",\"owner_last_name\":\"Wallets\",
  \"owner_email\":\"$CORREO\",\"owner_phone\":\"55512345\",
  \"owner_phone_prefix\":\"+502\",\"owner_gender\":\"male\",
  \"owner_birthdate\":\"1995-01-01\"}]}")

ORDEN=$(printf '%s' "$RESP" | python -c "import sys,json;print(json.load(sys.stdin).get('order_id',''))" 2>/dev/null)
CODIGO=$(printf '%s' "$RESP" | python -c "import sys,json;print(json.load(sys.stdin).get('payment_link_code',''))" 2>/dev/null)

if [ -z "$ORDEN" ]; then
  echo "NO se pudo crear la orden de prueba. Respuesta de la API:"
  printf '%s\n' "$RESP" | head -c 500; echo
  exit 1
fi

# ---- 2. Sesión de Unified Checkout ------------------------------------------
SESION=$(curl -s -X POST "$API/payments/capture-context" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
  -d "{\"order_id\":\"$ORDEN\",\"payment_link_code\":\"$CODIGO\"}")

printf '%s' "$SESION" | python -c '
import sys, json, base64

try:
    d = json.load(sys.stdin)
except Exception:
    print("  La pasarela no devolvió JSON. ¿Está UNIFIED_CHECKOUT_ENABLED en true?")
    raise SystemExit(1)

jwt = d.get("capture_context", "")
if not jwt:
    print("  NO se abrió la sesión. Lo que contestó el backend:")
    print("   ", json.dumps(d, ensure_ascii=False)[:400])
    raise SystemExit(1)

trozo = jwt.split(".")[1]
trozo += "=" * (-len(trozo) % 4)
ctx = json.loads(base64.urlsafe_b64decode(trozo))["ctx"][0]["data"]

metodos = [m if isinstance(m, str) else m.get("type", "?")
           for m in ctx.get("allowedPaymentTypes", [])]

print("  Versión del SDK :", ctx.get("clientVersion"))
print("  Mandato         :", (ctx.get("completeMandate") or {}).get("type"),
      "(CAPTURE = cobra ya / AUTH = retiene)")

huella = ((ctx.get("captureMandate") or {}).get("deviceFingerprinting") or {}).get("TM")
print("  Huella de disp. :", "sí, la hace el widget" if huella else "NO se está haciendo")
print()
print("  MÉTODOS QUE DEVUELVE LA PASARELA:")
for m in metodos:
    print("    -", m)
print()

for nombre, etiqueta in (("APPLEPAY", "Apple Pay"), ("GOOGLEPAY", "Google Pay"),
                         ("PANENTRY", "Tarjeta")):
    print(f"    {etiqueta:<12} {'DISPONIBLE' if nombre in metodos else 'NO aparece'}")

if "APPLEPAY" not in metodos:
    print()
    print("  Apple Pay no vuelve. Casi siempre significa que el dominio EXACTO no")
    print("  está registrado: Business Center > Payment Configuration >")
    print("  Unified Checkout > Apple Pay > Manage. Los subdominios NO heredan del")
    print("  dominio padre — 511events.pullevents.com hay que darlo de alta aparte.")
'

echo
echo "(orden de prueba: $ORDEN — queda pendiente y expira sola)"
