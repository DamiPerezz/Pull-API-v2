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
#
# NOTA fuente de verdad: 'origin' es el fork DamiPerezz/Pull-API-v2 — ahí viven
# el código de producción y los tags prod-*. El repo GreenLock (upstream) está
# desfasado y no acepta push (403); NO es referencia para rollbacks.
# =============================================================================
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
if [ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]; then
  if git merge-base --is-ancestor HEAD origin/main; then
    behind=$(git rev-list --count HEAD..origin/main)
    echo "ERROR: HEAD está $behind commit(s) POR DETRÁS de origin/main — deployarías"
    echo "       código viejo y el tag mentiría. Haz 'git pull' primero."
    if [ "${ALLOW_OLD_DEPLOY:-}" = "1" ]; then
      echo "(ALLOW_OLD_DEPLOY=1 — rollback deliberado a versión antigua, continuando)"
    else
      echo "       Para un rollback deliberado: ALLOW_OLD_DEPLOY=1 bash scripts/deploy_prod.sh"
      exit 1
    fi
  else
    echo "ERROR: HEAD no está pusheado a origin/main. Haz 'git push' primero:"
    echo "       nunca deployamos código que no esté respaldado en GitHub."
    exit 1
  fi
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

# ── VERSIONADO x.x.xx ────────────────────────────────────────────────────────
# El fichero VERSION (raíz del repo) manda. Formato MAYOR.MENOR.PARCHE, con el
# parche a 2 dígitos: 1.0.00, 1.0.01, … 1.1.00, 2.0.00.
#
#   PARCHE  lo sube SOLO este script, en cada deploy verificado.
#   MENOR   súbelo a mano cuando añadas algo nuevo visible para el usuario.
#   MAYOR   súbelo a mano en un cambio grande o incompatible.
#
# Se etiqueta DESPUÉS del health check a propósito: una versión solo existe si
# de verdad está viva. Si el deploy no verifica, no se gasta número.
bump_version() {
  local f="$ROOT/VERSION"
  [ -f "$f" ] || echo "1.0.00" > "$f"
  local cur M rest m p
  cur=$(tr -d ' \r\n' < "$f")
  M=${cur%%.*}; rest=${cur#*.}; m=${rest%%.*}; p=${rest##*.}
  p=$((10#$p + 1))
  # El parche son 2 dígitos: al llegar a 100 arrastra a MENOR y vuelve a 00.
  # Sin esto la versión 100 saldría como "1.0.100" y rompería el formato.
  if [ "$p" -gt 99 ]; then
    m=$((10#$m + 1))
    p=0
  fi
  printf "%s.%s.%02d" "$M" "$m" "$p"
}

version="$(bump_version)"
tag="v${version}"
echo "$version" > "$ROOT/VERSION"
git add "$ROOT/VERSION" >/dev/null 2>&1 || true
git commit -q -m "chore: version ${version}" >/dev/null 2>&1 || true
git push -q origin HEAD >/dev/null 2>&1 || true
if git tag -a "$tag" -m "Deploy $APP commit $commit (version $version)" && git push origin "$tag" --quiet; then
  echo ""
  echo "== DEPLOYADO: $commit → $APP  (tag: $tag) =="
else
  echo ""
  echo "== DEPLOYADO: $commit → $APP  — PERO el tag falló =="
  echo "El deploy SÍ está vivo y verificado; crea el tag a mano:"
  echo "  git tag -a $tag -m 'Deploy $APP commit $commit' && git push origin $tag"
fi
