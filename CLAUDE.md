# CLAUDE.md — Pull Events / Aurora Hall demo (MASTER HANDOFF)

**Cómo se usa este archivo:** este es el punto de entrada. El nuevo dev
guarda este archivo en una carpeta vacía (ej. `C:\dev\Pull\CLAUDE.md`),
abre **Claude Code** dentro de esa carpeta, y Claude ejecuta el bootstrap
que hay debajo.

> **NOTA PARA CLAUDE (bootstrap agente):** Si estás leyendo este archivo
> por primera vez y NO hay subcarpetas `Pull-API-v2/`, `PullWebApp-GL/`
> ni `PullMobileApp-GL/` a tu alrededor, entonces esta carpeta es un
> workspace nuevo. Ejecuta la sección **"BOOTSTRAP — primera vez"**
> automáticamente, comando a comando, con la herramienta Bash (o
> PowerShell en Windows). Después continúa con la sección "Después del
> bootstrap".
>
> Si ya existen esas 3 subcarpetas, salta al bloque
> **"Después del bootstrap"**.

---

## Índice

- [BOOTSTRAP — primera vez](#bootstrap--primera-vez)
- [Después del bootstrap](#después-del-bootstrap)
- [Qué es este proyecto](#qué-es-este-proyecto)
- [Inventario](#inventario)
- [Credenciales demo](#credenciales-demo)
- [Cómo trasladar los secretos al `.env`](#cómo-trasladar-los-secretos-al-env)
- [Smoke test — verificar que todo funciona](#smoke-test--verificar-que-todo-funciona)
- [Deploy flows](#deploy-flows)
- [Debug playbook](#debug-playbook)
- [Bug hunt — 12 root causes ya arreglados](#bug-hunt--12-root-causes-ya-arreglados)
- [TODO priorizado — pendientes de la demo](#todo-priorizado--pendientes-de-la-demo)
- [Arquitectura (diagrama + multi-tenant)](#arquitectura-diagrama--multi-tenant)
- [Anti-patrones](#anti-patrones)

---

## BOOTSTRAP — primera vez

Objetivo: dejar la carpeta con los 3 repos clonados, dependencias
instaladas, y una `.env` lista para llenar.

### Paso 0 — sanity check de tooling

Ejecuta este bloque. Si algo falla, avisa al usuario y para.

```powershell
git --version           # 2.40+
gh --version            # 2.x+
node --version          # v20.x
npm --version
go version              # 1.21+
flyctl version
python --version        # 3.10+
```

Si falta alguno, instala con winget (PowerShell como admin):

```powershell
winget install Git.Git -e
winget install GitHub.cli -e
winget install OpenJS.NodeJS.LTS -e
winget install GoLang.Go.1.21 -e
winget install Python.Python.3.12 -e
winget install Fly.Flyctl -e
# Fallback Fly:
#   iwr https://fly.io/install.ps1 -useb | iex

npm install -g eas-cli
npm install -g wrangler
```

**Reinicia la terminal** después de instalar para que el PATH refresque.

### Paso 1 — autenticación

Estas son interactivas (abren navegador). El usuario debe estar delante:

```powershell
gh auth login
# GitHub.com → HTTPS → Yes → Login with a web browser → copia código

flyctl auth login
# Cuenta greenlock.tech (o la que use el usuario)

# Solo si va a hacer deploys de WebApp:
wrangler login

# Solo si va a hacer EAS builds:
eas login
```

**Checkpoint:** ejecuta y confirma:

```powershell
gh auth status         # debe listar "Logged in to github.com"
flyctl auth whoami     # debe imprimir el email
```

Si alguno falla, para y avisa al usuario.

### Paso 2 — clonar los 3 repos activos

Desde la carpeta donde está este CLAUDE.md:

```bash
git clone https://github.com/GreenLock-Cybersecurity/Pull-API-v2.git
git clone -b dev https://github.com/GreenLock-Cybersecurity/PullWebApp-GL.git
git clone https://github.com/GreenLock-Cybersecurity/PullMobileApp-GL.git
```

Opcional (marketing site limpio, no se toca):

```bash
git clone https://github.com/GreenLock-Cybersecurity/Pull-Landing.git
```

**NO clonar** `Pull-API-Go` (legacy) ni `PullClientDashboard` (fuera del
demo) a menos que el usuario lo pida explícitamente.

**Checkpoint:** confirma que existen las 3 carpetas y sus `.git`:

```bash
ls -d Pull-API-v2 PullWebApp-GL PullMobileApp-GL
ls Pull-API-v2/.git PullWebApp-GL/.git PullMobileApp-GL/.git
```

### Paso 3 — instalar dependencias

```bash
cd Pull-API-v2 && go mod download && cd ..
cd PullWebApp-GL && npm install && cd ..
cd PullMobileApp-GL && npm install && cd ..
```

Node install en frontends puede tardar 2-5 min. Es normal.

**Checkpoint — la build del backend compila:**

```bash
cd Pull-API-v2 && go build ./... && echo OK && cd ..
```

Si falla aquí, hay un bug en el repo — muestra el error al usuario. No
sigas.

### Paso 4 — preparar `.env` files

**Backend (los secretos NO están en git — el usuario los trae):**

```bash
cp Pull-API-v2/.env.example Pull-API-v2/.env
```

Ahora el usuario tiene que rellenar los valores. Tres opciones:

**Opción A — copia el `.env` del ordenador viejo** (Signal, Bitwarden
Send, 1Password shared item — canal cifrado). Reemplaza el `.env` recién
creado.

**Opción B — `flyctl ssh` en la máquina de producción**:

```bash
flyctl ssh console --app pull-api-v2-demo -C "printenv" > /tmp/prod-env
grep -E "^(JWT_SECRET|APP_KEY|CENTRAL_|DEFAULT_|BREVO_|RESEND_)" /tmp/prod-env
# Copia esos valores al Pull-API-v2/.env
```

**Opción C — regenerar desde dashboards** (Supabase, Brevo, Resend).
Fine para las keys de servicios, pero `JWT_SECRET` y `APP_KEY` NO deben
regenerarse (rotarlos invalida todos los JWT emitidos y hace ilegibles
las credenciales encriptadas de venue).

**Frontends (no llevan secretos, apuntan a la demo pública):**

```bash
# WebApp
cat > PullWebApp-GL/.env <<'EOF'
VITE_API_URL=/api/v1
VITE_DEFAULT_VENUE_SLUG=aurora-hall
EOF

# Mobile
cat > PullMobileApp-GL/.env <<'EOF'
EXPO_PUBLIC_API_URL=https://aurora-hall.pages.dev/api/v1
EOF
```

**Checkpoint — arranca el backend contra la demo**:

```bash
cd Pull-API-v2
go run main.go &     # background para que el terminal no se bloquee
sleep 5
curl -sf http://localhost:8080/health && echo "BACKEND OK LOCAL"
# Después mata el proceso — no lo dejes corriendo
```

Si dice "Central database: Connected" y "Default venue database:
Connected" en los logs, `.env` está bien.

Si dice "Failed to connect", el `.env` está mal — vuelve a las 3
opciones.

### Paso 5 — smoke test end-to-end

Ejecuta el smoke test de la sección
[Smoke test](#smoke-test--verificar-que-todo-funciona) más abajo. Si
todo pasa, el bootstrap ha terminado.

**Después de este paso, avisa al usuario que estás listo y muéstrale un
resumen de lo que has hecho + los 3 URLs vivos (Fly, WebApp, mobile
demo). Ofrécele coger el primer P0 del [TODO](#todo-priorizado--pendientes-de-la-demo).**

---

## Después del bootstrap

Si los 3 repos ya existen a tu alrededor, esto es lo que necesitas leer
antes de tocar código.

**Prioridad de lectura:**

1. Este archivo (secciones abajo) — contexto, credenciales, deploys.
2. `Pull-API-v2/HANDOFF.md` — log detallado de bugs cazados con root
   cause. Impide reincidir en errores de shape.
3. `Pull-API-v2/TODO.md` — backlog priorizado. Coge el top de P0/P1.
4. `Pull-API-v2/ARCHITECTURE.md` — diagrama del sistema, multi-tenant.

Si esos docs contradicen el código, **confía en el código** y actualiza
los docs después de resolver la confusión.

---

## Qué es este proyecto

**Aurora Hall** es la demo pública de la plataforma multi-tenant Pull
Events. Tres piezas en producción:

- **Backend** `Pull-API-v2` — Go/Gin en Fly.io.
- **WebApp cliente** `PullWebApp-GL` — React/Vite en Cloudflare Pages.
- **App móvil staff** `PullMobileApp-GL` — Expo/React Native, en Expo Go
  ahora, EAS build pendiente.

Todo el tráfico pasa por:

```
https://aurora-hall.pages.dev/api/v1    (Cloudflare Pages Function proxy)
        ↓
https://pull-api-v2-demo.fly.dev/api/v1 (Fly.io backend Go)
```

Venue ficticio "Aurora Hall" fijo (sin venue picker). Pagos simulados
por `services/mock_processor.go` (`DEMO_MODE=true`).

---

## Inventario

| Proyecto | Tipo | Stack | GitHub | Branch |
|---|---|---|---|---|
| **Pull-API-v2** | Backend (ACTIVO) | Go 1.21, Gin | [Pull-API-v2](https://github.com/GreenLock-Cybersecurity/Pull-API-v2) | main |
| **PullWebApp-GL** | WebApp cliente (ACTIVO) | React 19, Vite, TS | [PullWebApp-GL](https://github.com/GreenLock-Cybersecurity/PullWebApp-GL) | **dev** |
| **PullMobileApp-GL** | App móvil staff (ACTIVO) | Expo 54, RN 0.81 | [PullMobileApp-GL](https://github.com/GreenLock-Cybersecurity/PullMobileApp-GL) | main |
| Pull-API-Go | **LEGACY, NO tocar** | Go 1.24, Gin | [Pull-API-Go](https://github.com/GreenLock-Cybersecurity/Pull-API-Go) | main |
| PullClientDashboard | Fuera del demo | React 19 | Sin repo | — |
| Pull-Landing | Marketing (untouched) | React 19, Tailwind | [Pull-Landing](https://github.com/GreenLock-Cybersecurity/Pull-Landing) | main |

`Pull-API-Go` se conserva solo por las plantillas HTML de email — v2 las
importa con `//go:embed`. Cualquier otro cambio ahí no llega a
producción.

`PullClientDashboard` apunta a `api.pullevents.com` que no forma parte
de la demo Aurora Hall. Ignóralo.

---

## Credenciales demo

**Staff (mobile app / cualquier endpoint staff):**

```
Email:    demo@aurorahall.com
Password: DemoStaff2026!
Rol:      admin (todos los permisos)
Venue:    Aurora Hall
```

**WebApp cliente:** sin login, checkout anónimo.

**Secretos de producción:** en `Pull-API-v2/.env` (gitignored). Ver
sección siguiente.

---

## Cómo trasladar los secretos al `.env`

**Los secretos NUNCA están en git.** El `.env.example` documenta cada
variable y dónde obtenerla. Aquí el resumen práctico:

| Variable | De dónde | Notas |
|---|---|---|
| `JWT_SECRET` | Fly SSH o `.env` viejo | 32+ chars. Rotar = todos los usuarios logout. |
| `APP_KEY` | Fly SSH o `.env` viejo | 64 hex chars. Rotar = credenciales de venue ilegibles. |
| `CENTRAL_SUPABASE_URL` | Supabase → proyecto `dqqvtehpidihahzabcxg` → Settings → API | Solo URL, no secreto. |
| `CENTRAL_SERVICE_KEY` | Supabase mismo → `service_role` | **Secreto.** |
| `CENTRAL_ANON_KEY` | Supabase mismo → `anon` | Public-safe. |
| `DEFAULT_SUPABASE_URL` | Supabase → proyecto `oqqhffxwiizukkevzkvz` (Aurora Hall) → Settings → API | Solo URL. |
| `DEFAULT_SERVICE_KEY` | Mismo → `service_role` | **Secreto.** |
| `DEFAULT_ANON_KEY` | Mismo → `anon` | Public-safe. |
| `BREVO_API_KEY` | https://app.brevo.com/settings/keys/api | `xkeysib-...`. Regenerable. |
| `BREVO_FROM_EMAIL` | Constante | `Pull Events <noreply@tickets.pullevents.com>` |
| `RESEND_API_KEY` | https://resend.com/api-keys | Opcional (Brevo es primario). |
| `STRIPE_*` | Vacío en demo (DEMO_MODE=true) | Skip. |

**3 formas de obtenerlos**, ordenadas por rapidez:

1. **Copia `.env` viejo** por canal cifrado (Signal / Bitwarden Send /
   1Password shared). Es lo más rápido.
2. **`flyctl ssh console --app pull-api-v2-demo -C "printenv"`** —
   recupera TODO desde la VM en producción.
3. **Regenerar desde dashboards** — solo válido para
   Supabase/Brevo/Resend. NO regeneres `JWT_SECRET` ni `APP_KEY`.

---

## Smoke test — verificar que todo funciona

Después del bootstrap, ejecuta esto:

```bash
# 1. Backend en producción responde
curl -sf https://pull-api-v2-demo.fly.dev/health && echo "BACKEND OK" || echo "BACKEND DOWN"

# 2. Login funciona → obtiene token
API="https://aurora-hall.pages.dev/api/v1"
TOKEN=$(curl -s -X POST "$API/auth/login-staff" \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@aurorahall.com","password":"DemoStaff2026!"}' \
  | python -c "import sys,json; print(json.load(sys.stdin).get('token',''))")
echo "TOKEN: ${TOKEN:0:30}..."

# 3. JWT decodes con TODOS los campos
echo "$TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | python -m json.tool
# Debe incluir: user_id, employee_id, email, name, venue_id,
# organization_id, role, type=venue_staff

# 4. Endpoints protegidos responden 200
for path in \
  "/event/upcoming-events/8450e956-8805-44c2-9ae5-d91b70d835ad" \
  "/orders/venue/8450e956-8805-44c2-9ae5-d91b70d835ad?status=pending" \
  "/employees/employees" \
  "/venue/get-venue-info/8450e956-8805-44c2-9ae5-d91b70d835ad"; do
  code=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$API$path")
  printf "  %-70s HTTP %s\n" "$path" "$code"
done

# 5. Backend local compila
cd Pull-API-v2 && go build ./... && echo "BUILD OK LOCAL" && cd ..

# 6. WebApp local buildea
cd PullWebApp-GL && npm run build 2>&1 | tail -3 && cd ..

# 7. Mobile expo-doctor
cd PullMobileApp-GL && npx expo-doctor 2>&1 | tail -3 && cd ..
```

Todo verde = listo. Cualquier rojo = arréglalo antes de tocar features.

---

## Deploy flows

> **⚠️ ACTUALIZADO 2026-07-22 — tres entornos.** La fuente de verdad es
> `ENVIRONMENTS.md` en la raíz del workspace. Resumen:
>
> - **PRODUCCIÓN 511 Events** (dinero real): `bash scripts/deploy_prod.sh`
>   — exige rama main + árbol limpio + push, y tagea `prod-*`.
> - **STAGING 511** (Cybersource sandbox): `bash scripts/deploy_staging.sh`.
> - **DEMO Aurora** (lo de abajo): solo con `-c fly.demo.toml`, y solo si
>   de verdad quieres tocar la demo de Diego. `fly.toml` YA NO EXISTE:
>   `flyctl deploy` a secas falla a propósito.

### Backend (Fly.io) — DEMO Aurora únicamente

```bash
cd Pull-API-v2
go build ./...                                       # sanity local
flyctl deploy -c fly.demo.toml --remote-only --strategy immediate
flyctl status --app pull-api-v2-demo                 # STARTED + passing?
```

Si la máquina termina STOPPED después del deploy:

```bash
flyctl machine start <machine-id> --app pull-api-v2-demo
```

Suele significar que la build subió pero la app entró en panic al
arrancar (route duplicada, typo en struct). Ver logs.

### WebApp (Cloudflare Pages)

> Producción 511: `bash scripts/deploy_prod.sh` (proyecto
> `pull-511-events`, rama main). Staging: `bash scripts/deploy_staging.sh`.
> Lo de abajo es la DEMO.

Auto-deploy de la DEMO en cada push a `dev` del repo **GreenLock**
(nuestro origin es el fork DamiPerezz: pushear ahí NO toca la demo).
Manual demo:

```bash
cd PullWebApp-GL
npm run build
wrangler pages deploy dist --project-name aurora-hall
```

El proxy Function `functions/api/[[path]].js` viaja con cada build.

### Móvil (EAS → TestFlight)

Ver `PullMobileApp-GL/BUILD_INSTRUCTIONS.md`. Resumen:

```bash
cd PullMobileApp-GL
eas login
eas build --platform ios --profile production
eas submit --platform ios --profile production --latest
```

- Bundle id: `com.pullevents.staff`
- EAS project id: `cc92c30d-3724-45c7-913f-6774f3a1ebfb`
- Push notifications requieren build EAS real (no funcionan en Expo Go).

---

## Debug playbook

### Leer logs vivos

```bash
# Últimas 60 líneas, todo el tráfico
flyctl logs --app pull-api-v2-demo | tail -60

# Solo 4xx/5xx (excluye 401 que es esperado en flows de auth)
flyctl logs --app pull-api-v2-demo | tail -200 \
  | grep -E "GIN.*(4[0-9]{2}|5[0-9]{2})" | grep -v 401

# Por request-id (aparece en el body del error)
flyctl logs --app pull-api-v2-demo | grep "<request-id>"
```

### Verificar shape wire desde CLI

```bash
API="https://aurora-hall.pages.dev/api/v1"
TOKEN=$(curl -s -X POST "$API/auth/login-staff" \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@aurorahall.com","password":"DemoStaff2026!"}' \
  | python -c "import sys,json; print(json.load(sys.stdin).get('token',''))")

curl -s -H "Authorization: Bearer $TOKEN" "$API/<PATH>" | python -m json.tool | head
```

### Errores comunes de Supabase (PostgreSQL)

| Código | Significado | Acción |
|---|---|---|
| `42703` | Columna no existe | Cambiar `select` o corregir schema |
| `22P02` | Enum inválido | `reservation_status`: `pending\|confirmed\|closed\|completed\|cancelled`. `order_status`: `pending\|processing\|confirmed\|failed\|cancelled` |
| `23505` | Unique constraint violada | Buscar el índice y ver qué row colisiona |
| `23503` | Foreign key violada | El id que insertaste no existe en la tabla referenciada |

---

## Bug hunt — 12 root causes ya arreglados

> **ACTUALIZACIÓN 2026-07-09:** hay 15 root causes MÁS (#13-27) cazados en
> la sesión de verificación total — ver `HANDOFF.md` sección "Bug hunt log".
> Los patrones que más se repiten (memorízalos):
>
> 1. **Columnas imaginarias.** Los handlers se escribieron contra un schema
>    que no existe. Columnas reales: `events` no tiene `venue_id/
>    organization_id/custom_location/table_capacity` (usa `location`,
>    `capacity`); `ticket_types` usa `quantity_total` (no `quantity`) y
>    `benefits` es `text[]`; `guest_list_types` usa `max_signups/
>    current_signups` (no `max_capacity/current_count`) y no tiene
>    `venue_id/updated_at`; `guest_list_signups` no tiene `venue_id/
>    updated_at/checked_in/plus_ones_used/rejected_*`; `orders` no tiene
>    `rejected_by`. La tenancy ES la base de datos por venue.
> 2. **Params de ruta.** Las rutas móviles registran camelCase (`:eventId`,
>    `:typeId`, `:signupId`) y los handlers leían `c.Param("id")` — 3
>    incidencias del mismo bug. Lee siempre ambos.
> 3. **Double-unwrap en el móvil.** Servicios que devuelven `response.data`
>    y pantallas que hacen `.data` encima (tickets, signups). Normaliza en
>    el servicio.
> 4. **`Alert.alert` es no-op en RN-web** — shim en
>    `PullMobileApp-GL/utils/webAlert.js`.
> 5. **VIP list flow RETIRADO** (decisión de producto 2026-07). La app lo
>    fuerza a false en `authService`; queda pendiente poner
>    `venues.use_vip_list_flow=false` en la DB central.
>
> Flujos verificados end-to-end (UI web de la app + API): login/reload,
> EventoDetalle (tickets+grupos+listas), editar evento, CRUD tickets, CRUD
> listas, aprobar/rechazar orders (con tickets+email), aprobar/rechazar
> grupos (con email de link de pago), aprobar/rechazar signups (con QR),
> tracking web del grupo con pago por invitado, empleados, notificaciones,
> logout.

Léelo antes de tocar cualquier cosa. No reincidas en estos:

**Backend / mobile compat layer:**

1. **`MobileApproveGroupReservation` 500** — enviaba `status="approved"`
   pero el enum `reservation_status` solo acepta
   `pending|confirmed|closed|completed|cancelled`. Fix en
   `controllers/mobile_compat_controller.go:MobileApproveGroupReservation`.

2. **`GetVenuePendingSignups` 500** — filtraba
   `guest_list_signups.venue_id` pero esa columna no existe. Fix:
   buscar events del venue primero, después
   `event_id in.(id1,id2,...)`. Ver `guestlist_controller.go`.

3. **`GetEventGuestLists` 404 "Event not found"** — leía
   `c.Param("event_id")` pero la ruta registra `:eventId` (camelCase).
   Ahora lee ambos.

4. **`MobileGetEmployees` devolvía `null`** — seleccionaba `role`
   (no existe); la columna real es `role_id`. También faltaba filtro
   `deleted_at is null`. Añadido lookup `role_id → role.name` para que
   la UI agrupe por role string.

5. **JWT sin `organization_id/employee_id/email/name`** — login usaba
   `GenerateStaffTokenSimple` que solo aceptaba id+venue+role. Ahora usa
   `GenerateStaffToken` con `Staff{}` completo. Añadido `EmployeeID`
   como alias de `UserID` en `JWTClaims` porque la app decodifica
   `.employee_id`.

6. **`/event/upcoming-events` shape** — devolvía `{count, events:[...]}`
   pero la app espera **array directo** (`response.data` va a la store
   tal cual). Fix: devolver `[...]` a secas.

7. **`/orders/venue` shape** — devolvía `{orders, count, page, limit}`.
   La app espera
   `{orders, pagination:{currentPage, totalPages, totalCount, hasMore, limit}}`.
   Añadida query separada para `totalCount`.

8. **`/guest-lists/types/event/:eventId` wrapper** — devolvía
   `{data:[...]}`. `guestListService` lee `response.data` como array
   crudo. Cambiado a array plano.

9. **`/event/get-event-details` wrapper** — devolvía `{event: {...}}`.
   `eventService` lee `response.data.name` etc. top-level. Cambiado a
   objeto plano.

10. **`/ticket-types/*` no existía** — añadidos los 4 endpoints
    (GET/POST/PUT/DELETE) en `mobile_compat_controller.go`
    (`MobileGetTicketTypesByEvent`, `MobileCreateTicketType`,
    `MobileUpdateTicketType`, `MobileDeleteTicketType`).

11. **CRUD eventos no existía** — añadidos `MobileCreateEvent`,
    `MobileCreateEventWithTickets`, `MobileUpdateEvent`,
    `MobileDeleteEvent`. Helpers `combineDateTime` (fecha + start +
    end → RFC3339 con `-06:00` Guatemala; rollover a día siguiente si
    end < start) y `slugify`.

12. **`verify-token` shape para recarga móvil** — devolvía
    `{valid, staff, venue}`. La app busca `response.data.type === 'jwt'`
    y lee de `.claims`. Fix:
    `{valid, type:"jwt", claims:{employee_id, email, organization_id,
    venue_id, venue_name, venue_slug, venue_currency, use_vip_list_flow,
    role, name}, staff, venue}`. Middleware `AuthenticateStaff` ahora
    también setea `name` en el context. **Primera deploy de este fix
    falló silenciosamente** porque el campo del struct es
    `venue.UseVipListFlow` (lowercase p), no `UseVIPListFlow`. `go build`
    local pasó por cache, Fly remote rechazó.

**WebApp cliente (arreglado en sesiones previas, ya en `dev`):**

- Campo Instagram (opcional) en los 3 flujos (individual/grupo/lista)
  con los mismos estilos form-field.
- Reserva grupo: nombre + descripción dentro de "Configuración del
  Grupo", con defaults sensatos.
- Precios mesa grupo: Q400 hombre / Q250 mujer (era 1 precio mal).
- Quitado el card duplicado dorado con corona en event-detail. Solo
  queda el azul MESA PREMIUM.
- Email reserva grupo usa plantilla v1 `group_reservation_pending.html`.
- PDF adjunto vía Brevo — clave `attachment` (singular) + QR 8-bit
  NRGBA (gofpdf no acepta PNG 16-bit de boombuler).
- Tracking link funciona — `management_code` y `payment_link_code`
  unificados a un `sharedCode` (12 chars).

---

## TODO priorizado — pendientes de la demo

### P0 — verifica esto en cuanto abras la app móvil

- [x] **Recargar la app SIN cerrar sesión** — VERIFICADO (2026-07-09).
- [x] **EventoDetalle** — VERIFICADO: foto, nombre, fecha, descripción,
      tickets con disponibilidad, grupos (mesas con precio M/F) y listas.
- [x] **Orders tab** — VERIFICADO con filtros, búsqueda y detalle.
- [ ] **Corregir dato central**: `venues.use_vip_list_flow=true` es config
      stale (el flujo VIP list está RETIRADO). La app ya lo ignora, pero:
      `PATCH <central>/rest/v1/venues?id=eq.8450e956-... {"use_vip_list_flow": false}`.

### P1 — pantallas que NUNCA se probaron desde UI

- [~] **EventoNuevo** — backend verificado vía API (arreglado: columnas
      fantasma, crea como `published`). El wizard de UI queda pendiente de
      probar en dispositivo (image picker nativo) y `POST
      /upload/event-image` sigue siendo stub (URL placeholder Unsplash).
- [x] **EventoEditar** — VERIFICADO desde UI (2026-07-09).
- [x] **Borrar evento** — VERIFICADO vía API (soft delete + undelete).
- [x] **TicketsGestion** — VERIFICADO: UI + CRUD completo (regular y
      grupo/mesa VIP).
- [ ] **EmpleadoNuevo / EmpleadoEditar** — **SIGUEN SIN endpoints
      `POST /employees/create`, `PUT /employees/:id`, `DELETE`**. Sólo
      GET list y GET by id. Añadir cuando alguien pruebe.
- [x] **ReservaDetalle** — VERIFICADO + approve (pipeline completo:
      tickets + email) y reject desde UI.
- [x] **GroupReservaDetalle** — VERIFICADO + approve desde UI (envía email
      con link de pago del grupo).
- [x] **GuestListDetalle** — VERIFICADO + approve (QR + email) y reject.
- [x] ~~**VIPListDetalle / VIPListNuevo**~~ — RETIRADO (producto). Pantallas
      muertas, candidatas a borrar.
- [ ] **Scanner QR real** — necesita ticket real y cámara física. Compra
      por WebApp, abre PDF del email, scanea QR desde la app en dispositivo.

### P2 — polish / smells

- [x] `PullMobileApp-GL/app/(tabs)/EventosList/index.js.backup` borrado
      (2026-07-09).
- [x] `PullMobileApp-GL/expo.log` en gitignore (2026-07-09).
- [ ] `PullWebApp-GL` — 62 alertas dependabot en dev (33 high). Correr
      `npm audit fix` y PR.
- [ ] Añadir `.env.production` al gitignore mobile — que EAS Build lea
      env vars desde su config, no del repo.
- [ ] Añadir GitHub Action para `go build ./...` + `go vet` en cada PR
      de Pull-API-v2. Test coverage en Pull-API-v2 es 0 — no vamos a
      arreglar ahora, pero por eso "build local ok" no cazó el
      `UseVIPListFlow`.

### P3 — legacy cleanup

- [ ] Añadir `DEPRECATED.md` al raíz de `Pull-API-Go` que apunte a
      Pull-API-v2.
- [ ] Decidir qué hacer con `PullClientDashboard` — borrar o wire a
      la demo.

### P4 — nice-to-have

- [ ] Mover `.env.example` values a Doppler / 1Password shared vault.
- [ ] `cmd/seed/main.go` — genera N orders pending, 1 signup pending,
      1 group reservation pending para hacer testing rápido del lado
      staff.
- [ ] Verificar `fly.toml [http_service] auto_stop_machines = "off"`
      para que la máquina no se pare por inactividad (ya está en off,
      confirmar).

---

## Arquitectura (diagrama + multi-tenant)

```
┌─────────────────────────────────────────────────────────────────────┐
│                            USUARIOS                                 │
├──────────────┬────────────────────────┬─────────────────────────────┤
│ Clientes     │ Staff venue (móvil)    │ Admins plataforma (unused)  │
└──────┬───────┴────────────┬───────────┴──────────────┬──────────────┘
       │                    │                          │
       ▼                    ▼                          ▼
┌──────────────┐    ┌──────────────────┐      ┌──────────────────────┐
│ PullWebApp   │    │ PullMobileApp-GL │      │ PullClientDashboard  │
│ -GL          │    │ (Expo/RN)        │      │ NO USADO EN DEMO     │
│ Cloudflare   │    │ Expo Go / EAS    │      │ api.pullevents.com   │
│ Pages        │    │ build            │      │                      │
└──────┬───────┘    └──────────┬───────┘      └──────────────────────┘
       │                       │
       │  /api/v1/*            │  /api/v1/*
       ▼                       ▼
┌──────────────────────────────────────────┐
│  Cloudflare Pages Function (proxy)       │
│  aurora-hall.pages.dev/api/[[path]]      │
│  - Bypass fly.dev DNS blocks             │
│  - Same origin → no CORS                 │
└──────────────┬───────────────────────────┘
               ▼
┌──────────────────────────────────────────┐
│  Pull-API-v2 (Go/Gin)                    │
│  Fly.io: pull-api-v2-demo                │
│  - controllers/ (auth, event, order, ...)│
│  - legacy_compat_controller.go   ← webApp v1 paths
│  - mobile_compat_controller.go   ← mobile v1 paths
│  - services/database_router.go   ← multi-tenant
└──────────────┬───────────────────────────┘
               │
       ┌───────┴───────┐
       ▼               ▼
┌──────────────┐   ┌──────────────┐
│ Central DB   │   │ Venue DB(s)  │
│ Supabase     │   │ Supabase     │
│ - venues     │   │ - events     │
│ - venue_db_  │   │ - orders     │
│   configs    │   │ - tickets    │
│   (encrypted)│   │ - staff      │
│              │   │              │
│ 1 instancia  │   │ 1 POR VENUE  │
└──────────────┘   └──────────────┘
```

**Multi-tenant:**

- **Central DB**: `venues`, `venue_database_configs` (con credenciales
  Supabase por venue cifradas AES-256-GCM), `pull_staff`.
- **Por venue**: events, orders, tickets, staff (`organization_workers`),
  guest signups, VIP reservations.
- Aurora Hall usa el venue en `oqqhffxwiizukkevzkvz.supabase.co`
  (`DEFAULT_SUPABASE_URL`).
- Router: `services.DB.Central()` vs `services.DB.ForVenue(venueID)`.

**JWT claims** (`v47` en adelante):

```json
{
  "iss": "pull-api-v2",
  "exp": <unix>,
  "iat": <unix>,
  "user_id":         "5032b851-...",
  "employee_id":     "5032b851-...",
  "email":           "demo@aurorahall.com",
  "name":            "Demo Admin",
  "venue_id":        "8450e956-...",
  "organization_id": "74f2fa79-...",
  "role":            "admin",
  "type":            "venue_staff"
}
```

**Compat layers:**

- `controllers/legacy_compat_controller.go` — mapea paths v1 del WebApp
  a lógica v2 (`/orders/create-pending-order`, `/orders/simulate-payment`,
  `/group-reservations/create`, `/guest-lists/signup`,
  `/event/get-detailed-event-info/:slug`, etc.).
- `controllers/mobile_compat_controller.go` — igual pero para la app
  móvil (approve/reject orders, group-reservations, guest-list; CRUD
  events, ticket-types, employees; push token register/unregister).

---

## Anti-patrones

Aprendidos a las malas:

- **NO tocar `Pull-API-Go`** salvo plantillas HTML de email. No está
  deployed en ningún sitio; los cambios no llegan a producción y
  confunden al siguiente dev.
- **NO tocar `PullClientDashboard`** para la demo Aurora. Otro scope.
- **NO `git add -A`** en `Pull-API-v2` sin verificar que `.env` no está
  staged. El `.gitignore` cubre pero sé paranoide.
- **NO borrar datos demo como smoke test.** Yo hice
  `DELETE /event/delete-event/:id` sobre Aurora Friday Nights y tuve
  que restaurarlo con `PUT status=published, deleted_at=null`. Prueba
  destructivo contra un evento throwaway.
- **NO confiar en "deploy succeeded".** El builder remoto de Fly puede
  fallar silenciosamente donde `go build` local pasó por cache (ver
  `UseVIPListFlow` / `UseVipListFlow`). Siempre verificar el shape
  del endpoint que tocaste con `curl`.
- **NO añadir rutas a `setupMobileRoutes`** sin comprobar si el mismo
  path ya está en otra sección de `main.go`. Duplicar
  `GET /guest-lists/venue/:venueId/pending` panic'ó el boot y dejó la
  máquina STOPPED.
- **NO usar `select "*"`** ciego en tablas con columnas enum nuevas.
  `EnrichEvent` tropezó con eso en un momento. Prefiere selects
  explícitos y añade columnas cuando las necesites.

---

## Contacto & ownership

- Org GitHub: `GreenLock-Cybersecurity`
- Dev principal: `diego.rodriguez@greenlock.tech`
- Cuenta Fly.io: mismo email
- Proyectos Supabase, Brevo, Cloudflare: mismo team account

Si dudas si un cambio es "authorized to ship", pregunta al usuario. La
demo es pública y los clientes la ven — una demo rota es peor que una
feature retrasada.
