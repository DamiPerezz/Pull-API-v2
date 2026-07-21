package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// =============================================
// CYBERSOURCE REST CLIENT (Visa Platform Connect / NeoNet)
// Implements HTTP Signature authentication and the payment operations the
// NeoNetProcessor needs: sale (auth+capture), authorization reversal and
// refund. Reference: "Payments Developer Guide — REST API, Visa Platform
// Connect" (PDF in the workspace root).
// =============================================

const (
	cybsHostTest = "apitest.cybersource.com"
	cybsHostProd = "api.cybersource.com"
)

// CybersourceClient is a minimal REST client for the Payments API.
type CybersourceClient struct {
	MerchantID   string
	KeyID        string // REST shared-secret key id (access_key column)
	SharedSecret string // base64 shared secret (secret_key_encrypted column)
	Host         string // apitest.cybersource.com | api.cybersource.com
	client       *http.Client
}

// NewCybersourceClient builds a client. environment "production" targets the
// live host; anything else targets the test host.
func NewCybersourceClient(merchantID, keyID, sharedSecret, environment string) *CybersourceClient {
	host := cybsHostTest
	if strings.EqualFold(environment, "production") || strings.EqualFold(environment, "live") {
		host = cybsHostProd
	}
	return &CybersourceClient{
		MerchantID:   merchantID,
		KeyID:        keyID,
		SharedSecret: sharedSecret,
		Host:         host,
		client:       &http.Client{Timeout: 30 * time.Second},
	}
}

// sign builds the HTTP Signature headers for a request.
// Signed headers (POST): host date request-target digest v-c-merchant-id
// Signed headers (GET):  host date request-target v-c-merchant-id
func (c *CybersourceClient) sign(method, path string, body []byte) (map[string]string, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(c.SharedSecret)
	if err != nil {
		return nil, fmt.Errorf("cybersource shared secret is not valid base64: %w", err)
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	target := strings.ToLower(method) + " " + path

	headers := map[string]string{
		"Host":            c.Host,
		"Date":            date,
		"v-c-merchant-id": c.MerchantID,
	}

	signedList := []string{"host", "date", "request-target", "v-c-merchant-id"}
	lines := []string{
		"host: " + c.Host,
		"date: " + date,
		"request-target: " + target,
	}

	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		sum := sha256.Sum256(body)
		digest := "SHA-256=" + base64.StdEncoding.EncodeToString(sum[:])
		headers["Digest"] = digest
		signedList = []string{"host", "date", "request-target", "digest", "v-c-merchant-id"}
		lines = append(lines, "digest: "+digest)
	}
	lines = append(lines, "v-c-merchant-id: "+c.MerchantID)

	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(strings.Join(lines, "\n")))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	headers["Signature"] = fmt.Sprintf(
		`keyid="%s", algorithm="HmacSHA256", headers="%s", signature="%s"`,
		c.KeyID, strings.Join(signedList, " "), signature,
	)
	return headers, nil
}

func (c *CybersourceClient) do(ctx context.Context, method, path string, payload interface{}) (int, map[string]interface{}, error) {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
	}

	sigHeaders, err := c.sign(method, path, body)
	if err != nil {
		return 0, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.Host+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range sigHeaders {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("cybersource request failed: %w", err)
	}
	defer resp.Body.Close()

	var parsed map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed, nil
}

// CybsCard holds raw card data collected by our payment page.
type CybsCard struct {
	Number     string
	ExpMonth   string // "12"
	ExpYear    string // "2031"
	SecurityCode string
}

// CybsBillTo is the minimum billing info Cybersource requires.
type CybsBillTo struct {
	FirstName  string
	LastName   string
	Email      string
	Phone      string
	Address1   string
	Locality   string
	AdminArea  string
	PostalCode string
	Country    string
}

// CybsSaleResult is the outcome of a sale (auth+capture).
type CybsSaleResult struct {
	Success          bool
	PaymentID        string // Cybersource transaction id (used for reversal/refund)
	Status           string // AUTHORIZED | DECLINED | ...
	AuthCode         string
	CardLast4        string
	AuthorizedAmount float64 // lo que el emisor aprobó de verdad (parcial < pedido)
	ErrorReason      string
	ErrorMessage     string
}

// cardTypeFor maps a PAN prefix to Cybersource's card type codes.
func cardTypeFor(number string) string {
	switch {
	case strings.HasPrefix(number, "4"):
		return "001" // Visa
	case strings.HasPrefix(number, "5"), strings.HasPrefix(number, "2"):
		return "002" // Mastercard
	case strings.HasPrefix(number, "34"), strings.HasPrefix(number, "37"):
		return "003" // Amex
	default:
		return ""
	}
}

// Sale authorizes a card. When capture is true it's an auth+capture (charges
// now — public events); when false it's authorization-only, which HOLDS the
// funds without charging until a follow-on Capture (private/approval events).
func (c *CybersourceClient) Sale(ctx context.Context, referenceCode string, amount float64, currency string, card CybsCard, billTo CybsBillTo, capture bool) (*CybsSaleResult, error) {
	payload := map[string]interface{}{
		"clientReferenceInformation": map[string]interface{}{"code": referenceCode},
		"processingInformation": map[string]interface{}{
			"capture":           capture,
			"commerceIndicator": "internet",
		},
		"paymentInformation": map[string]interface{}{
			"card": map[string]interface{}{
				"number":          card.Number,
				"expirationMonth": card.ExpMonth,
				"expirationYear":  card.ExpYear,
				"securityCode":    card.SecurityCode,
				"type":            cardTypeFor(card.Number),
			},
		},
		"orderInformation": map[string]interface{}{
			"amountDetails": map[string]interface{}{
				"totalAmount": fmt.Sprintf("%.2f", amount),
				"currency":    currency,
			},
			"billTo": map[string]interface{}{
				"firstName":          billTo.FirstName,
				"lastName":           billTo.LastName,
				"email":              billTo.Email,
				"phoneNumber":        billTo.Phone,
				"address1":           billTo.Address1,
				"locality":           billTo.Locality,
				"administrativeArea": billTo.AdminArea,
				"postalCode":         billTo.PostalCode,
				"country":            billTo.Country,
			},
		},
	}

	status, resp, err := c.do(ctx, http.MethodPost, "/pts/v2/payments", payload)
	if err != nil {
		return nil, err
	}

	result := &CybsSaleResult{
		PaymentID: GetString(resp, "id"),
		Status:    GetString(resp, "status"),
	}
	if proc, ok := resp["processorInformation"].(map[string]interface{}); ok {
		result.AuthCode = GetString(proc, "approvalCode")
	}
	if n := len(card.Number); n >= 4 {
		result.CardLast4 = card.Number[n-4:]
	}

	// AUTORIZACIÓN PARCIAL (tarjetas prepago, o el trigger "SDISCOUNT" del
	// sandbox de VisaNet GT, que con ciertos importes autoriza el 80%): el
	// emisor aprueba MENOS de lo pedido. NO se decide aquí — se expone el
	// importe autorizado y el CALLER elige (la parte del venue se rechaza y
	// reversa; el fee de Pull se acepta recortado y se captura lo autorizado,
	// porque matar una venta entera por el fee es peor negocio).
	result.AuthorizedAmount = amount
	if oi, ok := resp["orderInformation"].(map[string]interface{}); ok {
		if ad, ok := oi["amountDetails"].(map[string]interface{}); ok {
			if auth := GetFloat64(ad, "authorizedAmount"); auth > 0 {
				result.AuthorizedAmount = auth
			}
			if result.AuthorizedAmount < amount-0.005 {
				log.Printf("[Cybersource] PARTIAL AUTH ref=%s pedido=%.2f autorizado=%.2f",
					referenceCode, amount, result.AuthorizedAmount)
			}
		}
	}
	// Diagnóstico: cuando el status no es el AUTHORIZED de manual, dejar en
	// logs qué devolvió la pasarela (sin datos de tarjeta) para conciliar.
	if result.Status != "AUTHORIZED" {
		oiJSON, _ := json.Marshal(resp["orderInformation"])
		procJSON, _ := json.Marshal(resp["processorInformation"])
		log.Printf("[Cybersource] status=%s ref=%s orderInformation=%s processorInformation=%s",
			result.Status, referenceCode, oiJSON, procJSON)
	}

	// 201 + AUTHORIZED es el caso normal; el sandbox de VisaNet GT devuelve
	// a veces 201 + ACCEPTED en autorizaciones (visto en la 2ª auth del par
	// atómico con capture=false) — es una aprobación, no un rechazo. Todo lo
	// demás (DECLINED, INVALID_REQUEST, AUTHORIZED_RISK_DECLINED...) cae al
	// camino de rechazo de abajo.
	if status == 201 && (result.Status == "AUTHORIZED" || result.Status == "ACCEPTED") {
		result.Success = true
		return result, nil
	}

	// Declined / error paths — surface reason without leaking internals.
	if errInfo, ok := resp["errorInformation"].(map[string]interface{}); ok {
		result.ErrorReason = GetString(errInfo, "reason")
		result.ErrorMessage = GetString(errInfo, "message")
	}
	if result.ErrorMessage == "" {
		result.ErrorMessage = "Pago rechazado (" + result.Status + ")"
	}
	log.Printf("[Cybersource] sale NOT approved ref=%s http=%d status=%s reason=%s",
		referenceCode, status, result.Status, result.ErrorReason)
	return result, nil
}

// Reverse releases an authorization (used to undo the first sale when the
// second one of the atomic pair fails).
func (c *CybersourceClient) Reverse(ctx context.Context, paymentID, referenceCode string, amount float64, currency string) error {
	payload := map[string]interface{}{
		"clientReferenceInformation": map[string]interface{}{"code": referenceCode},
		"reversalInformation": map[string]interface{}{
			"amountDetails": map[string]interface{}{"totalAmount": fmt.Sprintf("%.2f", amount)},
			"reason":        "atomic pair rollback",
		},
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/pts/v2/payments/"+paymentID+"/reversals", payload)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("reversal HTTP %d status=%s", status, GetString(resp, "status"))
	}
	log.Printf("[Cybersource] reversal OK ref=%s status=%s", referenceCode, GetString(resp, "status"))
	return nil
}

// Capture settles a previously authorized (capture=false) payment. Used to
// charge a held authorization when staff approves a private-event order.
func (c *CybersourceClient) Capture(ctx context.Context, paymentID, referenceCode string, amount float64, currency string) error {
	payload := map[string]interface{}{
		"clientReferenceInformation": map[string]interface{}{"code": referenceCode},
		"orderInformation": map[string]interface{}{
			"amountDetails": map[string]interface{}{
				"totalAmount": fmt.Sprintf("%.2f", amount),
				"currency":    currency,
			},
		},
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/pts/v2/payments/"+paymentID+"/captures", payload)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("capture HTTP %d status=%s", status, GetString(resp, "status"))
	}
	// El status del body queda en logs para conciliar contra el EBC
	// (capturas suelen volver PENDING hasta el settlement del batch).
	log.Printf("[Cybersource] capture OK ref=%s status=%s", referenceCode, GetString(resp, "status"))
	return nil
}

// Refund issues a follow-on refund of a captured sale.
func (c *CybersourceClient) Refund(ctx context.Context, paymentID, referenceCode string, amount float64, currency string) error {
	payload := map[string]interface{}{
		"clientReferenceInformation": map[string]interface{}{"code": referenceCode},
		"orderInformation": map[string]interface{}{
			"amountDetails": map[string]interface{}{
				"totalAmount": fmt.Sprintf("%.2f", amount),
				"currency":    currency,
			},
		},
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/pts/v2/payments/"+paymentID+"/refunds", payload)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("refund HTTP %d status=%s", status, GetString(resp, "status"))
	}
	log.Printf("[Cybersource] refund OK ref=%s status=%s", referenceCode, GetString(resp, "status"))
	return nil
}
