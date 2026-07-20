package services

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"log"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
	"github.com/jung-kurt/gofpdf"
)

// =============================================
// PDF SERVICE
// Generates ticket PDFs with QR codes
// =============================================

// PDFService handles PDF generation
type PDFService struct{}

// Global PDF service instance
var PDF *PDFService

// InitPDFService initializes the PDF service
func InitPDFService() error {
	PDF = &PDFService{}
	log.Println("PDF Service: Initialized")
	return nil
}

// =============================================
// TICKET PDF GENERATION
// =============================================

// TicketPDFData contains data for ticket PDF
type TicketPDFData struct {
	EventName     string
	EventDate     string
	EventTime     string
	VenueName     string
	VenueLocation string
	TicketType    string
	OwnerName     string
	OrderNumber   string
	TicketID      string
	QRCode        string // QR token
	QRPNG         []byte // pre-rendered QR PNG; if set, the PDF reuses it instead of re-encoding
}

// translateTicket converts every visible string of the ticket from UTF-8 to
// the cp1252 encoding that gofpdf's core fonts expect — without this,
// "Pérez" renders as "PÃ©rez" in the PDF. The QR token is NOT translated:
// its content must stay byte-exact for the scanner.
func translateTicket(tr func(string) string, t TicketPDFData) TicketPDFData {
	t.EventName = tr(t.EventName)
	t.EventDate = tr(t.EventDate)
	t.EventTime = tr(t.EventTime)
	t.VenueName = tr(t.VenueName)
	t.VenueLocation = tr(t.VenueLocation)
	t.TicketType = tr(t.TicketType)
	t.OwnerName = tr(t.OwnerName)
	t.OrderNumber = tr(t.OrderNumber)
	return t
}

// GenerateTicketPDF generates a single ticket PDF
func (p *PDFService) GenerateTicketPDF(ticket TicketPDFData) ([]byte, error) {
	pdf := gofpdf.New("P", "mm", "A4", "")
	tr := pdf.UnicodeTranslatorFromDescriptor("") // UTF-8 → cp1252
	ticket = translateTicket(tr, ticket)
	pdf.SetMargins(15, 15, 15)
	pdf.AddPage()

	// Ticket container with border
	pdf.SetDrawColor(200, 200, 200)
	pdf.SetLineWidth(0.5)
	pdf.RoundedRect(15, 15, 180, 120, 5, "1234", "D")

	// Header background
	pdf.SetFillColor(30, 30, 30)
	pdf.RoundedRect(15, 15, 180, 25, 5, "12", "F")

	// Event name (header)
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("Helvetica", "B", 18)
	pdf.SetXY(20, 22)
	pdf.CellFormat(170, 10, ticket.EventName, "", 0, "L", false, 0, "")

	// Event details
	pdf.SetTextColor(60, 60, 60)
	pdf.SetFont("Helvetica", "", 11)

	// Date and time
	pdf.SetXY(20, 48)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(40, 5, "FECHA", "", 0, "L", false, 0, "")
	pdf.SetXY(20, 53)
	pdf.SetFont("Helvetica", "", 12)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(40, 6, ticket.EventDate, "", 0, "L", false, 0, "")

	pdf.SetXY(70, 48)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(40, 5, "HORA", "", 0, "L", false, 0, "")
	pdf.SetXY(70, 53)
	pdf.SetFont("Helvetica", "", 12)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(40, 6, ticket.EventTime, "", 0, "L", false, 0, "")

	// Venue
	pdf.SetXY(20, 65)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(100, 5, "LUGAR", "", 0, "L", false, 0, "")
	pdf.SetXY(20, 70)
	pdf.SetFont("Helvetica", "", 11)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(100, 6, ticket.VenueName, "", 0, "L", false, 0, "")
	if ticket.VenueLocation != "" {
		pdf.SetXY(20, 76)
		pdf.SetFont("Helvetica", "", 9)
		pdf.SetTextColor(100, 100, 100)
		pdf.CellFormat(100, 5, ticket.VenueLocation, "", 0, "L", false, 0, "")
	}

	// Ticket type
	pdf.SetXY(20, 88)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(60, 5, "TIPO DE ENTRADA", "", 0, "L", false, 0, "")
	pdf.SetXY(20, 93)
	pdf.SetFont("Helvetica", "B", 14)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(60, 7, ticket.TicketType, "", 0, "L", false, 0, "")

	// Owner name
	pdf.SetXY(20, 106)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(60, 5, "NOMBRE", "", 0, "L", false, 0, "")
	pdf.SetXY(20, 111)
	pdf.SetFont("Helvetica", "", 11)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(60, 6, ticket.OwnerName, "", 0, "L", false, 0, "")

	// QR Code
	qrImg, err := p.generateQRCode(ticket.QRCode, 200)
	if err == nil {
		// Convert image to PNG bytes
		var imgBuf bytes.Buffer
		png.Encode(&imgBuf, qrImg)

		// Register and use image
		imgName := fmt.Sprintf("qr_%s", ticket.TicketID)
		pdf.RegisterImageOptionsReader(imgName, gofpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(imgBuf.Bytes()))
		pdf.ImageOptions(imgName, 140, 50, 45, 45, false, gofpdf.ImageOptions{}, 0, "")
	}

	// Order number and ticket ID (footer)
	pdf.SetXY(140, 100)
	pdf.SetFont("Helvetica", "", 8)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(45, 4, fmt.Sprintf("Orden: %s", ticket.OrderNumber), "", 0, "C", false, 0, "")
	pdf.SetXY(140, 105)
	pdf.CellFormat(45, 4, fmt.Sprintf("ID: %s", ticket.TicketID[:8]), "", 0, "C", false, 0, "")

	// Dashed line (tear here)
	pdf.SetDrawColor(180, 180, 180)
	pdf.SetDashPattern([]float64{2, 2}, 0)
	pdf.Line(15, 135, 195, 135)

	// Instructions
	pdf.SetDashPattern([]float64{}, 0)
	pdf.SetXY(15, 140)
	pdf.SetFont("Helvetica", "", 9)
	pdf.SetTextColor(100, 100, 100)
	pdf.MultiCell(180, 5, tr("Presenta este código QR en la entrada del evento. Esta entrada es personal e intransferible."), "", "C", false)

	// Output
	var buf bytes.Buffer
	err = pdf.Output(&buf)
	if err != nil {
		return nil, fmt.Errorf("failed to generate PDF: %w", err)
	}

	return buf.Bytes(), nil
}

// GenerateMultiTicketPDF generates PDF with multiple tickets
func (p *PDFService) GenerateMultiTicketPDF(tickets []TicketPDFData) ([]byte, error) {
	pdf := gofpdf.New("P", "mm", "A4", "")
	tr := pdf.UnicodeTranslatorFromDescriptor("") // UTF-8 → cp1252
	pdf.SetMargins(15, 15, 15)

	for i, ticket := range tickets {
		if i%2 == 0 {
			pdf.AddPage()
		}

		yOffset := float64(15)
		if i%2 == 1 {
			yOffset = 150
		}

		p.drawTicketOnPage(pdf, tr, translateTicket(tr, ticket), yOffset)
	}

	var buf bytes.Buffer
	err := pdf.Output(&buf)
	if err != nil {
		return nil, fmt.Errorf("failed to generate PDF: %w", err)
	}

	return buf.Bytes(), nil
}

// drawTicketOnPage draws a single ticket at given Y offset. The ticket's
// strings must already be cp1252-translated (see translateTicket).
func (p *PDFService) drawTicketOnPage(pdf *gofpdf.Fpdf, tr func(string) string, ticket TicketPDFData, yOffset float64) {
	// Ticket container
	pdf.SetDrawColor(200, 200, 200)
	pdf.SetLineWidth(0.5)
	pdf.RoundedRect(15, yOffset, 180, 120, 5, "1234", "D")

	// Header background
	pdf.SetFillColor(30, 30, 30)
	pdf.RoundedRect(15, yOffset, 180, 25, 5, "12", "F")

	// Event name
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("Helvetica", "B", 16)
	pdf.SetXY(20, yOffset+7)
	pdf.CellFormat(170, 10, ticket.EventName, "", 0, "L", false, 0, "")

	// Details
	pdf.SetTextColor(60, 60, 60)
	pdf.SetFont("Helvetica", "", 10)

	pdf.SetXY(20, yOffset+33)
	pdf.SetFont("Helvetica", "B", 8)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(40, 4, "FECHA", "", 0, "L", false, 0, "")
	pdf.SetXY(20, yOffset+37)
	pdf.SetFont("Helvetica", "", 11)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(40, 5, ticket.EventDate, "", 0, "L", false, 0, "")

	pdf.SetXY(65, yOffset+33)
	pdf.SetFont("Helvetica", "B", 8)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(40, 4, "HORA", "", 0, "L", false, 0, "")
	pdf.SetXY(65, yOffset+37)
	pdf.SetFont("Helvetica", "", 11)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(40, 5, ticket.EventTime, "", 0, "L", false, 0, "")

	pdf.SetXY(20, yOffset+48)
	pdf.SetFont("Helvetica", "B", 8)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(60, 4, "LUGAR", "", 0, "L", false, 0, "")
	pdf.SetXY(20, yOffset+52)
	pdf.SetFont("Helvetica", "", 10)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(60, 5, ticket.VenueName, "", 0, "L", false, 0, "")

	pdf.SetXY(20, yOffset+63)
	pdf.SetFont("Helvetica", "B", 8)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(60, 4, "TIPO", "", 0, "L", false, 0, "")
	pdf.SetXY(20, yOffset+67)
	pdf.SetFont("Helvetica", "B", 12)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(60, 6, ticket.TicketType, "", 0, "L", false, 0, "")

	pdf.SetXY(20, yOffset+78)
	pdf.SetFont("Helvetica", "B", 8)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(60, 4, "NOMBRE", "", 0, "L", false, 0, "")
	pdf.SetXY(20, yOffset+82)
	pdf.SetFont("Helvetica", "", 10)
	pdf.SetTextColor(30, 30, 30)
	pdf.CellFormat(60, 5, ticket.OwnerName, "", 0, "L", false, 0, "")

	// QR Code — reuse the caller's pre-rendered PNG when available (the email
	// path already renders one per ticket) instead of encoding it twice.
	qrPNG := ticket.QRPNG
	if qrPNG == nil {
		if b, err := p.QRCodePNG(ticket.QRCode, 200); err == nil {
			qrPNG = b
		}
	}
	if qrPNG != nil {
		imgName := fmt.Sprintf("qr_%s_%f", ticket.TicketID, yOffset)
		pdf.RegisterImageOptionsReader(imgName, gofpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(qrPNG))
		pdf.ImageOptions(imgName, 145, yOffset+35, 40, 40, false, gofpdf.ImageOptions{}, 0, "")
	}

	// Footer info
	pdf.SetXY(140, yOffset+80)
	pdf.SetFont("Helvetica", "", 7)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(45, 4, fmt.Sprintf("Orden: %s", ticket.OrderNumber), "", 0, "C", false, 0, "")

	shortID := ticket.TicketID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	pdf.SetXY(140, yOffset+84)
	pdf.CellFormat(45, 4, fmt.Sprintf("ID: %s", shortID), "", 0, "C", false, 0, "")

	// Dashed separator
	pdf.SetDrawColor(180, 180, 180)
	pdf.SetDashPattern([]float64{2, 2}, 0)
	pdf.Line(15, yOffset+95, 195, yOffset+95)
	pdf.SetDashPattern([]float64{}, 0)

	// Instructions
	pdf.SetXY(15, yOffset+98)
	pdf.SetFont("Helvetica", "", 8)
	pdf.SetTextColor(100, 100, 100)
	pdf.MultiCell(180, 4, tr("Presenta este código QR en la entrada. Entrada personal e intransferible."), "", "C", false)
}

// generateQRCode generates a QR code image. Note: boombuler/barcode returns
// images backed by 16-bit color models, which gofpdf's PNG decoder rejects
// ("16-bit depth not supported in PNG file"). We re-draw into an 8-bit
// NRGBA buffer to make the bytes safe to embed in a PDF.
func (p *PDFService) generateQRCode(content string, size int) (image.Image, error) {
	qrCode, err := qr.Encode(content, qr.M, qr.Auto)
	if err != nil {
		return nil, err
	}

	qrCode, err = barcode.Scale(qrCode, size, size)
	if err != nil {
		return nil, err
	}

	bounds := qrCode.Bounds()
	rgba := image.NewNRGBA(bounds)
	draw.Draw(rgba, bounds, qrCode, bounds.Min, draw.Src)
	return rgba, nil
}

// =============================================
// UTILITY FUNCTIONS
// =============================================

// QRCodePNG generates a QR code and returns the encoded PNG bytes, so one
// render can be shared between the email (base64 inline) and the PDF.
func (p *PDFService) QRCodePNG(content string, size int) ([]byte, error) {
	img, err := p.generateQRCode(content, size)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// QRCodeToBase64 generates QR code and returns as base64 string
func (p *PDFService) QRCodeToBase64(content string, size int) (string, error) {
	b, err := p.QRCodePNG(content, size)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
