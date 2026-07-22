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
func InitPushService() {
	raw := os.Getenv("FCM_SERVICE_ACCOUNT_JSON")
	if raw == "" {
		log.Printf("[Push] FCM_SERVICE_ACCOUNT_JSON not set — push disabled (no-op)")
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
	Push = &PushService{
		sa:         &sa,
		privateKey: key,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
	log.Printf("[Push] FCM push initialized (project=%s)", sa.ProjectID)
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
	venueDB := DB.ForVenue(venueID)
	if venueDB == nil {
		log.Printf("[Push] NotifyVenueStaff: invalid venue %s", venueID)
		return
	}
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
		if GetString(r, "device_type") == "ios" {
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
	if len(fcmTokens) > 0 {
		accessToken, err := p.getAccessToken(ctx)
		if err != nil {
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
				if status == http.StatusNotFound || status == http.StatusBadRequest {
					deactivate(token)
				}
				log.Printf("[Push] FCM returned HTTP %d for a token", status)
			}
		}
	}

	// --- iOS / APNs (feature-flag: solo si APNS_* está configurado) ---
	if len(iosTokens) > 0 {
		if !apnsEnabled() {
			log.Printf("[Push] %d token(s) iOS sin enviar: APNs no configurado (faltan APNS_* secrets)", len(iosTokens))
		} else {
			for _, token := range iosTokens {
				status, err := apns.send(ctx, token, title, body, data)
				if err != nil {
					log.Printf("[APNs] send error: %v", err)
					continue
				}
				if status == http.StatusOK {
					sent++
					continue
				}
				// 410 Unregistered / 400 BadDeviceToken → token muerto.
				if status == http.StatusGone || status == http.StatusBadRequest {
					deactivate(token)
				}
				log.Printf("[APNs] returned HTTP %d for a token", status)
			}
		}
	}

	log.Printf("[Push] NotifyVenueStaff venue=%s sent=%d/%d (fcm=%d ios=%d)", venueID, sent, len(fcmTokens)+len(iosTokens), len(fcmTokens), len(iosTokens))
}
