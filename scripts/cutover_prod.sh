#!/usr/bin/env bash
# =============================================
# CUTOVER A PRODUCCIÓN — Cybersource/NeoNet (511 Events)
# Cambia la fila de credenciales del venue de SANDBOX a PRODUCCIÓN.
#
# NOTA (modelo B — 2 cuentas Cybersource): el script canónico es
# scripts/cutover_modelo_b.sh (entrada → cuenta del venue, fee 8% →
# cuenta de Pull vía PLATFORM_FEE_VENUE_ID). Este script equivale a su
# modo `cliente` (solo la fila del venue, sin tocar la fila Pull
# Platform); para la prueba real E2E o el rollback usa el nuevo.
#
# ⚠️ SOLO ejecutar cuando: (1) tengas las credenciales REST de PRODUCCIÓN
# generadas en el EBC2 (businesscenter.cybersource.com → Payment
# Configuration → Key Management → Generate Key → REST), y (2) vayas a hacer
# inmediatamente un cobro real de prueba. Esto pone el sistema a cobrar
# DINERO REAL.
#
# Uso (desde Pull-API-v2/, con .env.prod.local presente):
#   MERCHANT_ID="<merchant prod>" \
#   ACCESS_KEY="<Key ID prod (REST)>" \
#   SHARED_SECRET="<Shared Secret prod (REST)>" \
#   bash scripts/cutover_prod.sh
# =============================================
set -euo pipefail

: "${MERCHANT_ID:?Falta MERCHANT_ID (merchant de producción)}"
: "${ACCESS_KEY:?Falta ACCESS_KEY (Key ID REST de producción)}"
: "${SHARED_SECRET:?Falta SHARED_SECRET (Shared Secret REST de producción)}"

ENV_FILE=".env.prod.local"
APP_KEY=$(grep "^APP_KEY=" "$ENV_FILE" | cut -d= -f2-)
CKEY=$(grep "^CENTRAL_SERVICE_KEY=" "$ENV_FILE" | cut -d= -f2-)
CENTRAL="https://mwuppgpmlynfxyghkpzv.supabase.co"
ROW_ID="83ad16b1-22db-43c5-8da9-2e61038300f9"   # fila payment_gateway_credentials del venue 511

echo "== 1. Cifrando el Shared Secret con la APP_KEY (AES-256-GCM, formato del backend) =="
ENC=$(APP_KEY="$APP_KEY" SECRET="$SHARED_SECRET" python - <<'PY'
import os, base64
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
key = bytes.fromhex(os.environ["APP_KEY"])
nonce = os.urandom(12)  # gcm.NonceSize()==12, igual que cmd/encrypt
ct = AESGCM(key).encrypt(nonce, os.environ["SECRET"].encode(), None)
print(base64.b64encode(nonce + ct).decode())  # base64(nonce||ciphertext) = formato Go
PY
)
echo "  secret cifrado: ${ENC:0:12}... (${#ENC} chars)"

echo "== 2. Backup de la fila actual (por si hay que revertir) =="
curl -s "$CENTRAL/rest/v1/payment_gateway_credentials?id=eq.$ROW_ID" \
  -H "apikey: $CKEY" -H "Authorization: Bearer $CKEY" > /tmp/pgc_backup_$(date +%s 2>/dev/null || echo bak).json 2>/dev/null || true
echo "  backup en /tmp/pgc_backup_*.json"

echo "== 3. UPDATE a producción =="
code=$(curl -s -o /tmp/cutover_resp.json -w "%{http_code}" -X PATCH \
  "$CENTRAL/rest/v1/payment_gateway_credentials?id=eq.$ROW_ID" \
  -H "apikey: $CKEY" -H "Authorization: Bearer $CKEY" \
  -H "Content-Type: application/json" -H "Prefer: return=representation" \
  -d "{\"environment\":\"production\",\"merchant_id\":\"$MERCHANT_ID\",\"access_key\":\"$ACCESS_KEY\",\"secret_key_encrypted\":\"$ENC\"}")
echo "  HTTP $code"
[ "$code" = "200" ] && echo "  ✔ fila actualizada a producción" || { echo "  ✘ ERROR — revisar /tmp/cutover_resp.json"; exit 1; }

echo "== 4. Forzar recarga de la cache de credenciales (5 min TTL) — reinicio de máquinas =="
echo "  Ejecuta:  flyctl apps restart pull-api-v2-prod"
echo "  (o espera 5 min a que expire la cache)"

echo
echo "===== CUTOVER HECHO ====="
echo "SIGUIENTE (obligatorio antes de abrir al público):"
echo "  1. flyctl apps restart pull-api-v2-prod"
echo "  2. Compra REAL de prueba (Q1-5) con tu tarjeta en la web."
echo "  3. Verifica en el EBC2 PROD (businesscenter.cybersource.com) que salen"
echo "     las 2 transacciones -VENUE y -FEE en verde."
echo "  4. Reembólsala desde el EBC."
echo "  5. Si algo falla: revertir con /tmp/pgc_backup_*.json (PATCH de vuelta a environment=test)."
