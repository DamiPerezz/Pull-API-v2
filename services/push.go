package services

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// =============================================
// FIREBASE CLOUD MESSAGING (FCM HTTP v1)
// Sends push notifications straight to Android devices — no Expo push
// service in the middle. Devices register their native FCM token (the app
// calls getDevicePushTokenAsync) into staff_push_tokens; here we exchange a
// Firebase service account for an OAuth token and POST to the FCM v1 API.
//
// Config: FCM_SERVICE_ACCOUNT_JSON env var holds the whole service-account
// JSON (set as a Fly secret). If unset, push is a no-op (logged once).
// =============================================

type fcmServiceAccount struct {
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// PushService holds the FCM client state.
type PushService struct {
	sa         *fcmServiceAccount
	privateKey *rsa.PrivateKey
	client     *http.Client

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// Push is the global push service instance (nil until InitPushService).
var Push *PushService

// InitPushService initializes FCM from FCM_SERVICE_ACCOUNT_JSON.
//
// TRAMPA (esto dejaría los iPhone mudos sin una sola línea de error): `Push`
// tiene que quedar SIEMPRE distinto de nil, aunque FCM no esté configurado. Los
// tres sitios que notifican (order_controller, pay_controller,
// legacy_compat_controller) hacen `if services.Push != nil` antes de llamar. Si
// `Push` fuera nil por faltar FCM_SERVICE_ACCOUNT_JSON, NUNCA se entraría a
// NotifyVenueStaff — y por tanto tampoco a la rama de APNs — con las 4 APNS_*
// bien puestas y el arranque diciendo "[APNs] iOS push inicializado". iOS es una
// plataforma independiente de Android: no puede depender de que exista el
// secreto de la otra. Por eso el service existe siempre y es cada bloque (FCM /
// APNs) el que decide si sabe enviar.
func InitPushService() {
	Push = &PushService{client: &http.Client{Timeout: 10 * time.Second}}

	raw := os.Getenv("FCM_SERVICE_ACCOUNT_JSON")
	if raw == "" {
		log.Printf("[Push] FCM_SERVICE_ACCOUNT_JSON not set — Android push desactivado (iOS/APNs va por su cuenta)")
		return
	}
	var sa fcmServiceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		log.Printf("[Push] invalid FCM service account JSON: %v", err)
		return
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		log.Printf("[Push] could not decode FCM private key PEM")
		return
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			key = rk
		}
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	}
	if key == nil {
		log.Printf("[Push] FCM private key is not a supported RSA key")
		return
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	Push.sa = &sa
	Push.privateKey = key
	log.Printf("[Push] FCM push initialized (project=%s)", sa.ProjectID)
}

// fcmReady indica si la parte de Android está utilizable. Ojo: `Push` puede ser
// no-nil y esto devolver false — es justo el caso de "solo iOS configurado"
// (ver InitPushService). Todo lo que toque p.sa o p.privateKey tiene que pasar
// por aquí antes, o es un nil deref.
func (p *PushService) fcmReady() bool {
	return p != nil && p.sa != nil && p.privateKey != nil
}

// getAccessToken returns a cached (or freshly minted) OAuth token for FCM.
func (p *PushService) getAccessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accessToken != "" && time.Now().Before(p.tokenExpiry.Add(-1*time.Minute)) {
		return p.accessToken, nil
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   p.sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud":   p.sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(p.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	form := "grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=" + signed
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.sa.TokenURI, bytes.NewReader([]byte(form)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode oauth: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("empty access token (HTTP %d)", resp.StatusCode)
	}
	p.accessToken = out.AccessToken
	p.tokenExpiry = now.Add(time.Duration(out.ExpiresIn) * time.Second)
	return p.accessToken, nil
}

// sendOne posts a single message to FCM. Returns the HTTP status so callers
// can prune tokens that FCM reports as unregistered (404/400).
func (p *PushService) sendOne(ctx context.Context, accessToken, token, title, body, channelID string, data map[string]interface{}) (int, error) {
	// FCM data values must be strings.
	strData := map[string]string{}
	for k, v := range data {
		strData[k] = fmt.Sprintf("%v", v)
	}
	msg := map[string]interface{}{
		"message": map[string]interface{}{
			"token": token,
			"notification": map[string]interface{}{
				"title": title,
				"body":  body,
			},
			"data": strData,
			"android": map[string]interface{}{
				"priority": "high",
				"notification": map[string]interface{}{
					"channel_id": channelID,
					"sound":      "default",
				},
			},
		},
	}
	payload, _ := json.Marshal(msg)
	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", p.sa.ProjectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// NotifyVenueStaff sends a push to every active staff device of a venue.
// Fire-and-forget: errors are logged, dead tokens are deactivated.
func (p *PushService) NotifyVenueStaff(ctx context.Context, venueID, title, body, channelID string, data map[string]interface{}) {
	// Ahora que Push existe siempre (ver InitPushService), hay que cortar aquí
	// cuando NINGUNA plataforma está configurada — típico en local — o cada
	// compra pagaría una consulta a Supabase para no enviar nada.
	if !p.fcmReady() && !apnsEnabled() {
		return
	}
	venueDB := DB.ForVenue(venueID)
	if venueDB == nil {
		log.Printf("[Push] NotifyVenueStaff: invalid venue %s", venueID)
		return
	}
	// device_type es OBLIGATORIO en el select: es lo único que separa iOS de
	// Android. Si se cae de aquí, GetString devuelve "" y TODOS los tokens se
	// irían por FCM — que no sabe entregar a un device token de APNs, así que
	// los iPhone dejarían de recibir sin ningún error visible.
	rows, err := venueDB.QueryCtx(ctx, "staff_push_tokens", map[string]interface{}{
		"select": "push_token,device_type",
		"where":  map[string]interface{}{"is_active": true},
	})
	if err != nil {
		log.Printf("[Push] query tokens failed venue=%s: %v", venueID, err)
		return
	}

	// Separar por plataforma: iOS → APNs directo, resto → FCM. Un mismo token
	// no se envía dos veces (dedup por push_token).
	seen := map[string]bool{}
	var fcmTokens, iosTokens []string
	for _, r := range rows {
		t := GetString(r, "push_token")
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		// Comparación tolerante a mayúsculas y espacios: este valor lo escribe
		// el cliente (MobileRegisterPushToken guarda literal lo que manda la
		// app). Hoy llega "ios" en minúsculas — es Platform.OS de React Native —
		// pero si alguna versión mandara "iOS", ese token se iría por FCM y el
		// iPhone dejaría de recibir SIN ningún error visible. Un fallo mudo así
		// cuesta más de encontrar que lo que cuesta aceptar las dos formas.
		if strings.EqualFold(strings.TrimSpace(GetString(r, "device_type")), "ios") {
			iosTokens = append(iosTokens, t)
		} else {
			fcmTokens = append(fcmTokens, t)
		}
	}
	if len(fcmTokens) == 0 && len(iosTokens) == 0 {
		log.Printf("[Push] no active tokens for venue=%s", venueID)
		return
	}

	deactivate := func(token string) {
		venueDB.UpdateNoReturn(ctx, "staff_push_tokens", map[string]interface{}{
			"is_active": false,
		}, map[string]interface{}{"push_token": token})
	}

	sent := 0

	// --- Android / FCM ---
	// Simétrico al feature-flag de APNs de abajo: si falta
	// FCM_SERVICE_ACCOUNT_JSON, los tokens Android se saltan con una línea de
	// log y los iOS se envían igual. Sin este guard sería un nil deref sobre
	// p.sa dentro de getAccessToken — y en pánico dentro de RunBackground se
	// lleva por delante todo el proceso.
	if len(fcmTokens) > 0 {
		if !p.fcmReady() {
			log.Printf("[Push] %d token(s) Android sin enviar: FCM no configurado (falta FCM_SERVICE_ACCOUNT_JSON)", len(fcmTokens))
		} else if accessToken, err := p.getAccessToken(ctx); err != nil {
			log.Printf("[Push] cannot get FCM access token: %v", err)
		} else {
			for _, token := range fcmTokens {
				status, err := p.sendOne(ctx, accessToken, token, title, body, channelID, data)
				if err != nil {
					log.Printf("[Push] FCM send error: %v", err)
					continue
				}
				if status == http.StatusOK {
					sent++
					continue
				}
				// OJO: aquí vive la misma trampa que en la rama de APNs — un 400
				// de FCM (INVALID_ARGUMENT) puede ser configuración y no un
				// token muerto. Se deja como estaba porque Android lleva meses
				// funcionando así; si algún día se apagan varios Android a la
				// vez, empieza mirando esto (FCM manda el detalle en el cuerpo,
				// que tampoco se está leyendo).
				if status == http.StatusNotFound || status == http.StatusBadRequest {
					deactivate(token)
				}
				log.Printf("[Push] FCM returned HTTP %d for a token", status)
			}
		}
	}

	// --- iOS / APNs (feature-flag: solo si APNS_* está configurado) ---
	// Sin las APNS_* esto no falla ni rompe nada: los tokens iOS simplemente se
	// saltan (una línea de log) y Android ya se ha enviado arriba.
	if len(iosTokens) > 0 {
		if !apnsEnabled() {
			log.Printf("[Push] %d token(s) iOS sin enviar: APNs no configurado (faltan APNS_KEY_P8/KEY_ID/TEAM_ID/BUNDLE_ID)", len(iosTokens))
		} else {
			// Los BadDeviceToken no se resuelven aquí dentro: se apuntan y se
			// deciden al final del lote, cuando ya se sabe si algún envío iOS
			// llegó. El porqué, justo debajo del bucle.
			var suspect []string
			iosOK := 0
			for _, token := range iosTokens {
				status, reason, err := apns.send(ctx, token, title, body, data)
				if err != nil {
					log.Printf("[APNs] send error: %v", err)
					continue
				}
				if status == http.StatusOK {
					sent++
					iosOK++
					continue
				}
				// TRAMPA (ya pisada una vez): NO desactivar por el código HTTP.
				// Apple usa el mismo 400 para "este token no vale"
				// (BadDeviceToken) y para "tu configuración está mal"
				// (TopicDisallowed, InvalidProviderToken...). Desactivando por
				// el 400 a secas, un APNS_BUNDLE_ID equivocado apagaría de golpe
				// TODOS los iPhone del staff y habría que volver a registrarlos
				// uno a uno abriendo la app en cada teléfono.
				//
				// El 410 sí es inequívoco (Apple solo lo usa para Unregistered:
				// la app se desinstaló), así que vale aunque el cuerpo no se
				// haya podido leer.
				if apnsTokenIsDead(reason) || status == http.StatusGone {
					deactivate(token)
					log.Printf("[APNs] token desactivado — app desinstalada (HTTP %d reason=%q)", status, reason)
					continue
				}
				if apnsTokenMaybeDead(reason) {
					suspect = append(suspect, token)
					continue
				}
				log.Printf("[APNs] ATENCIÓN: HTTP %d reason=%q — parece configuración, NO se desactiva ningún token. Revisa APNS_BUNDLE_ID (debe ser el bundle del build), APNS_KEY_ID/APNS_TEAM_ID y APNS_SANDBOX (dev vs producción).", status, reason)
			}

			// BadDeviceToken es ambiguo (token corrupto vs. APNS_SANDBOX o
			// apns-topic equivocados). Solo se poda si OTRO envío del mismo lote
			// sí llegó: eso demuestra que clave, topic y entorno están bien y que
			// el problema es de ese teléfono concreto. Sin esa prueba se deja el
			// token activo — el coste de equivocarse no es simétrico: dejar vivo
			// un token muerto cuesta una fila y una línea de log por
			// notificación; matar uno vivo apaga los iPhone del staff hasta que
			// cada uno vuelva a abrir la app (el registro los reactiva, ver
			// MobileRegisterPushToken), y eso puede caer en mitad de un evento.
			if len(suspect) > 0 {
				if iosOK > 0 {
					for _, token := range suspect {
						deactivate(token)
					}
					log.Printf("[APNs] %d token(s) desactivados por BadDeviceToken (el resto del lote sí llegó, así que la configuración es correcta)", len(suspect))
				} else {
					log.Printf("[APNs] ATENCIÓN: %d/%d token(s) iOS con BadDeviceToken y NINGÚN envío correcto — huele a configuración, NO se desactiva nada. Revisa APNS_SANDBOX (dev vs producción: los tokens NO son intercambiables), APNS_BUNDLE_ID y APNS_KEY_ID/APNS_TEAM_ID.", len(suspect), len(iosTokens))
				}
			}
		}
	}

	log.Printf("[Push] NotifyVenueStaff venue=%s sent=%d/%d (fcm=%d ios=%d)", venueID, sent, len(fcmTokens)+len(iosTokens), len(fcmTokens), len(iosTokens))
}
