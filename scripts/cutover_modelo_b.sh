#!/usr/bin/env bash
# =============================================
# CUTOVER MODELO B — dos cuentas Cybersource (511 Events, PRODUCCIÓN)
#
# Modelo B = 2 filas en payment_gateway_credentials, 2 chargers:
#   fila del venue (83ad16b1…)  → cobra el PRECIO de la entrada
#   fila "Pull Platform"        → cobra el FEE 8% de Pull
#   (services/payment_router.go:GetFeeProcessor elige la fila Pull cuando
#    el secret PLATFORM_FEE_VENUE_ID de Fly apunta a su venue_id)
#
# ⚠️ Esto toca la central de PRODUCCIÓN (dinero real). Antes de tocar nada
# hace backup de TODAS las filas de payment_gateway_credentials en
# .backups/ (gitignored) y pide confirmación escribiendo el modo.
#
# Modos:
#   pull-como-venue  Prueba real E2E: las credenciales de PRODUCCIÓN de
#                    PULL van en LAS DOS filas (venue + Pull Platform).
#                    Las 2 transacciones caen en la cuenta de Pull pero se
#                    ejercita el camino real de 2 chargers. Crea/actualiza
#                    la fila Pull Platform, persiste su venue_id en
#                    .env.prod.local (PLATFORM_FEE_VENUE_ID_VALUE) y sube
#                    el secret PLATFORM_FEE_VENUE_ID a Fly.
#   cliente          Swap futuro: SOLO la fila del venue pasa a las
#                    credenciales de PRODUCCIÓN del CLIENTE 511. No toca
#                    la fila Pull Platform ni PLATFORM_FEE_VENUE_ID.
#                    (Equivale a scripts/cutover_prod.sh.)
#   rollback <file>  Restaura las filas desde un backup de .backups/
#                    (PATCH por id; si la fila ya no existe, POST).
#
# Uso (desde Pull-API-v2/, con .env.prod.local presente):
#   MERCHANT_ID="<merchant prod>" \
#   ACCESS_KEY="<Key ID prod (REST)>" \
#   SHARED_SECRET="<Shared Secret prod (REST)>" \
#   bash scripts/cutover_modelo_b.sh pull-como-venue [--dry-run]
#
#   MERCHANT_ID=... ACCESS_KEY=... SHARED_SECRET=... \
#   bash scripts/cutover_modelo_b.sh cliente [--dry-run]
#
#   bash scripts/cutover_modelo_b.sh rollback .backups/pgc_YYYYmmdd_HHMMSS.json [--dry-run]
#
# --dry-run: imprime cada operación (método, URL, body con los campos
# sensibles [REDACTED]) SIN ejecutar nada de red ni flyctl.
#
# Requiere: .env.prod.local (APP_KEY + CENTRAL_SERVICE_KEY), curl, python,
# go (cifra con cmd/recrypt -encrypt, el MISMO cifrado que lee el backend)
# y flyctl (solo pull-como-venue). NUNCA imprime secretos.
# =============================================
set -euo pipefail
cd "$(dirname "$0")/.."
command -v flyctl >/dev/null 2>&1 || export PATH="$PATH:$HOME/.fly/bin"
command -v go >/dev/null 2>&1 || export PATH="$PATH:/c/Program Files/Go/bin:$HOME/go/bin"

CENTRAL="https://mwuppgpmlynfxyghkpzv.supabase.co"
VENUE_ROW_ID="83ad16b1-22db-43c5-8da9-2e61038300f9"   # fila payment_gateway_credentials del venue 511
VENUE_511_ID="5d3a4758-fabb-46a2-9dc3-86b2ee8bcafa"   # venue 511 en central PROD (para el fallback FK)
FLY_APP="pull-api-v2-prod"
ENV_FILE=".env.prod.local"
BACKUP_DIR=".backups"
PGC="payment_gateway_credentials"

usage() {
  cat <<'EOF'
Uso: bash scripts/cutover_modelo_b.sh <modo> [args] [--dry-run]

Modos:
  pull-como-venue   Prueba real: credenciales de PRODUCCIÓN de Pull en las
                    DOS filas (venue + Pull Platform) + secret
                    PLATFORM_FEE_VENUE_ID en Fly. Exige env vars
                    MERCHANT_ID, ACCESS_KEY, SHARED_SECRET (las de Pull).
  cliente           Swap al cliente: SOLO la fila del venue 511 pasa a las
                    credenciales del CLIENTE. Exige MERCHANT_ID,
                    ACCESS_KEY, SHARED_SECRET (las del cliente). No toca
                    la fila Pull Platform ni PLATFORM_FEE_VENUE_ID.
  rollback <file>   Restaura payment_gateway_credentials desde un backup
                    .backups/pgc_*.json (PATCH por id; POST si ya no existe).

Flags:
  --dry-run         Imprime las operaciones (URL, método, body con
                    secretos [REDACTED]) sin ejecutar red ni flyctl.

Ejemplos:
  MERCHANT_ID=... ACCESS_KEY=... SHARED_SECRET=... \
    bash scripts/cutover_modelo_b.sh pull-como-venue --dry-run
  bash scripts/cutover_modelo_b.sh rollback .backups/pgc_20260722_120000.json
EOF
}

die() { echo "✘ ERROR: $*" >&2; exit 1; }

# ── Parseo de argumentos ─────────────────────────────────────────────
MODE="${1:-}"
if [ -z "$MODE" ] || [ "$MODE" = "-h" ] || [ "$MODE" = "--help" ]; then
  usage; exit 1
fi
shift || true

DRY_RUN=0
ROLLBACK_FILE=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    *)
      if [ "$MODE" = "rollback" ] && [ -z "$ROLLBACK_FILE" ]; then
        ROLLBACK_FILE="$arg"
      else
        echo "Argumento desconocido: $arg"; echo; usage; exit 1
      fi ;;
  esac
done

case "$MODE" in
  pull-como-venue|cliente|rollback) ;;
  *) echo "Modo desconocido: $MODE"; echo; usage; exit 1 ;;
esac

# ── Precondiciones (mensajes claros, se comprueban ANTES de tocar nada) ──
[ -f "$ENV_FILE" ] || die "falta $ENV_FILE en la raíz del repo (contiene APP_KEY y CENTRAL_SERVICE_KEY de PROD; ver ENVIRONMENTS.md)"
APP_KEY=$(grep '^APP_KEY=' "$ENV_FILE" | head -1 | cut -d= -f2- | tr -d '\r')
CKEY=$(grep '^CENTRAL_SERVICE_KEY=' "$ENV_FILE" | head -1 | cut -d= -f2- | tr -d '\r')
[ -n "$APP_KEY" ] || die "$ENV_FILE no tiene APP_KEY (64 hex; con ella se cifra el Shared Secret igual que lo lee el backend)"
[ -n "$CKEY" ] || die "$ENV_FILE no tiene CENTRAL_SERVICE_KEY (service_role de la central de PROD)"
command -v curl >/dev/null 2>&1 || die "falta curl en PATH"
PYBIN="$(command -v python3 2>/dev/null || true)"
[ -n "$PYBIN" ] || PYBIN="$(command -v python 2>/dev/null || true)"
[ -n "$PYBIN" ] || die "falta python (3.x) en PATH — todo el JSON se construye/parsea con python, nunca con interpolación cruda"

if [ "$MODE" != "rollback" ]; then
  : "${MERCHANT_ID:?Falta MERCHANT_ID (merchant de producción)}"
  : "${ACCESS_KEY:?Falta ACCESS_KEY (Key ID REST de producción)}"
  : "${SHARED_SECRET:?Falta SHARED_SECRET (Shared Secret REST de producción)}"
  command -v go >/dev/null 2>&1 || die "falta go en PATH (probé también 'Program Files/Go/bin' y ~/go/bin) — el cifrado usa cmd/recrypt -encrypt"
fi
if [ "$MODE" = "pull-como-venue" ] && [ "$DRY_RUN" = "0" ]; then
  command -v flyctl >/dev/null 2>&1 || die "falta flyctl (probé también ~/.fly/bin) — necesario para subir PLATFORM_FEE_VENUE_ID"
fi
if [ "$MODE" = "rollback" ]; then
  [ -n "$ROLLBACK_FILE" ] || die "el modo rollback necesita el archivo de backup: bash scripts/cutover_modelo_b.sh rollback .backups/pgc_*.json"
  [ -f "$ROLLBACK_FILE" ] || die "no existe el backup '$ROLLBACK_FILE'"
fi

TMP_WORK="$(mktemp -d)"
trap 'rm -rf "$TMP_WORK"' EXIT
RESP="$TMP_WORK/resp.json"

# ── Helpers ──────────────────────────────────────────────────────────
json_len() {
  "$PYBIN" -c 'import json,sys; d=json.load(open(sys.argv[1])); print(len(d) if isinstance(d,list) else -1)' "$1"
}

# Imprime un JSON (archivo) con los campos sensibles tapados. Se usa para
# TODO output de bodies: jamás se imprime un body sin pasar por aquí.
redact_json() {
  "$PYBIN" - "$1" <<'PY'
import json, sys
SENSITIVE = ("access_key", "secret_key", "shared_secret", "service_key", "anon_key")
def clean(o):
    if isinstance(o, dict):
        out = {}
        for k, v in o.items():
            if v not in (None, "") and (k.endswith("_encrypted") or k in SENSITIVE):
                out[k] = "[REDACTED]"
            else:
                out[k] = clean(v)
        return out
    if isinstance(o, list):
        return [clean(x) for x in o]
    return o
print(json.dumps(clean(json.load(open(sys.argv[1]))), ensure_ascii=False))
PY
}

announce() { # announce METODO path [body_file] — línea de dry-run
  echo "  [DRY-RUN] $1 $CENTRAL/rest/v1/$2"
  if [ -n "${3:-}" ]; then
    echo "            body: $(redact_json "$3")"
  fi
}

explain_resp() { # imprime code/message/details/hint del error PostgREST (sin bodies nuestros)
  "$PYBIN" - "$RESP" <<'PY' >&2 || true
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("  (respuesta no-JSON)"); raise SystemExit
if isinstance(d, dict):
    for k in ("code", "message", "details", "hint"):
        if d.get(k):
            print("  %s: %s" % (k, d[k]))
else:
    print("  respuesta inesperada (array/otro)")
PY
}

rest_get() { # rest_get "tabla?query" out_file  → 0 si HTTP 200
  local code
  code=$(curl -s -o "$2" -w '%{http_code}' "$CENTRAL/rest/v1/$1" \
    -H "apikey: $CKEY" -H "Authorization: Bearer $CKEY")
  REST_CODE="$code"
  [ "$code" = "200" ]
}

rest_write() { # rest_write METODO "tabla?query" body_file  → 0 si 2xx (respuesta en $RESP)
  local code
  code=$(curl -s -o "$RESP" -w '%{http_code}' -X "$1" "$CENTRAL/rest/v1/$2" \
    -H "apikey: $CKEY" -H "Authorization: Bearer $CKEY" \
    -H "Content-Type: application/json" -H "Prefer: return=representation" \
    -d @"$3")
  REST_CODE="$code"
  case "$code" in 2*) return 0 ;; *) return 1 ;; esac
}

write_or_die() { # write_or_die METODO path body_file "descripción"
  if ! rest_write "$1" "$2" "$3"; then
    echo "  ✘ ERROR: $4 → HTTP $REST_CODE" >&2
    explain_resp
    exit 1
  fi
}

confirm_or_abort() { # confirm_or_abort PALABRA "resumen de lo que va a pasar"
  [ "$DRY_RUN" = "1" ] && { echo "  (dry-run: se salta la confirmación)"; return 0; }
  echo
  echo "⚠️  $2"
  echo "   Central PROD: $CENTRAL (DINERO REAL)"
  printf '   Escribe %s para continuar: ' "$1"
  read -r answer
  [ "$answer" = "$1" ] || die "confirmación incorrecta — no se ha tocado nada"
}

do_backup() { # do_backup prefijo → deja la ruta en $BACKUP_FILE
  BACKUP_FILE="$BACKUP_DIR/${1}_$(date +%Y%m%d_%H%M%S).json"
  if [ "$DRY_RUN" = "1" ]; then
    announce GET "$PGC?select=*"
    echo "  [DRY-RUN]   → se guardaría en $BACKUP_FILE (TODAS las filas)"
    return 0
  fi
  mkdir -p "$BACKUP_DIR"
  # Cinturón y tirantes: los backups llevan ciphertexts — jamás a git.
  grep -q '^\.backups/' .gitignore 2>/dev/null || echo ".backups/" >> .gitignore
  rest_get "$PGC?select=*" "$BACKUP_FILE" || die "no pude leer $PGC de la central (HTTP $REST_CODE)"
  local n; n=$(json_len "$BACKUP_FILE")
  [ "$n" -ge 1 ] 2>/dev/null || die "el backup no trajo filas ($BACKUP_FILE) — no sigo sin red de seguridad"
  echo "  ✔ backup de $n fila(s) en $BACKUP_FILE"
}

encrypt_secret() { # cifra $SHARED_SECRET con APP_KEY → $ENC (placeholder en dry-run)
  if [ "$DRY_RUN" = "1" ]; then
    echo "  [DRY-RUN] (local, sin red) printf SHARED_SECRET | NEW_KEY=<APP_KEY de $ENV_FILE> go run ./cmd/recrypt -encrypt"
    ENC="DRYRUN-CIPHERTEXT-PLACEHOLDER"
    return 0
  fi
  ENC=$(printf '%s' "$SHARED_SECRET" | NEW_KEY="$APP_KEY" go run ./cmd/recrypt -encrypt)
  [ -n "$ENC" ] || die "cmd/recrypt no devolvió ciphertext"
  echo "  ✔ Shared Secret cifrado con la APP_KEY del backend (${#ENC} chars; no se muestra)"
}

build_creds_body() { # build_creds_body out_file [venue_id] — JSON SIEMPRE via python
  local out="$1" vid="${2:-}"
  OUT_VID="$vid" B_MID="$MERCHANT_ID" B_AK="$ACCESS_KEY" B_ENC="$ENC" "$PYBIN" - > "$out" <<'PY'
import json, os
body = {
    "environment": "production",
    "merchant_id": os.environ["B_MID"],
    "access_key": os.environ["B_AK"],
    "secret_key_encrypted": os.environ["B_ENC"],
}
vid = os.environ.get("OUT_VID", "")
if vid:
    # Fila Pull Platform: columnas EXACTAS que lee loadPaymentConfig
    # (services/payment_router.go). No inventamos columnas: default_currency
    # cae a GTQ en código y profile_id/terminal_id no se usan en el flujo REST.
    body.update({"venue_id": vid, "gateway": "neonet",
                 "is_active": True, "is_primary": True})
print(json.dumps(body))
PY
}

restart_note() {
  echo
  echo "RECUERDA: la caché de credenciales del backend dura 5 MIN (payment_router)."
  echo "  Para no esperar: flyctl apps restart $FLY_APP"
}

# ═════════════════════════════════════════════════════════════════════
# MODO pull-como-venue
# ═════════════════════════════════════════════════════════════════════
mode_pull_como_venue() {
  confirm_or_abort "PULL-COMO-VENUE" \
"Vas a poner las credenciales de PRODUCCIÓN de Pull en la fila del venue
   ($VENUE_ROW_ID) Y en la fila Pull Platform, y a configurar
   PLATFORM_FEE_VENUE_ID en Fly ($FLY_APP). Las compras cobrarán DINERO REAL."

  echo "== 1. Backup de TODAS las filas de $PGC =="
  do_backup "pgc"

  echo "== 2. Cifrado del Shared Secret (cmd/recrypt -encrypt, mismo cifrado que descifra el backend) =="
  encrypt_secret

  echo "== 3. PATCH fila del venue ($VENUE_ROW_ID) → production + credenciales de Pull =="
  local venue_body="$TMP_WORK/venue_patch.json"
  build_creds_body "$venue_body"
  if [ "$DRY_RUN" = "1" ]; then
    announce PATCH "$PGC?id=eq.$VENUE_ROW_ID" "$venue_body"
  else
    write_or_die PATCH "$PGC?id=eq.$VENUE_ROW_ID" "$venue_body" "PATCH de la fila del venue"
    [ "$(json_len "$RESP")" = "1" ] || die "el PATCH no encontró la fila $VENUE_ROW_ID (¿id correcto?) — revisa el backup $BACKUP_FILE"
    echo "  ✔ fila del venue → environment=production, merchant_id=$MERCHANT_ID"
  fi

  echo "== 4. Fila 'Pull Platform' (el charger del fee 8%) =="
  # Detección de fila existente: por venue_id == PLATFORM_FEE_VENUE_ID_VALUE
  # persistido en .env.prod.local (es el mismo UUID que GetFeeProcessor usa
  # para cargarla — el secret de Fly apunta a este venue_id, no al id de fila).
  PF_UUID=$(grep '^PLATFORM_FEE_VENUE_ID_VALUE=' "$ENV_FILE" | head -1 | cut -d= -f2- | tr -d '\r' || true)
  local pf_is_new=0
  if [ -n "$PF_UUID" ]; then
    echo "  → reutilizo PLATFORM_FEE_VENUE_ID_VALUE de $ENV_FILE: $PF_UUID"
  else
    PF_UUID=$("$PYBIN" -c 'import uuid; print(uuid.uuid4())')
    pf_is_new=1
    echo "  → UUID nuevo para la fila Pull Platform: $PF_UUID"
  fi
  local pull_body="$TMP_WORK/pull_row.json"
  build_creds_body "$pull_body" "$PF_UUID"

  if [ "$DRY_RUN" = "1" ]; then
    echo "  [DRY-RUN] Si ya existe fila con venue_id=$PF_UUID → PATCH; si no → POST:"
    announce GET "$PGC?venue_id=eq.$PF_UUID&select=id"
    announce PATCH "$PGC?venue_id=eq.$PF_UUID" "$pull_body"
    announce POST "$PGC" "$pull_body"
    echo "  [DRY-RUN]   (si el POST fallara con FK 23503 venue_id→venues(id), se crearía"
    echo "  [DRY-RUN]    una fila mínima en venues {id=$PF_UUID, organization_id=<la del venue 511>,"
    echo "  [DRY-RUN]    name='Pull Platform (cuenta fee)', slug='pull-platform-fee', is_active=false}"
    echo "  [DRY-RUN]    y se reintentaría el POST)"
  else
    rest_get "$PGC?venue_id=eq.$PF_UUID&select=id" "$TMP_WORK/pf_check.json" \
      || die "no pude comprobar si ya existe la fila Pull Platform (HTTP $REST_CODE)"
    local n_pf; n_pf=$(json_len "$TMP_WORK/pf_check.json")
    if [ "$n_pf" = "1" ]; then
      write_or_die PATCH "$PGC?venue_id=eq.$PF_UUID" "$pull_body" "PATCH de la fila Pull Platform existente"
      echo "  ✔ fila Pull Platform existente actualizada (venue_id=$PF_UUID)"
    elif [ "$n_pf" = "0" ]; then
      if ! rest_write POST "$PGC" "$pull_body"; then
        if grep -q '23503' "$RESP" 2>/dev/null; then
          echo "  ⚠ la tabla tiene FK venue_id→venues(id) — creo fila mínima en venues para la cuenta fee"
          rest_get "venues?id=eq.$VENUE_511_ID&select=organization_id" "$TMP_WORK/org.json" \
            || die "no pude leer organization_id del venue 511 (HTTP $REST_CODE)"
          local org_id
          org_id=$("$PYBIN" -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d[0]["organization_id"] if d else "")' "$TMP_WORK/org.json")
          [ -n "$org_id" ] || die "el venue 511 ($VENUE_511_ID) no aparece en venues — revisa la central"
          local venues_body="$TMP_WORK/venues_min.json"
          V_ID="$PF_UUID" V_ORG="$org_id" "$PYBIN" - > "$venues_body" <<'PY'
import json, os
print(json.dumps({"id": os.environ["V_ID"], "organization_id": os.environ["V_ORG"],
                  "name": "Pull Platform (cuenta fee)", "slug": "pull-platform-fee",
                  "is_active": False}))
PY
          write_or_die POST "venues" "$venues_body" "POST de la fila mínima en venues (solo satisface la FK; is_active=false)"
          echo "  ✔ fila mínima en venues creada (id=$PF_UUID, is_active=false)"
          write_or_die POST "$PGC" "$pull_body" "POST de la fila Pull Platform (reintento tras crear venues)"
        else
          echo "  ✘ ERROR: POST de la fila Pull Platform → HTTP $REST_CODE" >&2
          explain_resp
          exit 1
        fi
      fi
      echo "  ✔ fila Pull Platform creada (venue_id=$PF_UUID, gateway=neonet, production, primaria y activa)"
    else
      die "hay $n_pf filas con venue_id=$PF_UUID — imposible; revisa la central a mano"
    fi
  fi

  echo "== 5. Persistir PLATFORM_FEE_VENUE_ID_VALUE en $ENV_FILE =="
  if [ "$DRY_RUN" = "1" ]; then
    echo "  [DRY-RUN] (local) se escribiría PLATFORM_FEE_VENUE_ID_VALUE=$PF_UUID en $ENV_FILE"
    [ "$pf_is_new" = "1" ] && echo "  [DRY-RUN]   (UUID de ejemplo: en la ejecución real se genera y persiste uno)"
  else
    if grep -q '^PLATFORM_FEE_VENUE_ID_VALUE=' "$ENV_FILE"; then
      sed -i "s|^PLATFORM_FEE_VENUE_ID_VALUE=.*|PLATFORM_FEE_VENUE_ID_VALUE=$PF_UUID|" "$ENV_FILE"
    else
      {
        echo ""
        echo "# venue_id de la fila 'Pull Platform' en payment_gateway_credentials (modelo B)."
        echo "# Es el valor del secret PLATFORM_FEE_VENUE_ID en Fly. Generado por scripts/cutover_modelo_b.sh."
        echo "PLATFORM_FEE_VENUE_ID_VALUE=$PF_UUID"
      } >> "$ENV_FILE"
    fi
    echo "  ✔ persistido (PLATFORM_FEE_VENUE_ID_VALUE=$PF_UUID)"
  fi

  echo "== 6. Secret PLATFORM_FEE_VENUE_ID en Fly ($FLY_APP) =="
  if [ "$DRY_RUN" = "1" ]; then
    echo "  [DRY-RUN] flyctl secrets set -a $FLY_APP PLATFORM_FEE_VENUE_ID=$PF_UUID"
  else
    flyctl secrets set -a "$FLY_APP" "PLATFORM_FEE_VENUE_ID=$PF_UUID"
    echo "  ✔ secret subido — 'secrets set' ya crea release y reinicia las máquinas."
    echo "    Si el estado no cambia (release fallido): flyctl apps restart $FLY_APP"
  fi
  restart_note

  echo
  echo "== 7. Verificación post-cutover =="
  local verify_q="$PGC?environment=eq.production&select=id,venue_id,environment,merchant_id,is_active,is_primary"
  if [ "$DRY_RUN" = "1" ]; then
    announce GET "$verify_q"
    echo "  [DRY-RUN]   → se esperan EXACTAMENTE 2 filas en production (venue + Pull Platform)"
    echo "  [DRY-RUN] flyctl secrets list -a $FLY_APP | grep PLATFORM"
  else
    rest_get "$verify_q" "$TMP_WORK/verify.json" || die "verificación: GET falló (HTTP $REST_CODE)"
    "$PYBIN" - "$TMP_WORK/verify.json" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))
for r in rows:
    print("  fila %s  venue_id=%s  env=%s  merchant=%s  active=%s  primary=%s"
          % (r.get("id"), r.get("venue_id"), r.get("environment"),
             r.get("merchant_id"), r.get("is_active"), r.get("is_primary")))
PY
    local n_prod; n_prod=$(json_len "$TMP_WORK/verify.json")
    if [ "$n_prod" = "2" ]; then
      echo "  ✔ exactamente 2 filas en production (venue + Pull Platform) — modelo B armado"
    else
      echo "  ⚠ hay $n_prod fila(s) en production y se esperaban 2 — revisa arriba antes de cobrar nada"
    fi
    flyctl secrets list -a "$FLY_APP" | grep -i PLATFORM \
      || echo "  ⚠ PLATFORM_FEE_VENUE_ID no aparece en flyctl secrets list — revísalo"
  fi

  echo
  echo "===== SIGUIENTES PASOS (obligatorios antes de abrir al público) ====="
  echo "  1. Espera el reinicio de $FLY_APP (o fuérzalo) — caché de credenciales 5 min."
  echo "  2. Compra REAL pequeña (Q1-5) con tu tarjeta en la web de producción."
  echo "  3. Verifica en el EBC PROD (businesscenter.cybersource.com) que aparecen"
  echo "     las 2 transacciones (entrada -VENUE y fee -FEE) en verde."
  echo "  4. Reembólsalas desde el EBC."
  echo "  5. Si todo cuadra: abrir. El día del swap al cliente:"
  echo "     MERCHANT_ID=... ACCESS_KEY=... SHARED_SECRET=... bash scripts/cutover_modelo_b.sh cliente"
  echo "  Rollback: bash scripts/cutover_modelo_b.sh rollback $BACKUP_DIR/pgc_*.json"
}

# ═════════════════════════════════════════════════════════════════════
# MODO cliente
# ═════════════════════════════════════════════════════════════════════
mode_cliente() {
  confirm_or_abort "CLIENTE" \
"Vas a cambiar SOLO la fila del venue ($VENUE_ROW_ID) a las credenciales de
   PRODUCCIÓN del CLIENTE 511. La fila Pull Platform y PLATFORM_FEE_VENUE_ID
   quedan intactos (el fee 8% sigue cayendo en la cuenta de Pull)."

  echo "== 1. Backup de TODAS las filas de $PGC =="
  do_backup "pgc"

  echo "== 2. Cifrado del Shared Secret del cliente (cmd/recrypt -encrypt) =="
  encrypt_secret

  echo "== 3. PATCH fila del venue ($VENUE_ROW_ID) → credenciales del CLIENTE =="
  local venue_body="$TMP_WORK/venue_patch.json"
  build_creds_body "$venue_body"
  if [ "$DRY_RUN" = "1" ]; then
    announce PATCH "$PGC?id=eq.$VENUE_ROW_ID" "$venue_body"
    echo "  [DRY-RUN] (NO se toca la fila Pull Platform ni el secret PLATFORM_FEE_VENUE_ID)"
  else
    write_or_die PATCH "$PGC?id=eq.$VENUE_ROW_ID" "$venue_body" "PATCH de la fila del venue"
    [ "$(json_len "$RESP")" = "1" ] || die "el PATCH no encontró la fila $VENUE_ROW_ID — revisa el backup $BACKUP_FILE"
    echo "  ✔ fila del venue → credenciales del cliente (merchant_id=$MERCHANT_ID)"
    echo "  ✔ fila Pull Platform y PLATFORM_FEE_VENUE_ID: intactos"
  fi

  echo
  echo "== 4. Verificación =="
  local verify_q="$PGC?id=eq.$VENUE_ROW_ID&select=id,venue_id,environment,merchant_id,is_active,is_primary"
  if [ "$DRY_RUN" = "1" ]; then
    announce GET "$verify_q"
  else
    rest_get "$verify_q" "$TMP_WORK/verify.json" || die "verificación: GET falló (HTTP $REST_CODE)"
    "$PYBIN" - "$TMP_WORK/verify.json" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))
for r in rows:
    print("  fila %s  venue_id=%s  env=%s  merchant=%s  active=%s  primary=%s"
          % (r.get("id"), r.get("venue_id"), r.get("environment"),
             r.get("merchant_id"), r.get("is_active"), r.get("is_primary")))
PY
  fi
  restart_note

  echo
  echo "===== SIGUIENTES PASOS ====="
  echo "  1. flyctl apps restart $FLY_APP (o espera 5 min de caché)."
  echo "  2. Compra REAL pequeña de verificación: la ENTRADA debe caer en el EBC"
  echo "     del CLIENTE y el FEE en el EBC de Pull. Reembolsa después."
  echo "  Rollback: bash scripts/cutover_modelo_b.sh rollback $BACKUP_DIR/pgc_*.json"
}

# ═════════════════════════════════════════════════════════════════════
# MODO rollback
# ═════════════════════════════════════════════════════════════════════
mode_rollback() {
  local n
  n=$(json_len "$ROLLBACK_FILE")
  { [ "$n" -ge 1 ] 2>/dev/null; } || die "'$ROLLBACK_FILE' no es un backup válido (array JSON con ≥1 fila)"
  echo "Backup: $ROLLBACK_FILE ($n fila(s))"

  confirm_or_abort "ROLLBACK" \
"Vas a restaurar $n fila(s) de $PGC en la central de PROD desde
   $ROLLBACK_FILE (PATCH por id; POST si la fila ya no existe)."

  echo "== 1. Backup del estado ACTUAL (por si hay que des-hacer el rollback) =="
  do_backup "pgc_pre_rollback"

  echo "== 2. Restaurar filas =="
  local i=0
  while [ "$i" -lt "$n" ]; do
    local row_file="$TMP_WORK/row_$i.json"
    "$PYBIN" -c 'import json,sys; rows=json.load(open(sys.argv[1])); json.dump(rows[int(sys.argv[2])], open(sys.argv[3],"w"))' \
      "$ROLLBACK_FILE" "$i" "$row_file"
    local rid
    rid=$("$PYBIN" -c 'import json,sys; print(json.load(open(sys.argv[1])).get("id",""))' "$row_file")
    [ -n "$rid" ] || die "la fila #$i del backup no tiene 'id' — backup corrupto"
    if [ "$DRY_RUN" = "1" ]; then
      announce PATCH "$PGC?id=eq.$rid" "$row_file"
      echo "  [DRY-RUN]   → si la respuesta viene vacía (la fila ya no existe): POST $CENTRAL/rest/v1/$PGC con esa misma fila"
    else
      write_or_die PATCH "$PGC?id=eq.$rid" "$row_file" "PATCH restaurando la fila $rid"
      if [ "$(json_len "$RESP")" = "0" ]; then
        write_or_die POST "$PGC" "$row_file" "POST re-creando la fila $rid (ya no existía)"
        echo "  ✔ fila $rid re-creada (POST — no existía)"
      else
        echo "  ✔ fila $rid restaurada (PATCH)"
      fi
    fi
    i=$((i + 1))
  done
  restart_note
  echo
  echo "Si el rollback deshace un 'pull-como-venue', recuerda también quitar el secret:"
  echo "  flyctl secrets unset -a $FLY_APP PLATFORM_FEE_VENUE_ID   (si procede)"
}

# ── Dispatch ─────────────────────────────────────────────────────────
[ "$DRY_RUN" = "1" ] && echo "### DRY-RUN — no se ejecuta NADA de red ni flyctl ###"
case "$MODE" in
  pull-como-venue) mode_pull_como_venue ;;
  cliente)         mode_cliente ;;
  rollback)        mode_rollback ;;
esac
