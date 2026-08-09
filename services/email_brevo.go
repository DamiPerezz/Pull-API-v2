package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"pull-api-v2/config"
	"time"
)

// =============================================
// BREVO PROVIDER
// =============================================
//
// Brevo (antes Sendinblue) es el ÚNICO proveedor de correo desde el
// 2026-08-09: se retiró el fallback a Resend por decisión de producto.
//
// OJO CON EL PLAN: el gratuito son 300 correos AL DÍA, y ahí entra todo
// (entradas, solicitudes, avisos al staff, enlaces de pago). Agotada la cuota,
// Brevo responde 429 y el correo NO SALE POR NINGÚN SITIO: el comprador paga y
// se queda sin su QR. Antes de un evento con volumen, comprobar el plan:
//
//	curl -s https://api.brevo.com/v3/account -H "api-key: $BREVO_API_KEY"

// sendViaBrevo posts the email payload to Brevo's transactional API. Returns
// the Brevo messageId on success.
func (e *EmailService) sendViaBrevo(ctx context.Context, req EmailRequest) (string, error) {
	if config.App.BrevoAPIKey == "" {
		return "", fmt.Errorf("brevo api key not configured")
	}

	// Parse "Name <email>" into the structured sender Brevo expects.
	senderName, senderEmail := parseFromHeader(config.App.BrevoFromEmail)
	if senderEmail == "" {
		senderName, senderEmail = parseFromHeader(e.fromEmail)
	}
	if senderEmail == "" {
		return "", fmt.Errorf("brevo: no sender email configured (set BREVO_FROM_EMAIL)")
	}

	to := make([]map[string]string, 0, len(req.To))
	for _, addr := range req.To {
		to = append(to, map[string]string{"email": addr})
	}

	body := map[string]interface{}{
		"sender":      map[string]string{"name": senderName, "email": senderEmail},
		"to":          to,
		"subject":     req.Subject,
		"htmlContent": req.HTML,
	}
	if req.Text != "" && req.HTML == "" {
		body["textContent"] = req.Text
	}

	// Brevo's transactional API expects attachments under the `attachment`
	// key, with each item having `name` + `content` (already base64). Our
	// internal EmailAttachment uses `Filename`/`Content` (base64 string), so
	// we just rename keys here.
	if len(req.Attachments) > 0 {
		atts := make([]map[string]string, 0, len(req.Attachments))
		totalBytes := 0
		for _, a := range req.Attachments {
			atts = append(atts, map[string]string{
				"name":    a.Filename,
				"content": a.Content,
			})
			totalBytes += len(a.Content)
		}
		body["attachment"] = atts
		log.Printf("[Email/Brevo] attaching %d files (%d base64 bytes total) to subject=%q",
			len(atts), totalBytes, req.Subject)
	} else {
		log.Printf("[Email/Brevo] NO attachments on subject=%q", req.Subject)
	}

	buf := emailBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer emailBufferPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.brevo.com/v3/smtp/email", buf)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("api-key", config.App.BrevoAPIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("brevo: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		log.Printf("[Email/Brevo] REJECTED to=%v subject=%q status=%d body=%s",
			req.To, req.Subject, resp.StatusCode, string(respBody))
		return "", fmt.Errorf("brevo error %d: %s", resp.StatusCode, string(respBody))
	}

	var out struct {
		MessageID string `json:"messageId"`
	}
	_ = json.Unmarshal(respBody, &out)
	log.Printf("[Email/Brevo] Sent to=%v subject=%q messageId=%s", req.To, req.Subject, out.MessageID)
	return out.MessageID, nil
}

// parseFromHeader splits "Name <email>" into the two components. When the
// input has no angle brackets the whole string is returned as the email and
// the name is empty.
func parseFromHeader(s string) (name, email string) {
	if s == "" {
		return "", ""
	}
	open := -1
	close := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '<' {
			open = i
		} else if s[i] == '>' {
			close = i
		}
	}
	if open >= 0 && close > open {
		email = s[open+1 : close]
		name = ""
		for i := 0; i < open; i++ {
			if s[i] != ' ' && s[i] != '\t' {
				name = s[:open]
				break
			}
		}
		// trim trailing whitespace
		for len(name) > 0 && (name[len(name)-1] == ' ' || name[len(name)-1] == '\t') {
			name = name[:len(name)-1]
		}
		return name, email
	}
	return "", s
}

// briefDelay keeps the linter from complaining about an unused import in
// builds where the email service hasn't been touched in months. Cheap no-op.
var _ = time.Second
