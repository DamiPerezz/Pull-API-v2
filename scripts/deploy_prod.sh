#!/usr/bin/env bash
# =============================================================================
# Deploy de PRODUCCIÓN — pull-api-v2-prod (511 Events, dinero real)
#
#   bash scripts/deploy_prod.sh
#
# Guards (el script se niega si no se cumplen):
#   1. Rama main (producción SOLO se deploya desde main)
#   2. Árbol limpio (nada sin commitear)
#   3. HEAD pusheado a origin/main (el respaldo va ANTES que el deploy)
#   4. go build + go vet pasan
# Después del deploy: health check + tag prod-YYYYMMDD-HHMM (responde a
# "¿qué versión hay en producción?" y da el punto exacto de rollback).
#
# Rollback rápido (sin rebuild):
#   flyctl releases -a pull-api-v2-prod          # lista releases con imagen
#   flyctl deploy -c fly.prod.toml --image <ref-imagen-anterior>
# Rollback por código: git checkout <tag-anterior> -- . && commit && deploy.
# =============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
command -v flyctl >/dev/null 2>&1 || export PATH="$PATH:$HOME/.fly/bin"

APP="pull-api-v2-prod"
CFG="fly.prod.toml"
URL="https://pull-api-v2-prod.fly.dev"

branch=$(git rev-parse --abbrev-ref HEAD)
if [ "$branch" != "main" ]; then
  echo "ERROR: producción se deploya desde 'main' (estás en '$branch')."
  echo "       Prueba en staging desde dev, y cuando esté verde: merge a main."
  exit 1
fi

if [ -n "$(git status --porcelain)" ]; then
  echo "ERROR: árbol sucio — commitea (o stashea) antes de deployar:"
  git status -sb
  exit 1
fi

git fetch origin main --quiet
if ! git merge-base --is-ancestor HEAD origin/main; then
  echo "ERROR: HEAD no está en origin/main. Haz 'git push' primero:"
  echo "       nunca deployamos código que no esté respaldado en GitHub."
  exit 1
fi

echo "== go build + go vet =="
go build ./...
go vet ./...

commit=$(git rev-parse --short HEAD)
echo "== Deploy $APP (commit $commit, estrategia rolling, config $CFG) =="
flyctl deploy -c "$CFG" --remote-only --strategy rolling

echo "== Health check =="
sleep 5
if curl -sf --max-time 15 "$URL/health" >/dev/null; then
  echo "HEALTH OK ($URL/health)"
else
  echo "ERROR: $URL/health no responde tras el deploy."
  echo "       Mira: flyctl logs -a $APP   y   flyctl status -a $APP"
  echo "       NO se ha creado tag (el deploy no está verificado)."
  exit 1
fi

tag="prod-$(date +%Y%m%d-%H%M)"
git tag -a "$tag" -m "Deploy $APP commit $commit"
git push origin "$tag" --quiet
echo ""
echo "== DEPLOYADO: $commit → $APP  (tag: $tag) =="
