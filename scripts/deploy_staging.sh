#!/usr/bin/env bash
# =============================================================================
# Deploy de STAGING — pull-api-v2-staging (espejo de prod, Cybersource sandbox)
#
#   bash scripts/deploy_staging.sh
#
# Guards: árbol limpio + build/vet. A diferencia de prod, NO exige rama main
# ni push previo: staging sirve justo para probar cualquier commit (incluido
# reproducir el tag exacto que falla en prod: git checkout <tag> y deployar).
# La máquina se auto-apaga sin tráfico; la primera request la despierta (~1s).
# =============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
command -v flyctl >/dev/null 2>&1 || export PATH="$PATH:$HOME/.fly/bin"

APP="pull-api-v2-staging"
CFG="fly.staging.toml"
URL="https://pull-api-v2-staging.fly.dev"

if [ -n "$(git status --porcelain)" ]; then
  echo "ERROR: árbol sucio — commitea antes de deployar (aunque sea a staging):"
  echo "       si no está commiteado, no sabremos qué versión estaba corriendo."
  git status -sb
  exit 1
fi

echo "== go build + go vet =="
go build ./...
go vet ./...

commit=$(git rev-parse --short HEAD)
branch=$(git rev-parse --abbrev-ref HEAD)
echo "== Deploy $APP (commit $commit, rama $branch, config $CFG) =="
flyctl deploy -c "$CFG" --remote-only --strategy immediate

echo "== Health check =="
sleep 5
if curl -sf --max-time 30 "$URL/health" >/dev/null; then
  echo "HEALTH OK ($URL/health)"
else
  echo "ERROR: $URL/health no responde tras el deploy."
  echo "       Mira: flyctl logs -a $APP   y   flyctl status -a $APP"
  exit 1
fi

echo ""
echo "== DEPLOYADO: $commit ($branch) → $APP =="
