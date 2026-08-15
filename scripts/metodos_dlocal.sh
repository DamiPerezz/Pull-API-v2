#!/usr/bin/env bash
# =============================================================================
# ¿QUÉ MÉTODOS DE PAGO TIENE dLOCAL EN CADA PAÍS?
#
#     bash scripts/metodos_dlocal.sh              # GT y AR (comparativa)
#     bash scripts/metodos_dlocal.sh GT UY MX     # los que quieras
#
# Cómo funciona: se crea un cobro de prueba en ese país y se le pregunta a
# dLocal qué métodos ofrecería. NO cobra nada — los pagos nacen PENDING y
# caducan solos.
#
# OJO: el endpoint que devuelve la lista NO está documentado por dLocal. Lo
# usa su propia página de checkout (lo saqué de su bundle de JavaScript), así
# que podría cambiar sin avisar. Si un día deja de funcionar, no es un bug
# nuestro: es que lo han movido.
# =============================================================================
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
[ -f .env.dlocal.local ] && { set -a; . ./.env.dlocal.local; set +a; }
: "${DLOCAL_API_KEY:?falta DLOCAL_API_KEY (está en .env.dlocal.local)}"
: "${DLOCAL_SECRET_KEY:?falta DLOCAL_SECRET_KEY}"
AUTH="Authorization: Bearer ${DLOCAL_API_KEY}:${DLOCAL_SECRET_KEY}"

# Moneda por país (la local; si no coincide, dLocal la convierte).
# Importe de consulta por país: tiene que superar el mínimo de dLocal, que
# varía mucho (en GTQ son ~10, en ARS o COP hacen falta miles).
importe() { case "$1" in
  AR|CO|PY|CL) echo 20000;; BR|UY|MX|PE|BO) echo 500;; *) echo 100;; esac; }

moneda() { case "$1" in
  GT) echo GTQ;; AR) echo ARS;; UY) echo UYU;; MX) echo MXN;; BR) echo BRL;;
  CL) echo CLP;; CO) echo COP;; PE) echo PEN;; EC) echo USD;; CR) echo USD;;
  PY) echo PYG;; BO) echo BOB;; *) echo USD;; esac; }

PAISES=("$@"); [ ${#PAISES[@]} -eq 0 ] && PAISES=(GT AR)

for P in "${PAISES[@]}"; do
  M=$(moneda "$P"); A=$(importe "$P")
  # 1) crear el cobro (no cobra: nace PENDING)
  TOK=$(curl -s -X POST "https://api.dlocalgo.com/v1/payments" \
        -H "$AUTH" -H 'Content-Type: application/json' \
        -d "{\"amount\":$A,\"currency\":\"$M\",\"country\":\"$P\",\"description\":\"consulta metodos\",\"success_url\":\"https://511events.pullevents.com\"}" \
        | python -c "import sys,json;print(json.load(sys.stdin).get('merchant_checkout_token',''))" 2>/dev/null)

  if [ -z "$TOK" ]; then
    printf "\n%s (%s): no se pudo crear el cobro — puede que dLocal no opere en ese país\n" "$P" "$M"
    continue
  fi

  # 2) preguntar qué métodos ofrece ese cobro (sin auth: es público)
  TMP="${TMPDIR:-$HOME}/dl_$P.json"
  curl -s "https://api.dlocalgo.com/v1/checkout/$TOK" -o "$TMP"
  P="$P" M="$M" TMP="$TMP" python - <<'PY'
import json, os, collections
p = os.environ['P']
d = json.load(open(os.environ['TMP'], encoding='utf-8'))
ms = d.get('paymentMethods') or []
por_tipo = collections.Counter(m.get('type', '?') for m in ms)
print("\n=== %s (%s) — %d métodos ===" % (p, os.environ['M'], len(ms)))
tarjetas = [m for m in ms if 'CARD' in (m.get('type') or '')]
print("   TARJETA: %s" % ("SÍ, %d" % len(tarjetas) if tarjetas else "NO HAY"))
for t, n in por_tipo.most_common():
    print("     %-14s %d" % (t, n))
for m in ms[:8]:
    print("      - %-5s %-22s %s" % (m.get('id'), (m.get('name') or '')[:22], m.get('type')))
if len(ms) > 8:
    print("      ... y %d más" % (len(ms) - 8))
PY
done

cat <<'FIN'

CÓMO LEERLO
  TARJETA: NO HAY  -> dLocal Go no ofrece tarjeta en ese país (para esta cuenta).
                      No es configuración: no existe el método. Solo lo puede
                      cambiar dLocal.
  TARJETA: SÍ      -> ahí sí se puede cobrar con tarjeta.

Si un país dice NO y otro dice SÍ con LA MISMA cuenta, queda demostrado que el
problema no son tus claves ni tu configuración: es la cobertura del país.
FIN
