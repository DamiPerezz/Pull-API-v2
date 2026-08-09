package controllers

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"pull-api-v2/config"
	"pull-api-v2/services"

	"github.com/gin-gonic/gin"
)

// sendTicketsEmailForOrder construye y envía el correo con las entradas: QR por
// ticket, PDF adjunto y (si está activo) el botón de Apple Wallet.
//
// Extraído de ConfirmPayment para que el REENVÍO mande el MISMO correo. Con
// Brevo como único proveedor, el reenvío es la única red de seguridad que
// queda cuando un envío falla: no puede ser una versión distinta del original.
//
// Devuelve error si el correo no salió. NUNCA devuelve nil por un envío
// fallido: quien llama no debe poder dar por entregada una entrada que no lo está.
func sendTicketsEmailForOrder(
	bgCtx context.Context,
	venueDB *services.SupabaseClient,
	venueID, eventID, ticketTypeName, userEmail, userName string,
	order map[string]interface{},
	ticketData []map[string]interface{},
) error {
	if services.Email == nil {
		return nil
	}

	// Resolve event + venue context for the email.
	eventName := ""
	eventDate := ""
	eventTime := ""
	eventImage := ""
	venueName := ""
	venueAddress := ""
	if ev, _ := venueDB.QueryOne(bgCtx, "events", map[string]interface{}{
		"select": "name,start_datetime,end_datetime,location,address,image,cover_image",
		"where":  map[string]interface{}{"id": eventID},
	}); ev != nil {
		eventName = services.GetString(ev, "name")
		eventImage = services.GetString(ev, "image")
		if eventImage == "" {
			eventImage = services.GetString(ev, "cover_image")
		}
		services.EnrichEvent(ev)
		eventDate = services.GetString(ev, "event_date")
		eventTime = services.GetString(ev, "start_time")
		venueName = services.GetString(ev, "location")
		venueAddress = services.GetString(ev, "address")
	}
	if vCentral, _ := services.DB.Central().QueryOne(bgCtx, "venues", map[string]interface{}{
		"select": "name,address", "where": map[string]interface{}{"id": venueID},
	}); vCentral != nil {
		if venueName == "" {
			venueName = services.GetString(vCentral, "name")
		}
		if venueAddress == "" {
			venueAddress = services.GetString(vCentral, "address")
		}
	}

	// Build per-ticket payload: PDF rows + QR images inline in the HTML.
	pdfTickets := make([]services.TicketPDFData, 0, len(ticketData))
	emailTickets := make([]services.TicketData, 0, len(ticketData))
	for _, td := range ticketData {
		qrToken := services.GetString(td, "qr_token")
		ownerName := services.GetString(td, "owner_name")
		if ln := services.GetString(td, "owner_last_name"); ln != "" {
			ownerName += " " + ln
		}
		ticketID := qrToken // stand-in until the row insert returns an id

		// Render the QR ONCE per ticket; the same PNG feeds both the
		// inline email image and the PDF attachment.
		var qrPNG []byte
		qrDataURL := ""
		if services.PDF != nil {
			if b, err := services.PDF.QRCodePNG(qrToken, 200); err == nil {
				qrPNG = b
				qrDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
			}
		}
		pdfTickets = append(pdfTickets, services.TicketPDFData{
			EventName:     eventName,
			EventDate:     eventDate,
			EventTime:     eventTime,
			VenueName:     venueName,
			VenueLocation: venueAddress,
			TicketType:    ticketTypeName,
			OwnerName:     ownerName,
			OrderNumber:   services.GetString(order, "order_number"),
			TicketID:      ticketID,
			QRCode:        qrToken,
			QRPNG:         qrPNG,
		})
		walletURL := ""
		if services.ApplePassEnabled() {
			// Enlace al .pkpass (vía el proxy → backend). Solo si Apple
			// Wallet está configurado; si no, el email sale sin botón.
			walletURL = fmt.Sprintf("%s/api/v1/tickets/apple-pass/%s?venue_id=%s",
				config.App.FrontendURL, qrToken, venueID)
		}
		emailTickets = append(emailTickets, services.TicketData{
			ID:             ticketID,
			Type:           ticketTypeName,
			OwnerName:      ownerName,
			QRCode:         qrToken,
			QRImageDataURL: qrDataURL,
			WalletURL:      walletURL,
		})
	}

	var pdfBytes []byte
	ordNumForPDF := services.GetString(order, "order_number")
	if services.PDF != nil {
		// Reintentar: un fallo puntual de render (pico de memoria, deploy en
		// curso) dejaría el email con el PDF vacío = ticket "defectuoso" para
		// el comprador aunque el ticket sea válido en BD. 3 intentos.
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			b, err := services.PDF.GenerateMultiTicketPDF(pdfTickets)
			if err == nil && len(b) > 0 {
				pdfBytes = b
				log.Printf("[Email/PDF] generated %d bytes for order=%s tickets=%d (intento %d)",
					len(pdfBytes), ordNumForPDF, len(pdfTickets), attempt)
				break
			}
			lastErr = err
			log.Printf("[Email/PDF] intento %d/3 FALLÓ order=%s: %v", attempt, ordNumForPDF, err)
			time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
		}
		// Si tras 3 intentos no hay PDF, NO enviar un email defectuoso (sin
		// ticket): abortar el envío con ALERT para reenviar cuando el PDF
		// vuelva. El ticket YA está en BD (escaneable) — solo falta el email.
		if len(pdfBytes) == 0 {
			log.Printf("[Email/PDF] ALERT sin PDF tras 3 intentos order=%s: %v — NO se envía email defectuoso, REENVIAR con POST /orders/<order_id>/resend-tickets (order %s)",
				ordNumForPDF, lastErr, ordNumForPDF)
			return fmt.Errorf("PDF vacío para order=%s: %w", ordNumForPDF, lastErr)
		}
	} else {
		log.Printf("[Email/PDF] services.PDF is nil — InitPDFService never ran")
	}

	totalStr := fmt.Sprintf("%.2f", services.GetFloat64(order, "total"))
	emailErr := services.Email.SendTickets(bgCtx, userEmail, services.TicketEmailData{
		OrderNumber:   services.GetString(order, "order_number"),
		CustomerName:  userName,
		EventName:     eventName,
		EventDate:     eventDate,
		EventTime:     eventTime,
		EventImage:    eventImage,
		EventLocation: venueAddress,
		VenueName:     venueName,
		TicketType:    ticketTypeName,
		Currency:      services.GetString(order, "currency"),
		Total:         totalStr,
		Tickets:       emailTickets,
	}, pdfBytes)

	// El QR viaja SOLO por email. Si el envío falla, el comprador YA pagó y sus
	// entradas YA están en la BD (válidas y escaneables): solo falta el correo.
	// Se deja rastro LOUD y accionable para arreglarlo durante el evento con
	// POST /orders/<order_id>/resend-tickets. Nunca dejar el fallo en silencio.
	if emailErr != nil {
		log.Printf("[Email] ALERT ticket email FAILED order=%s to=%s: %v — REENVIAR con POST /orders/<order_id>/resend-tickets (order %s)",
			services.GetString(order, "order_number"), userEmail, emailErr, services.GetString(order, "order_number"))
	}
	return emailErr
}

// ResendTicketsEmail reenvía el correo con las entradas de una orden YA pagada.
// POST /orders/:orderId/resend-tickets  (staff admin/manager)
//
// Es la palanca de emergencia del evento. Con Brevo como único proveedor de
// correo, el caso "he pagado y no me ha llegado nada" va a pasar: se acabó la
// cuota diaria del plan, Brevo tuvo un mal minuto, el cliente tecleó mal su
// email. Hasta ahora los logs decían "REENVIAR (cmd/resend ...)" y esa
// herramienta NO EXISTÍA: el aviso mandaba al operador a un sitio vacío.
//
// NO reemite entradas ni toca la orden: usa los tickets que YA están en la base
// de datos, con sus mismos QR. Reenviar diez veces no crea nada nuevo, así que
// se puede pulsar sin miedo delante de un cliente enfadado.
//
// Acepta `email` en el cuerpo para corregir una dirección mal escrita, que es
// la otra mitad de los incidentes reales.
func ResendTicketsEmail(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	if role := c.GetString("role"); role != "admin" && role != "manager" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Solo un admin o manager puede reenviar entradas"})
		return
	}
	venueID := c.GetString("venue_id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	var body struct {
		Email string `json:"email"`
	}
	_ = c.ShouldBindJSON(&body)

	orderID := c.Param("orderId")
	order, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,status,event_id,ticket_type_id,quantity,total,currency,user_name,user_email,metadata",
		"where":  map[string]interface{}{"id": orderID},
	})
	if order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}
	// Solo tiene sentido sobre una orden pagada: si no está confirmada, no hay
	// entradas que reenviar y el problema es otro (que el staff no lo tape).
	if st := services.GetString(order, "status"); st != "confirmed" && st != "checked_in" {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "Esta orden no está pagada: no hay entradas que reenviar",
			"status": st,
		})
		return
	}

	tickets, err := venueDB.QueryCtx(ctx, "tickets", map[string]interface{}{
		"select": "id,qr_token,owner_name,owner_last_name,ticket_type_name",
		"where":  map[string]interface{}{"order_id": orderID},
	})
	if err != nil || len(tickets) == 0 {
		log.Printf("[ResendTickets] ALERT orden %s pagada SIN filas de tickets — emitir a mano",
			services.GetString(order, "order_number"))
		c.JSON(http.StatusConflict, gin.H{"error": "La orden está pagada pero no tiene entradas emitidas. Avisa a soporte."})
		return
	}

	to := strings.TrimSpace(body.Email)
	if to == "" {
		to = services.GetString(order, "user_email")
	}
	if to == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "La orden no tiene email y no se indicó ninguno"})
		return
	}

	ticketTypeName := services.GetString(tickets[0], "ticket_type_name")
	if ticketTypeName == "" {
		if tt, _ := venueDB.QueryOne(ctx, "ticket_types", map[string]interface{}{
			"select": "name", "where": map[string]interface{}{"id": services.GetString(order, "ticket_type_id")},
		}); tt != nil {
			ticketTypeName = services.GetString(tt, "name")
		}
	}

	// SÍNCRONO a propósito: quien pulsa esto tiene al cliente delante y necesita
	// saber AHORA si salió o no. En cola no sabría si funcionó.
	err = sendTicketsEmailForOrder(ctx, venueDB, venueID,
		services.GetString(order, "event_id"), ticketTypeName,
		to, services.GetString(order, "user_name"), order, tickets)
	if err != nil {
		log.Printf("[ResendTickets] FALLÓ order=%s to=%s: %v",
			services.GetString(order, "order_number"), to, err)
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   "No se pudo enviar el correo. Si Brevo ha agotado la cuota diaria del plan, hay que subirlo.",
			"details": err.Error(),
		})
		return
	}

	log.Printf("[ResendTickets] reenviadas %d entradas order=%s to=%s",
		len(tickets), services.GetString(order, "order_number"), to)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("Se reenviaron %d entradas a %s", len(tickets), to),
		"tickets": len(tickets),
		"email":   to,
	})
}
