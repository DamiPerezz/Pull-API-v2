#!/usr/bin/env bash
# =============================================================================
# Selector de entorno para ejecución LOCAL del backend (go run / scripts).
# Copia .env.<entorno>.local → .env con un banner. NUNCA toca Fly ni deploya.
#
#   bash scripts/use_env.sh staging   # desarrollo normal (BDs staging, sandbox)
#   bash scripts/use_env.sh prod      # EXCEPCIONAL: BDs de PRODUCCIÓN
#   bash scripts/use_env.sh demo      # demo Aurora (si existe .env.demo.local)
# =============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ENV_NAME="${1:-}"
case "$ENV_NAME" in
  staging|prod|demo) ;;
  *) echo "Uso: bash scripts/use_env.sh {staging|prod|demo}"; exit 1 ;;
esac

SRC=".env.${ENV_NAME}.local"
if [ ! -f "$SRC" ]; then
  echo "ERROR: no existe $SRC en $(pwd)."
  exit 1
fi

if [ "$ENV_NAME" = "prod" ]; then
  echo "⚠️  Vas a apuntar tu backend LOCAL a las bases de datos de PRODUCCIÓN"
  echo "   (511 Events — clientes y dinero real). Casi siempre lo que quieres"
  echo "   es 'staging'."
  read -r -p "Escribe PRODUCCION para confirmar: " ok
  if [ "$ok" != "PRODUCCION" ]; then
    echo "Cancelado."
    exit 1
  fi
fi

{
  echo "# >>> ENTORNO ACTIVO: ${ENV_NAME} — generado por scripts/use_env.sh <<<"
  echo "# >>> NO editar a mano: edita ${SRC} y re-ejecuta el script. <<<"
  echo ""
  cat "$SRC"
} > .env

echo "OK: .env local ahora apunta a ${ENV_NAME} (fuente: ${SRC})."
