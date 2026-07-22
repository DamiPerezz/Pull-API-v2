package services

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
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
//   APNS_KEY_P8    contenido del .p8 (PEM, incluidas las líneas BEGIN/END)
//   APNS_KEY_ID    Key ID de la APNs Auth Key (10 chars)
//   APNS_TEAM_ID   Team ID de la cuenta Apple Developer (10 chars)
//   APNS_BUNDLE_ID bundle de la app (com.pullevents.staff) → apns-topic
//   APNS_SANDBOX   opcional; "true" usa el host sandbox (solo dev builds).
//                  TestFlight y App Store usan el host de PRODUCCIÓN (default).
// =============================================================================

type apnsClient struct {
	keyID, teamID, bundleID string
	host                    string
	privateKey              *ecdsa.PrivateKey
	client                  *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// apns is the global APNs client (nil until InitAPNs con las 4 vars).
var apns *apnsClient

// InitAPNs initializes the APNs client from env. No-op (feature off) si falta
// cualquier variable o la clave no parsea.
func InitAPNs() {
	p8 := os.Getenv("APNS_KEY_P8")
	keyID := os.Getenv("APNS_KEY_ID")
	teamID := os.Getenv("APNS_TEAM_ID")
	bundleID := os.Getenv("APNS_BUNDLE_ID")
	if p8 == "" || keyID == "" || teamID == "" || bundleID == "" {
		log.Printf("[APNs] no configurado (falta APNS_KEY_P8/KEY_ID/TEAM_ID/BUNDLE_ID) — iOS push desactivado")
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
	return signed, nil
}

// send posts one alert to a device token. Devuelve el status HTTP para que el
// caller pode tokens muertos (410 Unregistered / 400 BadDeviceToken).
func (a *apnsClient) send(ctx context.Context, deviceToken, title, body string, data map[string]interface{}) (int, error) {
	jwtTok, err := a.providerToken()
	if err != nil {
		return 0, err
	}
	payload := map[string]interface{}{
		"aps": map[string]interface{}{
			"alert": map[string]string{"title": title, "body": body},
			"sound": "default",
		},
	}
	for k, v := range data {
		payload[k] = fmt.Sprintf("%v", v)
	}
	raw, _ := json.Marshal(payload)
	url := a.host + "/3/device/" + deviceToken
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("authorization", "bearer "+jwtTok)
	req.Header.Set("apns-topic", a.bundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
