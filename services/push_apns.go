package services

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// =============================================================================
// APPLE PUSH NOTIFICATION SERVICE (APNs) — envío directo desde el backend.
//
// La app (getDevicePushTokenAsync) registra en iOS el token nativo de APNs
// con device_type="ios". FCM no sabe entregar a ese token, así que iOS se
// enruta por aquí: se firma un provider JWT (ES256, clave .p8) y se hace
// POST a api.push.apple.com/3/device/{token} sobre HTTP/2.
//
// FEATURE-FLAG: solo se activa si están las 4 variables. Si falta cualquiera,
// APNs queda apagado (los tokens iOS se saltan; Android por FCM intacto).
//   APNS_KEY_P8    contenido del .p8 (PEM, incluidas las líneas BEGIN/END).
//                  Lo crea una persona en developer.apple.com → Keys → + →
//                  marcar "Apple Push Notification service (APNs)". Es lo ÚNICO
//                  que falta para encender iOS. El .p8 se descarga UNA sola vez:
//                  si se pierde hay que generar otra clave.
//   APNS_KEY_ID    Key ID de esa misma APNs Auth Key (10 chars).
//   APNS_TEAM_ID   Team ID de la cuenta Apple Developer (10 chars). Es EL MISMO
//                  valor que el secreto PASS_TEAM_ID que ya existe en el entorno
//                  (lo usa Apple Wallet, ver applewallet.go) — cópialo de ahí en
//                  vez de buscarlo en el portal.
//   APNS_BUNDLE_ID com.pullevents.staff — el bundle de la app de staff; viaja
//                  como cabecera apns-topic. TRAMPA: si no coincide EXACTAMENTE
//                  con el bundle del build instalado, Apple rechaza TODOS los
//                  envíos con BadTopic/TopicDisallowed. Eso NO significa que los
//                  tokens estén muertos (ver apnsTokenIsDead más abajo).
//   APNS_SANDBOX   opcional; "true" usa el host sandbox (solo dev builds).
//                  TestFlight y App Store usan el host de PRODUCCIÓN (default).
//                  TRAMPA: entorno equivocado = 400 BadDeviceToken con un token
//                  perfectamente válido; los tokens de dev y de producción no
//                  son intercambiables.
// =============================================================================

type apnsClient struct {
	keyID, teamID, bundleID string
	host                    string
	privateKey              *ecdsa.PrivateKey
	client                  *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	tokenIssued time.Time // cuándo se firmó el JWT vigente (ver resetProviderToken)
}

// apns is the global APNs client (nil until InitAPNs con las 4 vars).
var apns *apnsClient

// InitAPNs initializes the APNs client from env. No-op (feature off) si falta
// cualquier variable o la clave no parsea.
func InitAPNs() {
	// Las 4 variables que hay que poner para ENCENDER iOS push (ver cabecera):
	//   APNS_KEY_P8    el .p8 entero (PEM). Lo genera una persona en Apple.
	//   APNS_KEY_ID    Key ID de ese .p8 (10 chars).
	//   APNS_TEAM_ID   MISMO valor que el secreto PASS_TEAM_ID ya existente.
	//   APNS_BUNDLE_ID com.pullevents.staff (bundle de la app de staff).
	// Mientras falte cualquiera, esto es un no-op: iOS no se envía y Android
	// (FCM) sigue funcionando exactamente igual.
	p8 := os.Getenv("APNS_KEY_P8")
	keyID := os.Getenv("APNS_KEY_ID")
	teamID := os.Getenv("APNS_TEAM_ID")
	bundleID := os.Getenv("APNS_BUNDLE_ID")
	if p8 == "" || keyID == "" || teamID == "" || bundleID == "" {
		log.Printf("[APNs] no configurado (falta APNS_KEY_P8/KEY_ID/TEAM_ID/BUNDLE_ID) — iOS push desactivado; Android (FCM) sin cambios")
		return
	}
	block, _ := pem.Decode([]byte(p8))
	if block == nil {
		log.Printf("[APNs] APNS_KEY_P8 no es PEM válido — iOS push desactivado")
		return
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		log.Printf("[APNs] no pude parsear la clave .p8 (PKCS8): %v", err)
		return
	}
	ecKey, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		log.Printf("[APNs] la clave .p8 no es ECDSA (esperado para APNs)")
		return
	}
	host := "https://api.push.apple.com"
	if os.Getenv("APNS_SANDBOX") == "true" {
		host = "https://api.sandbox.push.apple.com"
	}
	apns = &apnsClient{
		keyID:      keyID,
		teamID:     teamID,
		bundleID:   bundleID,
		host:       host,
		privateKey: ecKey,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
	log.Printf("[APNs] iOS push inicializado (team=%s bundle=%s host=%s)", teamID, bundleID, host)
}

// apnsEnabled reports whether APNs is configured.
func apnsEnabled() bool { return apns != nil }

// providerToken returns a cached (or fresh) APNs provider JWT. Apple acepta el
// token hasta 1h; se regenera cada ~50 min (Apple rate-limita la generación).
func (a *apnsClient) providerToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Now().Before(a.tokenExpiry) {
		return a.token, nil
	}
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": a.teamID,
		"iat": now.Unix(),
	})
	tok.Header["kid"] = a.keyID
	signed, err := tok.SignedString(a.privateKey)
	if err != nil {
		return "", fmt.Errorf("firmar APNs JWT: %w", err)
	}
	a.token = signed
	a.tokenExpiry = now.Add(50 * time.Minute)
	a.tokenIssued = now
	return signed, nil
}

// resetProviderToken tira la caché del provider JWT. Se llama cuando Apple
// contesta que el token de proveedor no vale: sin esto se repetiría el MISMO
// JWT (y el mismo error) durante los 50 minutos de caché.
//
// TRAMPA: Apple rate-limita la GENERACIÓN de provider tokens (contesta 429
// TooManyProviderTokenUpdates) y pide no refrescar más de una vez cada 20 min.
// Si APNS_KEY_ID/APNS_TEAM_ID están mal, TODOS los envíos contestan
// InvalidProviderToken: sin este freno firmaríamos un JWT nuevo por cada iPhone
// y por cada notificación, y Apple terminaría bloqueando la clave — dejándonos
// peor que al principio. Firmar otro JWT no arregla una clave mal configurada;
// eso lo arregla una persona cambiando el secreto. El caso que sí se arregla
// solo (ExpiredProviderToken por desfase de reloj) llega con el JWT ya viejo,
// así que pasa el filtro.
func (a *apnsClient) resetProviderToken() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.tokenIssued.IsZero() && time.Since(a.tokenIssued) < 20*time.Minute {
		return
	}
	a.token = ""
	a.tokenExpiry = time.Time{}
}

// apnsErrorReason extrae el campo "reason" del cuerpo de error de APNs. Apple
// contesta los 4xx/5xx con {"reason":"BadDeviceToken"} (410 añade "timestamp").
// Ese motivo es LO ÚNICO que dice qué está mal de verdad: con solo el código
// HTTP no se distingue "el token está muerto" de "el topic o la clave están mal
// configurados", que son problemas opuestos. El cuerpo NO lleva datos
// personales, así que es seguro loguearlo entero.
func apnsErrorReason(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, 4<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var out struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &out) == nil && out.Reason != "" {
		return out.Reason
	}
	// Cuerpo inesperado (proxy, HTML de error...): devuélvelo tal cual recortado
	// en vez de tragárselo — perder la pista aquí es lo que costaba un día.
	return strings.TrimSpace(string(raw))
}

// apnsTokenIsDead indica si el motivo de Apple es INEQUÍVOCO: este device token
// ya no vale nunca más y procede desactivarlo en la BD sin pensarlo.
//
//	Unregistered → la app se desinstaló de ese teléfono (llega con HTTP 410).
//	               Es el único motivo que Apple NO usa nunca para errores de
//	               configuración, y además es el 99% de las podas legítimas.
//
// TRAMPA: cualquier OTRO motivo (TopicDisallowed, BadTopic, ExpiredProviderToken,
// InvalidProviderToken, PayloadTooLarge, TooManyRequests...) es un fallo de
// configuración NUESTRO, no del dispositivo. Si se desactivaran esos tokens se
// perderían de golpe todos los iPhone del staff y habría que volver a
// registrarlos uno a uno abriendo la app en cada teléfono.
func apnsTokenIsDead(reason string) bool {
	return reason == "Unregistered"
}

// apnsTokenMaybeDead marca los motivos AMBIGUOS: pueden ser el token o pueden
// ser nosotros, y desde una sola respuesta no hay forma de distinguirlo.
//
// BadDeviceToken sale tanto cuando el token está corrupto como cuando el ENTORNO
// no coincide (APNS_SANDBOX mal puesto: los tokens de dev y de producción no son
// intercambiables) o el apns-topic no es el del build instalado. O sea: el mismo
// motivo que aparece cuando un token está muerto es el que aparece cuando
// TODOS están vivos y el mal configurado eres tú — y ahora mismo estamos en
// sandbox, que es justo cuando se pisa. Por eso el caller no decide token a
// token: espera a ver si algún otro envío del mismo lote llegó (ver push.go).
func apnsTokenMaybeDead(reason string) bool {
	return reason == "BadDeviceToken"
}

// maskAPNsToken deja solo los últimos 6 caracteres del device token. El token
// identifica a un dispositivo concreto: sirve para correlacionar líneas de log,
// pero no se escribe entero ni cuando falla.
func maskAPNsToken(t string) string {
	if len(t) <= 6 {
		return "..."
	}
	return "..." + t[len(t)-6:]
}

// send posts one alert to a device token. Devuelve el status HTTP y el "reason"
// que manda Apple, para que el caller decida si el token está muerto
// (apnsTokenIsDead) o si lo que está mal es la configuración.
func (a *apnsClient) send(ctx context.Context, deviceToken, title, body string, data map[string]interface{}) (int, string, error) {
	jwtTok, err := a.providerToken()
	if err != nil {
		return 0, "", err
	}
	payload := map[string]interface{}{
		"aps": map[string]interface{}{
			"alert": map[string]string{"title": title, "body": body},
			"sound": "default",
		},
	}
	for k, v := range data {
		// "aps" es el bloque reservado de Apple: si un caller metiera esa clave
		// en data se cargaría el alert y la notificación llegaría muda (entrega
		// silenciosa, sin banner). Ningún caller lo hace hoy, pero el día que
		// pase el síntoma sería "no llega nada" y nadie miraría aquí.
		if k == "aps" {
			log.Printf("[APNs] clave 'aps' ignorada en data — es el bloque reservado de Apple")
			continue
		}
		payload[k] = fmt.Sprintf("%v", v)
	}
	raw, _ := json.Marshal(payload)
	url := a.host + "/3/device/" + deviceToken
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("authorization", "bearer "+jwtTok)
	req.Header.Set("apns-topic", a.bundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	// TRAMPA de Apple: SIN esta cabecera el valor por defecto es 0, que significa
	// "intenta entregarla una vez y si el aparato no está conectado, tírala". Un
	// teléfono en el parking del local o con la pantalla bloqueada y mala
	// cobertura se pierde el aviso para siempre. Estas notificaciones avisan de
	// una solicitud privada que el staff tiene que decidir dentro de una ventana
	// de horas, así que pedimos a APNs que la guarde y reintente durante 1h;
	// pasado ese rato ya no aporta (el aviso vive también dentro de la app).
	req.Header.Set("apns-expiration", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
	req.Header.Set("content-type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, "", nil // 200 llega sin cuerpo
	}

	// Aquí es donde antes se tiraba la respuesta a la basura. El motivo se
	// registra EN ESTE PUNTO porque es el único sitio con el device token
	// delante; el caller solo recibe el reason. El apns-id sirve para que Apple
	// pueda rastrear el envío si algún día hay que preguntarles.
	reason := apnsErrorReason(resp.Body)
	log.Printf("[APNs] HTTP %d reason=%q device=%s apns-id=%s",
		resp.StatusCode, reason, maskAPNsToken(deviceToken), resp.Header.Get("apns-id"))

	// Provider JWT caducado o rechazado: invalida la caché para que el siguiente
	// envío firme uno nuevo, en vez de repetir el error hasta que expire sola.
	if reason == "ExpiredProviderToken" || reason == "InvalidProviderToken" {
		a.resetProviderToken()
	}
	return resp.StatusCode, reason, nil
}
