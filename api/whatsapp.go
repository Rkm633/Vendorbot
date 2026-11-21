package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"wa-server/ramstore"
	"wa-server/whatsapp"

	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
)

func encodeQRToDataURL(qrCode string) string {
	if qrCode == "" {
		return ""
	}
	pngBytes, err := qrcode.Encode(qrCode, qrcode.Medium, 240)
	if err != nil || len(pngBytes) == 0 {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
}

// StatusHandler returns single-vendor WhatsApp connection status & generates QR on this Vendor Bot's own dashboard
// ONLY if Central Brain Node has no saved WhatsMeow session.
func StatusHandler(w http.ResponseWriter, r *http.Request) {
	vendor := ramstore.Global.GetVendor()
	waSession := ramstore.Global.GetWhatsAppSession()
	syncSt := ramstore.Global.GetSyncStatus()

	status := "Needs_Login"
	jid := ""
	phone := waSession.Phone

	client := whatsapp.GetVendorClient()
	isLoggedIn := client != nil && client.Store != nil && client.Store.ID != nil

	if isLoggedIn {
		jid = client.Store.ID.String()
		phone = whatsapp.FormatPhone(client.Store.ID.User)
		if client.IsConnected() {
			status = "Connected"
		} else {
			status = "Connecting"
			go whatsapp.ConnectWithAutoRetry(client, vendor.Username)
		}
	} else if waSession.HasSession && waSession.Phone != "" {
		status = "Connecting"
		jid = waSession.DeviceJID
		phone = waSession.Phone
	}

	qrCode := ""
	qrDataURL := ""
	// Generate QR on this bot dashboard only when vendor has NOT linked WhatsApp yet
	if !isLoggedIn && !waSession.HasSession {
		qrCode = whatsapp.GetCurrentQRCode()
		if qrCode == "" && r.URL.Query().Get("start_qr") != "false" {
			qrCode = whatsapp.GetOrGenerateQR(vendor.Username)
		}
		if qrCode != "" {
			status = "QR_Ready"
			qrDataURL = encodeQRToDataURL(qrCode)
		}
	}

	respondJSON(w, 200, map[string]interface{}{
		"status":         status,
		"needs_login":    !isLoggedIn && !waSession.HasSession,
		"has_session":    isLoggedIn || waSession.HasSession,
		"qr":             qrCode,
		"qr_data_url":    qrDataURL,
		"jid":            jid,
		"device_jid":     jid,
		"phone":          phone,
		"connected":      status == "Connected",
		"vendor":         vendor.BusinessName,
		"vendor_id":      vendor.VendorID,
		"bot_public_url": syncSt.BotPublicURL,
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
	})
}

// PairPhoneHandler generates an 8-digit WhatsApp Link-With-Phone-Number pairing code on this Vendor Bot's own dashboard.
func PairPhoneHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}

	var req struct {
		Phone string `json:"phone"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	cleanPhone := whatsapp.FormatPhone(req.Phone)
	if cleanPhone == "" {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Phone number with country code is required"})
		return
	}

	client := whatsapp.GetVendorClient()
	if client == nil {
		respondJSON(w, 503, map[string]string{"status": "error", "message": "WhatsApp client not initialized"})
		return
	}

	if !client.IsConnected() {
		_ = client.Connect()
		for i := 0; i < 25; i++ {
			if client.IsConnected() {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	code, err := client.PairPhone(context.Background(), cleanPhone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		log.Printf("⚠️ [Pair Error]: %v", err)
		respondJSON(w, 500, map[string]string{"status": "error", "message": "Failed to generate pairing code: " + err.Error()})
		return
	}

	respondJSON(w, 200, map[string]interface{}{
		"status":  "success",
		"code":    code,
		"phone":   cleanPhone,
		"message": "Enter this 8-digit code in WhatsApp -> Linked Devices -> Link with phone number. Once linked, the session is automatically saved to Central Brain Node.",
	})
}

// LogoutWAHandler unlinks WhatsApp, clears ephemeral session file, and deletes saved session on Central Brain Node.
func LogoutWAHandler(w http.ResponseWriter, r *http.Request) {
	vendorUser := ramstore.Global.GetVendor().Username
	whatsapp.DisconnectAndUnlinkClient(vendorUser)
	whatsapp.PushToLiveChat(vendorUser, "", "WhatsApp session unlinked", "session_unlinked")

	respondJSON(w, 200, map[string]string{
		"status":  "success",
		"message": "WhatsApp unlinked and session cleared from Central Brain Node.",
	})
}
