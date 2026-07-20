package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// =============================================
// ANTI-CARDING GUARDS for POST /orders/pay
//
// /orders/pay es público (checkout anónimo) y devuelve aprobado/declinado,
// así que sin más defensa es un oráculo para probar tarjetas robadas. Tres
// capas, además del rate limit por IP que ya existe:
//
//  1. payment_link_code obligatorio — solo quien creó la orden (y recibió el
//     código en la respuesta de create-pending-order) puede intentar pagarla.
//  2. Límite de intentos por ORDEN (persistido en metadata) y por TARJETA
//     (hash en memoria) — el rate limit por IP no basta: detrás del proxy de
//     Cloudflare, Fly ve pocas IPs, y un atacante puede pegar directo a
//     fly.dev con muchas IPs.
//  3. Cloudflare Turnstile opcional (TURNSTILE_SECRET_KEY) — apagado hasta
//     que el frontend tenga el widget (VITE_TURNSTILE_SITE_KEY).
// =============================================

const (
	maxAttemptsPerOrder = 5                // intentos declinados por orden
	maxAttemptsPerCard  = 10               // intentos por tarjeta (todas las órdenes)
	cardAttemptWindow   = 15 * time.Minute // ventana del límite por tarjeta
)

// matchPaymentLinkCode compares the caller-supplied code against the one the
// order was created with, in constant time. Fail closed: an order without a
// stored code (legacy/test rows) is not payable through this endpoint.
func matchPaymentLinkCode(orderMetadata map[string]interface{}, supplied string) bool {
	stored, _ := orderMetadata["payment_link_code"].(string)
	if stored == "" || supplied == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(supplied)) == 1
}

// cardAttemptKey derives a non-reversible in-memory key from a card number.
// Never store or log the PAN itself.
func cardAttemptKey(cardNumber string) string {
	sum := sha256.Sum256([]byte(cardNumber))
	return hex.EncodeToString(sum[:8])
}

var cardAttempts = struct {
	sync.Mutex
	m map[string]*attemptWindow
}{m: make(map[string]*attemptWindow)}

type attemptWindow struct {
	count int
	start time.Time
}

// allowCardAttempt counts one payment attempt for this card hash and reports
// whether it is still under the per-card limit. In-memory: resets on deploy
// and is per-machine (2 máquinas ⇒ límite efectivo 2×) — acceptable, it is a
// brake, not an accounting system.
func allowCardAttempt(key string) bool {
	now := time.Now()
	cardAttempts.Lock()
	defer cardAttempts.Unlock()

	// Opportunistic prune so the map can't grow unbounded under an attack.
	if len(cardAttempts.m) > 10000 {
		for k, w := range cardAttempts.m {
			if now.Sub(w.start) > cardAttemptWindow {
				delete(cardAttempts.m, k)
			}
		}
	}

	w := cardAttempts.m[key]
	if w == nil || now.Sub(w.start) > cardAttemptWindow {
		cardAttempts.m[key] = &attemptWindow{count: 1, start: now}
		return true
	}
	w.count++
	return w.count <= maxAttemptsPerCard
}

// verifyTurnstile validates a Cloudflare Turnstile token. Disabled (returns
// nil) while TURNSTILE_SECRET_KEY is unset, so it can ship dark and be turned
// on once the frontend renders the widget.
func verifyTurnstile(ctx context.Context, token, remoteIP string) error {
	secret := os.Getenv("TURNSTILE_SECRET_KEY")
	if secret == "" {
		return nil
	}
	if token == "" {
		return fmt.Errorf("missing turnstile token")
	}
	body, _ := json.Marshal(map[string]string{
		"secret":   secret,
		"response": token,
		"remoteip": remoteIP,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://challenges.cloudflare.com/turnstile/v0/siteverify", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("turnstile verification failed")
	}
	return nil
}
