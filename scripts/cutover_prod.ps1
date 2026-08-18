# cutover_prod.ps1 - lanzador para PowerShell del cutover a produccion.
#
# POR QUE EXISTE: en PowerShell no vale la sintaxis "VAR=valor comando" (es de
# bash), y escribir "bash" a secas resuelve a WSL, que no ve el disco de Windows
# igual y responde "No such file or directory". Este script evita las dos cosas:
# pide los valores, los pasa como variables de entorno, y llama a Git Bash por
# su ruta completa.
#
# OJO AL EDITARLO: este fichero debe quedarse en ASCII puro. PowerShell 5.1 lee
# los .ps1 sin BOM como ANSI, y un guion largo o una tilde en UTF-8 se convierte
# en comillas tipograficas, que PowerShell trata como delimitador de cadena y
# rompen el script entero con un error que no apunta a la linea culpable.
#
# Uso:  .\scripts\cutover_prod.ps1

$ErrorActionPreference = "Stop"

$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

Write-Host ""
Write-Host "=== Cutover de produccion a NeoNet ===" -ForegroundColor Cyan
Write-Host "Repo: $repo"
Write-Host ""

# Git Bash por ruta completa. "bash" a secas se iria a WSL.
$gitBash = "C:\Program Files\Git\bin\bash.exe"
if (-not (Test-Path $gitBash)) {
    $gitBash = "C:\Program Files (x86)\Git\bin\bash.exe"
}
if (-not (Test-Path $gitBash)) {
    Write-Host "No encuentro Git Bash. Instalalo o edita la ruta en este script." -ForegroundColor Red
    exit 1
}

# Merchant ID: es el mismo en sandbox y en produccion.
$merchant = Read-Host "Merchant ID [visanetgt_pull]"
if ([string]::IsNullOrWhiteSpace($merchant)) { $merchant = "visanetgt_pull" }

$keyId = Read-Host "Key ID de PRODUCCION"
if ([string]::IsNullOrWhiteSpace($keyId)) {
    Write-Host "Sin Key ID no se puede continuar." -ForegroundColor Red
    exit 1
}

# El secreto se pide oculto: no queda en pantalla ni en el historial.
$secureSecret = Read-Host "Shared Secret de PRODUCCION" -AsSecureString
$bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureSecret)
$secret = [Runtime.InteropServices.Marshal]::PtrToStringAuto($bstr)
[Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)

if ([string]::IsNullOrWhiteSpace($secret)) {
    Write-Host "Sin Shared Secret no se puede continuar." -ForegroundColor Red
    exit 1
}

$largo = $secret.Length
Write-Host ""
Write-Host "Merchant : $merchant"
Write-Host "Key ID   : $keyId"
Write-Host "Secreto  : (oculto, $largo caracteres)"
Write-Host ""
Write-Host "Esto ESCRIBE en la base de datos de PRODUCCION." -ForegroundColor Yellow
$ok = Read-Host "Escribe SI para continuar"
if ($ok -ne "SI") {
    Write-Host "Cancelado. No se ha tocado nada."
    exit 0
}

$env:MERCHANT_ID   = $merchant
$env:ACCESS_KEY    = $keyId
$env:SHARED_SECRET = $secret

& $gitBash "scripts/cutover_prod.sh"
$code = $LASTEXITCODE

# No dejar el secreto rondando en la sesion de PowerShell.
$env:SHARED_SECRET = ""
$secret = $null

Write-Host ""
if ($code -eq 0) {
    Write-Host "Cutover terminado." -ForegroundColor Green
    Write-Host ""
    Write-Host "Siguiente paso, reiniciar para soltar la cache de credenciales:"
    Write-Host "  flyctl apps restart pull-api-v2-prod"
    Write-Host ""
    Write-Host "Y despues: una compra real pequena, comprobarla en el Business"
    Write-Host "Center de produccion, y reembolsarla."
} else {
    Write-Host "El cutover fallo (codigo $code). NO se ha cambiado la pasarela." -ForegroundColor Red
    Write-Host "La copia de la fila esta en Pull-API-v2/.backups/"
}
