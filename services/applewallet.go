package services

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"

	"github.com/digitorus/pkcs7"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// =============================================================================
// APPLE WALLET (.pkpass) — genera y firma pases de entrada para Apple Wallet.
//
// Un .pkpass es un ZIP con: pass.json (el pase, con el QR = qr_token del
// ticket), iconos PNG, manifest.json (SHA1 de cada archivo) y signature (firma
// PKCS#7 detached del manifest, con el cert del Pass Type ID + WWDR).
// El QR lo renderiza el iPhone a partir del qr_token → se escanea en la puerta
// EXACTAMENTE igual que el del PDF (mismo token, mismo escáner).
//
// FEATURE-FLAG: solo se activa con las 4 variables. Sin ellas, el endpoint
// responde 404 y el resto del sistema (PDF+email) sigue igual.
//   PASS_P12_BASE64      .p12 (cert Pass Type ID + clave) en base64
//   PASS_P12_PASSWORD    contraseña del .p12
//   PASS_WWDR_BASE64     cert WWDR de Apple (.cer DER) en base64
//   PASS_TYPE_ID         pass.com.pullevents.tickets
//   PASS_TEAM_ID         DSRBHX6N45
// =============================================================================

type applePassService struct {
	cert       *x509.Certificate
	privateKey crypto.PrivateKey
	wwdr       *x509.Certificate
	passTypeID string
	teamID     string
	orgName    string
	// iconos generados una vez (mismos para todos los pases)
	images map[string][]byte
}

var applePass *applePassService

// ApplePassData son los datos de un ticket para pintar el pase.
type ApplePassData struct {
	SerialNumber string // id del ticket (único por pase)
	QRToken      string // el barcode — lo escanea la puerta
	EventName    string
	EventDate    string // "2006-01-02"
	EventTime    string // "15:04:05" o "15:04"
	VenueName    string
	OwnerName    string
	TicketType   string
	Currency     string
}

// InitApplePass carga los certificados desde env. No-op (feature off) si falta
// cualquiera o algo no parsea.
func InitApplePass() {
	p12b64 := os.Getenv("PASS_P12_BASE64")
	p12pass := os.Getenv("PASS_P12_PASSWORD")
	wwdrb64 := os.Getenv("PASS_WWDR_BASE64")
	passTypeID := os.Getenv("PASS_TYPE_ID")
	teamID := os.Getenv("PASS_TEAM_ID")
	if p12b64 == "" || wwdrb64 == "" || passTypeID == "" || teamID == "" {
		log.Printf("[ApplePass] no configurado (faltan PASS_* vars) — Apple Wallet desactivado")
		return
	}
	p12Bytes, err := base64.StdEncoding.DecodeString(p12b64)
	if err != nil {
		log.Printf("[ApplePass] PASS_P12_BASE64 no es base64 válido: %v", err)
		return
	}
	privKey, cert, err := pkcs12.Decode(p12Bytes, p12pass)
	if err != nil {
		log.Printf("[ApplePass] no pude abrir el .p12 (¿contraseña incorrecta?): %v", err)
		return
	}
	wwdrBytes, err := base64.StdEncoding.DecodeString(wwdrb64)
	if err != nil {
		log.Printf("[ApplePass] PASS_WWDR_BASE64 no es base64 válido: %v", err)
		return
	}
	wwdr, err := x509.ParseCertificate(wwdrBytes)
	if err != nil {
		log.Printf("[ApplePass] no pude parsear el cert WWDR: %v", err)
		return
	}
	orgName := os.Getenv("PASS_ORG_NAME")
	if orgName == "" {
		orgName = "511 Events"
	}
	applePass = &applePassService{
		cert:       cert,
		privateKey: privKey,
		wwdr:       wwdr,
		passTypeID: passTypeID,
		teamID:     teamID,
		orgName:    orgName,
		images:     buildPassImages(),
	}
	log.Printf("[ApplePass] Apple Wallet inicializado (passType=%s team=%s)", passTypeID, teamID)
}

// ApplePassEnabled reporta si Apple Wallet está configurado.
func ApplePassEnabled() bool { return applePass != nil }

// BuildPass genera el .pkpass firmado para un ticket. Devuelve los bytes del
// archivo (Content-Type application/vnd.apple.pkpass).
func BuildPass(d ApplePassData) ([]byte, error) {
	if applePass == nil {
		return nil, fmt.Errorf("Apple Wallet no configurado")
	}
	return applePass.build(d)
}

func (s *applePassService) build(d ApplePassData) ([]byte, error) {
	passJSON, err := s.passJSON(d)
	if err != nil {
		return nil, err
	}

	// Reunir todos los archivos del pase.
	files := map[string][]byte{"pass.json": passJSON}
	for name, data := range s.images {
		files[name] = data
	}

	// manifest.json = { "archivo": "sha1hex", ... }
	manifest := map[string]string{}
	for name, data := range files {
		sum := sha1.Sum(data)
		manifest[name] = hex.EncodeToString(sum[:])
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}

	// signature = firma PKCS#7 DETACHED del manifest, con la cadena WWDR.
	signature, err := s.sign(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("firmar manifest: %w", err)
	}

	// Empaquetar todo en un ZIP (.pkpass).
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZip := func(name string, data []byte) error {
		w, e := zw.Create(name)
		if e != nil {
			return e
		}
		_, e = w.Write(data)
		return e
	}
	for name, data := range files {
		if err := writeZip(name, data); err != nil {
			return nil, err
		}
	}
	if err := writeZip("manifest.json", manifestJSON); err != nil {
		return nil, err
	}
	if err := writeZip("signature", signature); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sign produce la firma PKCS#7 detached que Apple exige.
func (s *applePassService) sign(manifest []byte) ([]byte, error) {
	sd, err := pkcs7.NewSignedData(manifest)
	if err != nil {
		return nil, err
	}
	sd.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	if err := sd.AddSigner(s.cert, s.privateKey, pkcs7.SignerInfoConfig{}); err != nil {
		return nil, err
	}
	// Añadir el intermedio WWDR a la cadena para que Apple valide la firma.
	sd.AddCertificate(s.wwdr)
	sd.Detach()
	return sd.Finish()
}

// passJSON construye el pass.json (tipo eventTicket).
func (s *applePassService) passJSON(d ApplePassData) ([]byte, error) {
	field := func(key, label, value string) map[string]interface{} {
		return map[string]interface{}{"key": key, "label": label, "value": value}
	}
	// Separador ASCII: el JSON del pase viaja como UTF-8, pero algunos
	// visores/logs mostraban mal el "·" — con guion no hay ambigüedad.
	when := d.EventDate
	if d.EventTime != "" {
		when = d.EventDate + " - " + d.EventTime
	}
	pass := map[string]interface{}{
		"formatVersion":       1,
		"passTypeIdentifier":  s.passTypeID,
		"teamIdentifier":      s.teamID,
		"organizationName":    s.orgName,
		"serialNumber":        d.SerialNumber,
		"description":         "Entrada " + d.EventName,
		"foregroundColor":     "rgb(255, 255, 255)",
		"backgroundColor":     "rgb(20, 20, 28)",
		"labelColor":          "rgb(167, 139, 250)",
		"barcodes": []map[string]interface{}{{
			"format":          "PKBarcodeFormatQR",
			"message":         d.QRToken,
			"messageEncoding": "iso-8859-1",
			"altText":         d.QRToken[:min(8, len(d.QRToken))],
		}},
		"eventTicket": map[string]interface{}{
			"headerFields":    []map[string]interface{}{field("date", "FECHA", when)},
			"primaryFields":   []map[string]interface{}{field("event", "EVENTO", d.EventName)},
			"secondaryFields": []map[string]interface{}{field("holder", "TITULAR", d.OwnerName), field("type", "ENTRADA", d.TicketType)},
			"auxiliaryFields": []map[string]interface{}{field("venue", "LUGAR", d.VenueName)},
		},
	}
	return json.MarshalIndent(pass, "", "  ")
}

// buildPassImages genera los iconos/logo requeridos (cuadrado morado de Pull).
// Apple exige icon.png (+ @2x/@3x); logo es opcional pero mejora el aspecto.
func buildPassImages() map[string][]byte {
	purple := color.NRGBA{R: 139, G: 92, B: 246, A: 255}
	square := func(size int) []byte {
		img := image.NewNRGBA(image.Rect(0, 0, size, size))
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				img.Set(x, y, purple)
			}
		}
		var b bytes.Buffer
		_ = png.Encode(&b, img)
		return b.Bytes()
	}
	rect := func(w, h int) []byte {
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.Set(x, y, purple)
			}
		}
		var b bytes.Buffer
		_ = png.Encode(&b, img)
		return b.Bytes()
	}
	return map[string][]byte{
		"icon.png":     square(29),
		"icon@2x.png":  square(58),
		"icon@3x.png":  square(87),
		"logo.png":     rect(160, 50),
		"logo@2x.png":  rect(320, 100),
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
