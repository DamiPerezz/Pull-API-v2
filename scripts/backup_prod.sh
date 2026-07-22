#!/usr/bin/env bash
# =============================================================================
# Backup manual de PRODUCCIÓN vía REST (el free tier de Supabase no tiene PITR).
# Vuelca cada tabla a JSON con la service key. Correr el VIERNES tras limpiar
# datos de prueba, y guardar la carpeta FUERA de Supabase (Drive/USB).
# Uso:  bash scripts/backup_prod.sh
# Lee las URLs y service keys de .env.prod.local (nunca imprime secretos).
# =============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f .env.prod.local ] || { echo "ABORTADO: no encuentro .env.prod.local"; exit 1; }
command -v python >/dev/null || { echo "ABORTADO: falta python"; exit 1; }

CU=$(grep '^CENTRAL_SUPABASE_URL=' .env.prod.local | cut -d= -f2- | tr -d '\r')
CK=$(grep '^CENTRAL_SERVICE_KEY='  .env.prod.local | cut -d= -f2- | tr -d '\r')
VU=$(grep '^DEFAULT_SUPABASE_URL=' .env.prod.local | cut -d= -f2- | tr -d '\r')
VK=$(grep '^DEFAULT_SERVICE_KEY='  .env.prod.local | cut -d= -f2- | tr -d '\r')
[ -n "$CU" ] && [ -n "$CK" ] && [ -n "$VU" ] && [ -n "$VK" ] || { echo "ABORTADO: faltan URLs/keys en .env.prod.local"; exit 1; }

STAMP=$(python -c "import datetime;print(datetime.datetime.now().strftime('%Y%m%d_%H%M%S'))")
OUT=".backups/prod_$STAMP"
mkdir -p "$OUT/central" "$OUT/venue"

dump() { # base key tabla destino
  local n
  n=$(curl -s "$1/rest/v1/$3?select=*" -H "apikey: $2" -H "Authorization: Bearer $2" -o "$4/$3.json" -w '%{http_code}')
  local rows
  rows=$(python -c "import json,sys;print(len(json.load(open('$4/$3.json'))))" 2>/dev/null || echo '?')
  printf "  %-28s HTTP %s  (%s filas)\n" "$3" "$n" "$rows"
}

echo "== CENTRAL ($OUT/central) =="
for t in venues venue_database_configs payment_gateway_credentials pull_staff transactions organizations; do
  dump "$CU" "$CK" "$t" "$OUT/central"
done
echo "== VENUE ($OUT/venue) =="
for t in events ticket_types orders tickets public_users verification_codes organization_workers roles guest_list_types guest_list_signups group_reservations staff_push_tokens; do
  dump "$VU" "$VK" "$t" "$OUT/venue"
done

echo ""
echo "BACKUP EN: $OUT"
echo "⚠️ COPIA esta carpeta FUERA de Supabase (Drive/USB). Contiene datos personales de clientes."
