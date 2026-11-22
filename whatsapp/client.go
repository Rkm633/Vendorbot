package whatsapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"wa-server/central"
	"wa-server/ramstore"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var (
	StoreContainer *sqlstore.Container
	vendorClient   *whatsmeow.Client
	currentQRCode  string
	ClientsLock    sync.Mutex

	// 0ms In-Memory LID-to-RealPhone & Phone-to-JID Mappings
	lidToPnRAM   = make(map[string]string)
	phoneToJID   = make(map[string]types.JID)
	lidCacheLock sync.RWMutex

	// Per-Customer AI Concurrency Lock + Split-Message Interruption Guard
	customerInFlightMap  = make(map[string]bool)
	splitNoticeSentMap   = make(map[string]bool)
	customerInFlightLock sync.Mutex

	// Live Chat Listeners (Browser SSE channels)
	LiveChatStreams = make(map[string]chan map[string]string)
	StreamLock      sync.Mutex

	// Smart Human-Override: Tracks Message ID -> true when read by human store owner
	MessageReadByHuman sync.Map

	// Per-customer cooldown for Store Closed / Holiday notices so we don't spam repeated messages
	closedNoticeMu   sync.Mutex
	closedNoticeSent = make(map[string]time.Time)

	// Universal Static JPEG Thumbnail (fallback if image decode fails)
	UniversalThumbnail []byte
)

const (
	// MessageReadDelay waits 1 second before reading (blue-ticking) and processing a customer message.
	MessageReadDelay = 1 * time.Second

	// SplitMessagePeacefulNotice is sent with WhatsApp's inbuilt reply-quote tag whenever a customer
	// sends another message before the bot finishes replying to their previous message.
	SplitMessagePeacefulNotice = "🙏 *Kripya dhyan dein ji!* 😊\n\nApni kisi bhi baat ko *ek sath likh kar bhejein*, unhe tukdo me karke na bhejein — taaki main aapko *fast aur behtar* reply de saku! ✨💛"

	// FirstTurnSingleMessageTip is politely shown on a customer's very first conversation turn.
	FirstTurnSingleMessageTip = "\n\n💡 _Ek chhoti si request:_ Koshish karein ki apni poori baat *ek sath ek hi message me* likh kar bhejein (tukdo me nahi), taaki main aapko aur jaldi & best help kar saku! 😊✨"
)

func init() {
	const universalThumbnailBase64 = "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"
	if decoded, err := base64.StdEncoding.DecodeString(universalThumbnailBase64); err == nil && len(decoded) > 0 {
		UniversalThumbnail = decoded
	}
}

// LoadLIDMapCache loads LID -> Phone mappings from the local ephemeral whatsmeow session DB into RAM.
func LoadLIDMapCache() {
	sessionFileMu.Lock()
	db := ephemeralSQLDB
	sessionFileMu.Unlock()
	if db == nil {
		return
	}
	rows, err := db.Query("SELECT lid, pn FROM whatsmeow_lid_map WHERE pn != ''")
	if err != nil {
		return
	}
	defer rows.Close()

	lidCacheLock.Lock()
	defer lidCacheLock.Unlock()
	for rows.Next() {
		var lid, pn string
		if err := rows.Scan(&lid, &pn); err == nil && lid != "" && pn != "" {
			lidToPnRAM[lid] = pn
		}
	}
}

// GetRealPhoneFromLID resolves a WhatsApp LID to a phone number using RAM cache or ephemeral session DB.
func GetRealPhoneFromLID(lid string) string {
	cleanLID := regexp.MustCompile(`\D`).ReplaceAllString(lid, "")
	if cleanLID == "" {
		return ""
	}
	lidCacheLock.RLock()
	pn, exists := lidToPnRAM[cleanLID]
	lidCacheLock.RUnlock()
	if exists && pn != "" {
		return FormatPhone(pn)
	}

	sessionFileMu.Lock()
	db := ephemeralSQLDB
	sessionFileMu.Unlock()
	if db != nil {
		var dbPN string
		if err := db.QueryRow("SELECT pn FROM whatsmeow_lid_map WHERE lid = ? LIMIT 1", cleanLID).Scan(&dbPN); err == nil && dbPN != "" {
			lidCacheLock.Lock()
			lidToPnRAM[cleanLID] = dbPN
			lidCacheLock.Unlock()
			return FormatPhone(dbPN)
		}
	}
	return ""
}

var (
	restoreGuardMu sync.Mutex
	lastRestoreTry time.Time
)

// EnsureSessionRestoredFromRAM self-heals a delayed session restore if RAM holds a saved Central Node session
// while the local whatsmeow client is still unpaired.
func EnsureSessionRestoredFromRAM(ws ramstore.WhatsAppSessionState) error {
	if !ws.HasSession || strings.TrimSpace(ws.SessionBlobBase64) == "" {
		return nil
	}
	restoreGuardMu.Lock()
	if time.Since(lastRestoreTry) < 8*time.Second {
		restoreGuardMu.Unlock()
		return nil
	}
	lastRestoreTry = time.Now()
	restoreGuardMu.Unlock()

	client := GetVendorClient()
	if client != nil && client.Store != nil && client.Store.ID != nil {
		return nil
	}
	return BootstrapWhatsAppEngine(&central.BootstrapWhatsAppSession{
		HasSession:        ws.HasSession,
		Status:            ws.Status,
		DeviceJID:         ws.DeviceJID,
		Phone:             ws.Phone,
		PushName:          ws.PushName,
		SessionBlobBase64: ws.SessionBlobBase64,
	})
}

// BootstrapWhatsAppEngine initializes the ephemeral session container from Central Node's response.
// If Central Node returned a saved session, resets any stale unpaired client singleton, restores SQLite, and auto-connects immediately.
// Otherwise marks WhatsApp as Needs_Login so the vendor can scan QR or enter 8-digit pairing code from dashboard.
func BootstrapWhatsAppEngine(centralSession *central.BootstrapWhatsAppSession) error {
	hasCentralBlob := centralSession != nil && centralSession.HasSession && strings.TrimSpace(centralSession.SessionBlobBase64) != ""

	// Protect an already-paired live WhatsApp client if Central returned no session (e.g. cold-start failure)
	ClientsLock.Lock()
	existingClient := vendorClient
	existingPaired := existingClient != nil && existingClient.Store != nil && existingClient.Store.ID != nil
	if !hasCentralBlob && existingPaired {
		ClientsLock.Unlock()
		log.Printf("📱 [WhatsApp Boot] Keeping active local paired session for +%s.", existingClient.Store.ID.User)
		return nil
	}
	// Reset any stale/unpaired vendorClient BEFORE replacing StoreContainer so GetVendorClient() binds to the newly restored SQLite DB!
	if existingClient != nil {
		existingClient.Disconnect()
		vendorClient = nil
	}
	currentQRCode = ""
	ClientsLock.Unlock()

	if err := InitEphemeralSessionStore(centralSession); err != nil {
		return err
	}

	if hasCentralBlob {
		client := GetVendorClient()
		if client != nil && client.Store != nil && client.Store.ID != nil {
			log.Printf("📱 [WhatsApp Boot] Restored paired session for +%s. Auto-connecting...", client.Store.ID.User)
			ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
				ws.HasSession = true
				ws.NeedsLogin = false
				ws.Status = "Connecting"
				ws.DeviceJID = client.Store.ID.String()
				ws.Phone = FormatPhone(client.Store.ID.User)
				if client.Store.PushName != "" {
					ws.PushName = client.Store.PushName
				}
				ws.QRCode = ""
			})
			ConnectWithAutoRetry(client, ramstore.Global.GetVendor().Username)
			return nil
		}
		log.Println("⚠️ [WhatsApp Boot] Session blob restored to SQLite but no paired device row found inside DB; entering Needs_Login.")
	}

	// No valid saved session -> inform dashboard that WhatsApp login is required
	ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
		ws.HasSession = false
		ws.NeedsLogin = true
		ws.Status = "Needs_Login"
		ws.DeviceJID = ""
		ws.Phone = ""
		ws.QRCode = ""
	})
	log.Println("ℹ️ [WhatsApp Boot] Central Brain Node reports no saved WhatsApp session. Dashboard Login (QR / Pairing Code) is ready.")
	return nil
}

// GetVendorClient returns (or lazily creates) the single-vendor whatsmeow client.
func GetVendorClient() *whatsmeow.Client {
	ClientsLock.Lock()
	defer ClientsLock.Unlock()

	if vendorClient != nil {
		return vendorClient
	}
	if StoreContainer == nil {
		if err := InitEphemeralSessionStore(nil); err != nil {
			log.Printf("⚠️ WhatsApp ephemeral store init failed: %v", err)
			return nil
		}
	}

	deviceStore, err := StoreContainer.GetFirstDevice(context.Background())
	if err != nil || deviceStore == nil {
		deviceStore = StoreContainer.NewDevice()
	}

	vendorUser := ramstore.Global.GetVendor().Username
	if vendorUser == "" {
		vendorUser = "vendor"
	}

	clientLog := waLog.Stdout("VendorWA", "ERROR", false)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	client.AddEventHandler(func(evt interface{}) {
		handleWhatsAppEvents(vendorUser, client, evt)
	})

	vendorClient = client
	return client
}

// GetOrCreateClient alias kept for clean handler ergonomics.
func GetOrCreateClient(_ string) *whatsmeow.Client {
	return GetVendorClient()
}

// GetAnyLoggedInClient returns the vendor client if logged in.
func GetAnyLoggedInClient() *whatsmeow.Client {
	c := GetVendorClient()
	if c != nil && c.IsLoggedIn() {
		return c
	}
	return nil
}

// GetOrGenerateQR initiates a fresh QR channel for an unlinked vendor bot and returns the QR string.
func GetOrGenerateQR(_ string) string {
	ClientsLock.Lock()
	client := vendorClient
	existingQR := currentQRCode
	ClientsLock.Unlock()

	if client != nil && client.Store != nil && client.Store.ID != nil {
		if !client.IsConnected() {
			ConnectWithAutoRetry(client, ramstore.Global.GetVendor().Username)
		}
		return ""
	}
	if existingQR != "" {
		return existingQR
	}

	if client != nil {
		client.Disconnect()
	}
	if StoreContainer == nil {
		_ = InitEphemeralSessionStore(nil)
	}
	if StoreContainer == nil {
		return ""
	}

	deviceStore := StoreContainer.NewDevice()
	vendorUser := ramstore.Global.GetVendor().Username
	client = whatsmeow.NewClient(deviceStore, waLog.Stdout("VendorWA", "ERROR", false))
	client.AddEventHandler(func(evt interface{}) {
		handleWhatsAppEvents(vendorUser, client, evt)
	})

	qrChan, err := client.GetQRChannel(context.Background())
	if err != nil {
		log.Printf("⚠️ GetQRChannel error: %v", err)
		return ""
	}

	ClientsLock.Lock()
	vendorClient = client
	currentQRCode = ""
	ClientsLock.Unlock()

	go func() {
		for evt := range qrChan {
			if evt.Event == "code" {
				ClientsLock.Lock()
				currentQRCode = evt.Code
				ClientsLock.Unlock()
				ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
					ws.Status = "QR_Ready"
					ws.QRCode = evt.Code
					ws.NeedsLogin = true
				})
			} else if evt.Event == "success" {
				ClientsLock.Lock()
				currentQRCode = ""
				ClientsLock.Unlock()
				if client.Store != nil && client.Store.ID != nil {
					go SyncSessionOnFirstLogin(client.Store.ID.String(), client.Store.ID.User, client.Store.PushName, "Connected")
				}
			} else if evt.Event == "timeout" {
				ClientsLock.Lock()
				currentQRCode = ""
				ClientsLock.Unlock()
				ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
					if !ws.HasSession {
						ws.Status = "Needs_Login"
						ws.QRCode = ""
					}
				})
			}
		}
	}()

	go func() {
		if err := client.Connect(); err != nil {
			log.Printf("⚠️ WhatsApp connect for QR notice: %v", err)
		}
	}()

	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		ClientsLock.Lock()
		code := currentQRCode
		ClientsLock.Unlock()
		if code != "" {
			return code
		}
	}

	ClientsLock.Lock()
	defer ClientsLock.Unlock()
	return currentQRCode
}

// GetCurrentQRCode returns the currently active QR string (if any).
func GetCurrentQRCode() string {
	ClientsLock.Lock()
	defer ClientsLock.Unlock()
	return currentQRCode
}

// DisconnectAndUnlinkClient cleanly logs out, wipes ephemeral session file, and deletes saved session on Central Node.
func DisconnectAndUnlinkClient(_ string) {
	ClientsLock.Lock()
	client := vendorClient
	vendorClient = nil
	currentQRCode = ""
	ClientsLock.Unlock()

	if client != nil {
		if client.IsConnected() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = client.Logout(ctx)
			cancel()
		}
		client.Disconnect()
		if client.Store != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = client.Store.Delete(ctx)
			cancel()
		}
	}

	ResetEphemeralSessionFile()

	ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
		ws.HasSession = false
		ws.NeedsLogin = true
		ws.Status = "Needs_Login"
		ws.DeviceJID = ""
		ws.Phone = ""
		ws.PushName = ""
		ws.QRCode = ""
		ws.SessionBlobBase64 = ""
	})

	go func() {
		if err := central.Client.DeleteWhatsAppSession(); err != nil {
			log.Printf("⚠️ [Session Delete] Central Node notify note: %v", err)
		}
	}()

	log.Println("🔌 [WhatsApp Gateway] Device unlinked; ephemeral session wiped & Central Node notified.")
}

// ConnectWithAutoRetry connects the whatsmeow websocket with exponential retry.
func ConnectWithAutoRetry(client *whatsmeow.Client, username string) {
	if client == nil || client.IsConnected() {
		return
	}
	go func() {
		for attempt := 1; attempt <= 8; attempt++ {
			if client.IsConnected() {
				return
			}
			err := client.Connect()
			if err == nil || strings.Contains(err.Error(), "already connected") {
				for w := 0; w < 15; w++ {
					if client.IsConnected() {
						_ = client.SendPresence(context.Background(), types.PresenceAvailable)
						log.Printf("✅ WhatsApp WebSocket connected for vendor @%s", username)
						return
					}
					time.Sleep(200 * time.Millisecond)
				}
			}
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}
	}()
}

// EnsureConnected ensures WhatsApp websocket is connected before sending.
func EnsureConnected(client *whatsmeow.Client) error {
	if client == nil {
		return fmt.Errorf("whatsapp client is nil")
	}
	if client.IsConnected() {
		return nil
	}
	_ = client.Connect()
	for i := 0; i < 25; i++ {
		if client.IsConnected() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if client.IsConnected() {
		return nil
	}
	return fmt.Errorf("whatsapp connection timeout")
}

// StartKeepAliveMonitor keeps a paired vendor session connected in the background.
func StartKeepAliveMonitor() {
	go func() {
		ticker := time.NewTicker(45 * time.Second)
		for range ticker.C {
			c := GetVendorClient()
			if c != nil && c.Store != nil && c.Store.ID != nil && !c.IsConnected() {
				log.Println("🔄 [KeepAlive] Reconnecting vendor WhatsApp WebSocket...")
				ConnectWithAutoRetry(c, ramstore.Global.GetVendor().Username)
			}
		}
	}()
}

// FormatPhone normalizes phone numbers cleanly.
func FormatPhone(phone string) string {
	return ramstore.NormalizePhone(phone)
}

// ResolveRealPhoneNumber resolves real phone numbers (+91/global) from whatsmeow JIDs & LIDs.
func ResolveRealPhoneNumber(_ *whatsmeow.Client, senderJID, chatJID types.JID) string {
	if senderJID.Server == "lid" || strings.HasPrefix(senderJID.User, "117") || strings.HasPrefix(senderJID.User, "223") || strings.HasPrefix(senderJID.User, "100") {
		if realPhone := GetRealPhoneFromLID(senderJID.User); realPhone != "" {
			return realPhone
		}
	}
	if chatJID.Server == "lid" || strings.HasPrefix(chatJID.User, "117") || strings.HasPrefix(chatJID.User, "223") || strings.HasPrefix(chatJID.User, "100") {
		if realPhone := GetRealPhoneFromLID(chatJID.User); realPhone != "" {
			return realPhone
		}
	}

	if (chatJID.Server == types.DefaultUserServer || chatJID.Server == "c.us" || chatJID.Server == "s.whatsapp.net") && chatJID.User != "" {
		clean := regexp.MustCompile(`\D`).ReplaceAllString(strings.Split(chatJID.User, ".")[0], "")
		if len(clean) >= 8 && !strings.HasPrefix(clean, "117") && !strings.HasPrefix(clean, "223") {
			return FormatPhone(clean)
		}
	}
	if (senderJID.Server == types.DefaultUserServer || senderJID.Server == "c.us" || senderJID.Server == "s.whatsapp.net") && senderJID.User != "" {
		clean := regexp.MustCompile(`\D`).ReplaceAllString(strings.Split(senderJID.User, ".")[0], "")
		if len(clean) >= 8 && !strings.HasPrefix(clean, "117") && !strings.HasPrefix(clean, "223") {
			return FormatPhone(clean)
		}
	}

	rawUser := senderJID.User
	if rawUser == "" || senderJID.Server == "lid" {
		rawUser = chatJID.User
	}
	clean := regexp.MustCompile(`\D`).ReplaceAllString(strings.Split(rawUser, ".")[0], "")
	if len(clean) >= 8 {
		return FormatPhone(clean)
	}
	return ""
}

// SaveCustomerChatAndPrune stores only the last 10 messages per customer strictly in bot RAM (zero Central Node chat sync).
func SaveCustomerChatAndPrune(username, customerPhone, role, messageText string) {
	cleanPhone := FormatPhone(customerPhone)
	cleanMsg := strings.TrimSpace(messageText)
	if cleanPhone == "" || cleanMsg == "" {
		return
	}

	normalRole := strings.ToLower(strings.TrimSpace(role))
	direction := "incoming"
	if normalRole == "user" || normalRole == "client" || normalRole == "customer" || normalRole == "incoming" {
		normalRole = "customer"
		direction = "incoming"
	} else if normalRole == "outgoing_manual" || normalRole == "manual" {
		normalRole = "assistant"
		direction = "outgoing_manual"
	} else {
		normalRole = "assistant"
		direction = "outgoing_ai"
	}

	ramstore.Global.AppendChat(cleanPhone, normalRole, direction, cleanMsg)
	_ = updateSlidingWindowRAM(username, cleanPhone, normalRole, cleanMsg)
}

// PushToLiveChat pushes real-time chat events to connected dashboard SSE streams.
func PushToLiveChat(_, customerPhone, text, msgType string) {
	payload := map[string]string{
		"phone": FormatPhone(customerPhone),
		"text":  text,
		"type":  msgType,
		"time":  time.Now().Format("15:04"),
	}
	StreamLock.Lock()
	defer StreamLock.Unlock()
	for _, ch := range LiveChatStreams {
		select {
		case ch <- payload:
		default:
		}
	}
}

// TriggerConfirmedPaymentLead alerts vendor owner when payment is verified.
func TriggerConfirmedPaymentLead(vendorUser, customerPhone, orderID, amount, txnID string) {
	notifPhone := FormatPhone(ramstore.Global.GetVendor().NotificationPhone)
	if notifPhone == "" {
		return
	}
	client := GetAnyLoggedInClient()
	if client == nil || !client.IsLoggedIn() {
		return
	}
	msg := fmt.Sprintf("💰 *PAYMENT RECEIVED!* ✅\n\n• Order ID: *#%s*\n• Customer: +%s\n• Amount: *₹%s*\n• Txn ID: %s\n\nOrder has been synced to Central Brain Node.", orderID, customerPhone, amount, txnID)
	_ = SendMessage(client, notifPhone, msg)
	PushToLiveChat(vendorUser, notifPhone, msg, "lead_alert")
}

// --- Message Inspection Helpers ---

func extractIncomingImageMessage(msg *waE2E.Message) *waE2E.ImageMessage {
	if msg == nil {
		return nil
	}
	if img := msg.GetImageMessage(); img != nil {
		return img
	}
	if ephem := msg.GetEphemeralMessage(); ephem != nil && ephem.GetMessage() != nil {
		return extractIncomingImageMessage(ephem.GetMessage())
	}
	if vo := msg.GetViewOnceMessage(); vo != nil && vo.GetMessage() != nil {
		return extractIncomingImageMessage(vo.GetMessage())
	}
	if vo2 := msg.GetViewOnceMessageV2(); vo2 != nil && vo2.GetMessage() != nil {
		return extractIncomingImageMessage(vo2.GetMessage())
	}
	return nil
}

func extractLocationCoords(msg *waE2E.Message) (float64, float64, bool) {
	if msg == nil {
		return 0, 0, false
	}
	if loc := msg.GetLocationMessage(); loc != nil {
		return loc.GetDegreesLatitude(), loc.GetDegreesLongitude(), true
	}
	if live := msg.GetLiveLocationMessage(); live != nil {
		return live.GetDegreesLatitude(), live.GetDegreesLongitude(), true
	}
	if ephem := msg.GetEphemeralMessage(); ephem != nil && ephem.GetMessage() != nil {
		return extractLocationCoords(ephem.GetMessage())
	}
	return 0, 0, false
}

func extractIncomingAudioMessage(msg *waE2E.Message) *waE2E.AudioMessage {
	if msg == nil {
		return nil
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return aud
	}
	if ephem := msg.GetEphemeralMessage(); ephem != nil && ephem.GetMessage() != nil {
		return extractIncomingAudioMessage(ephem.GetMessage())
	}
	return nil
}

// Voice note transcription is handled by the Octopus Voice Hand (`ExecuteVoiceNoteHand` in voice_router.go).

func extractMessageText(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if s := strings.TrimSpace(msg.GetConversation()); s != "" {
		return s
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil && strings.TrimSpace(ext.GetText()) != "" {
		return strings.TrimSpace(ext.GetText())
	}
	if img := msg.GetImageMessage(); img != nil && strings.TrimSpace(img.GetCaption()) != "" {
		return strings.TrimSpace(img.GetCaption())
	}
	if vid := msg.GetVideoMessage(); vid != nil && strings.TrimSpace(vid.GetCaption()) != "" {
		return strings.TrimSpace(vid.GetCaption())
	}
	if doc := msg.GetDocumentMessage(); doc != nil && strings.TrimSpace(doc.GetCaption()) != "" {
		return strings.TrimSpace(doc.GetCaption())
	}
	if ord := msg.GetOrderMessage(); ord != nil {
		return fmt.Sprintf("[WhatsApp Catalog Order]: %s (%d items)", ord.GetOrderTitle(), ord.GetItemCount())
	}
	if prod := msg.GetProductMessage(); prod != nil && prod.GetProduct() != nil {
		return fmt.Sprintf("[WhatsApp Catalog Product Inquiry]: %s", prod.GetProduct().GetTitle())
	}
	if ephem := msg.GetEphemeralMessage(); ephem != nil && ephem.GetMessage() != nil {
		return extractMessageText(ephem.GetMessage())
	}
	if vo := msg.GetViewOnceMessage(); vo != nil && vo.GetMessage() != nil {
		return extractMessageText(vo.GetMessage())
	}
	if vo2 := msg.GetViewOnceMessageV2(); vo2 != nil && vo2.GetMessage() != nil {
		return extractMessageText(vo2.GetMessage())
	}
	return ""
}

func reverseGeocodeNominatim(lat, lng float64) string {
	client := &http.Client{Timeout: 4 * time.Second}
	url := fmt.Sprintf("https://nominatim.openstreetmap.org/reverse?format=json&lat=%.6f&lon=%.6f&zoom=18", lat, lng)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "VendorWhatsAppBotNode/2.0")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var parsed struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err == nil {
		return strings.TrimSpace(parsed.DisplayName)
	}
	return ""
}

// --- Core WhatsApp Event Handler ---

func handleWhatsAppEvents(username string, client *whatsmeow.Client, evt interface{}) {
	switch v := evt.(type) {
	case *events.Receipt:
		if v.Type == types.ReceiptTypeRead || v.Type == types.ReceiptTypeReadSelf || strings.Contains(string(v.Type), "read") {
			for _, id := range v.MessageIDs {
				MessageReadByHuman.Store(string(id), true)
			}
		}

	case *events.PairSuccess:
		// Trigger #1 of 3 for session persistence: save WhatsMeow session ONCE on first-time QR/pairing login completion.
		if client != nil {
			_ = client.SendPresence(context.Background(), types.PresenceAvailable)
			if client.Store != nil && client.Store.ID != nil {
				ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
					ws.HasSession = true
					ws.NeedsLogin = false
					ws.Status = "Connected"
					ws.DeviceJID = client.Store.ID.String()
					ws.Phone = FormatPhone(client.Store.ID.User)
					if client.Store.PushName != "" {
						ws.PushName = client.Store.PushName
					}
					ws.QRCode = ""
				})
				PushToLiveChat(username, "", "whatsapp_paired", "ram_synced")
				go SyncSessionOnFirstLogin(client.Store.ID.String(), client.Store.ID.User, client.Store.PushName, "Connected")
			}
		}

	case *events.Connected:
		if client != nil {
			_ = client.SendPresence(context.Background(), types.PresenceAvailable)
			if client.Store != nil && client.Store.ID != nil {
				ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
					ws.HasSession = true
					ws.NeedsLogin = false
					ws.Status = "Connected"
					ws.DeviceJID = client.Store.ID.String()
					ws.Phone = FormatPhone(client.Store.ID.User)
					if client.Store.PushName != "" {
						ws.PushName = client.Store.PushName
					}
					ws.QRCode = ""
				})
				PushToLiveChat(username, "", "whatsapp_connected", "ram_synced")
				// Ensure first-time pairing (QR or 8-digit phone code) is definitely uploaded once Signal keys are ready
				if !initialPairUploaded.Load() {
					go SyncSessionOnFirstLogin(client.Store.ID.String(), client.Store.ID.User, client.Store.PushName, "Connected")
				}
			}
		}

	case *events.HistorySync:
		// Strictly local — do not upload session on history sync bursts.

	case *events.Disconnected:
		ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
			if ws.HasSession {
				ws.Status = "Connecting"
			}
		})

	case *events.LoggedOut:
		log.Println("⚠️ [WhatsApp Event] Device logged out from phone. Clearing session and notifying Central Brain Node.")
		DisconnectAndUnlinkClient(username)
		PushToLiveChat(username, "", "WhatsApp session logged out from phone", "session_unlinked")

	case *events.Message:
		// Anti-Ban Guard: strictly ignore self, group chats, WhatsApp Channels (newsletter), and Status broadcasts
		chatStr := strings.ToLower(v.Info.Chat.String())
		if v.Info.IsFromMe ||
			v.Info.IsGroup ||
			v.Info.Chat.Server == types.BroadcastServer ||
			v.Info.Chat.Server == types.GroupServer ||
			v.Info.Chat.Server == "newsletter" ||
			chatStr == "status@broadcast" {
			return
		}

		senderPhone := ResolveRealPhoneNumber(client, v.Info.Sender, v.Info.Chat)
		if senderPhone == "" {
			return
		}

		lidCacheLock.Lock()
		phoneToJID[senderPhone] = v.Info.Chat
		lidCacheLock.Unlock()

		// Ensure customer exists in RAM & sync to Central ONLY if brand-new or profile details changed
		if saved, changed := ramstore.Global.UpsertCustomerWithChangeCheck(&ramstore.Customer{
			WANumber: senderPhone,
			JID:      v.Info.Chat.String(),
			FullName: strings.TrimSpace(v.Info.PushName),
			Source:   "whatsapp",
		}); changed && saved != nil {
			central.Client.PushCustomerAsync(saved)
		}

		// 1. Handle GPS Location Pin (read after 1s delay)
		if lat, lng, isLoc := extractLocationCoords(v.Message); isLoc {
			go func(id types.MessageID, ts time.Time, chat, sender types.JID, phone string, la, ln float64) {
				time.Sleep(MessageReadDelay)
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				_ = client.MarkRead(ctx, []types.MessageID{id}, ts, chat, sender)
				cancel()
				handleIncomingLocationPin(username, client, phone, la, ln)
			}(v.Info.ID, v.Info.Timestamp, v.Info.Chat, v.Info.Sender, senderPhone, lat, lng)
			return
		}

		incomingText := extractMessageText(v.Message)

		// 2. Handle Voice Note / Audio Message (respects in-flight split guard + 1s read delay)
		if audMsg := extractIncomingAudioMessage(v.Message); audMsg != nil {
			handleIncomingCustomerTurn(username, client, v.Info.ID, v.Info.Timestamp, v.Info.Chat, v.Info.Sender, v.Message, senderPhone, "", audMsg)
			return
		}

		// 3. Handle General Customer Image (NO payment screenshot / UTR scanning — Central Brain handles 100% of payment verification)
		if imgMsg := extractIncomingImageMessage(v.Message); imgMsg != nil {
			if incomingText == "" {
				incomingText = "[Customer shared an image — politely ask which product or detail they need help with; note that online payments are verified automatically by our Central payment system]"
			} else {
				incomingText = fmt.Sprintf("[Customer shared an image with caption]: %s", incomingText)
			}
		}

		if incomingText == "" {
			return
		}

		handleIncomingCustomerTurn(username, client, v.Info.ID, v.Info.Timestamp, v.Info.Chat, v.Info.Sender, v.Message, senderPhone, incomingText, nil)
	}
}

// handleIncomingCustomerTurn enforces:
//   - If the customer sends another message BEFORE the bot finishes replying to their previous message,
//     we IGNORE it (zero AI/Whisper tokens) and reply once with a hardcoded peaceful WhatsApp reply-quoted notice.
//   - Otherwise, we claim the customer's turn lock, wait 1 second before reading (blue ticks), and process the message.
func handleIncomingCustomerTurn(username string, client *whatsmeow.Client, msgID types.MessageID, msgTime time.Time, chatJID, senderJID types.JID, rawMsg *waE2E.Message, phone, text string, audMsg *waE2E.AudioMessage) {
	customerInFlightLock.Lock()
	if customerInFlightMap[phone] {
		alreadyNotified := splitNoticeSentMap[phone]
		if !alreadyNotified {
			splitNoticeSentMap[phone] = true
		}
		customerInFlightLock.Unlock()

		log.Printf("🛑 [Split-Message Ignored] +%s sent an extra message before bot replied (notified=%v)", phone, !alreadyNotified)
		if !alreadyNotified {
			go func() {
				time.Sleep(MessageReadDelay)
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				_ = client.MarkRead(ctx, []types.MessageID{msgID}, msgTime, chatJID, senderJID)
				cancel()
				if errSend := SendReplyMessage(client, phone, SplitMessagePeacefulNotice, msgID, senderJID, rawMsg); errSend != nil {
					_ = SendMessage(client, phone, SplitMessagePeacefulNotice)
				}
				PushToLiveChat(username, phone, SplitMessagePeacefulNotice, "outgoing_ai")
			}()
		}
		return
	}
	customerInFlightMap[phone] = true
	splitNoticeSentMap[phone] = false
	customerInFlightLock.Unlock()

	go processSingleCustomerTurn(username, client, msgID, msgTime, chatJID, senderJID, rawMsg, phone, text, audMsg)
}

func hasPriorAssistantReplyInRAM(phone string) bool {
	for _, rec := range ramstore.Global.GetChatMessages(FormatPhone(phone)) {
		if rec.Role == "assistant" || rec.Direction == "outgoing_ai" || rec.Direction == "outgoing_manual" {
			return true
		}
	}
	return false
}

func replyAlreadyHasSingleMsgTip(reply string) bool {
	low := strings.ToLower(reply)
	return strings.Contains(low, "tukdo") ||
		strings.Contains(low, "ek sath") ||
		strings.Contains(low, "ek saath") ||
		strings.Contains(low, "ek hi message") ||
		strings.Contains(low, "one message") ||
		strings.Contains(low, "single message")
}

func shouldUseWhatsAppQuote(octResp *OctopusAIResponse, incomingText string) bool {
	if octResp != nil && octResp.QuoteReply {
		return true
	}
	// Quote when customer asked a long specific multi-part question (>60 chars with '?')
	if len([]rune(strings.TrimSpace(incomingText))) > 60 && strings.Contains(incomingText, "?") {
		return true
	}
	return false
}

func processSingleCustomerTurn(username string, client *whatsmeow.Client, msgID types.MessageID, msgTime time.Time, chatJID, senderJID types.JID, rawMsg *waE2E.Message, phone, text string, audMsg *waE2E.AudioMessage) {
	defer func() {
		customerInFlightLock.Lock()
		delete(customerInFlightMap, phone)
		delete(splitNoticeSentMap, phone)
		customerInFlightLock.Unlock()
	}()

	// 1-second deliberate read delay before reading (blue-ticking) the customer's message
	time.Sleep(MessageReadDelay)

	// Human override check (if vendor already read on phone during the 1s delay)
	if _, readByHuman := MessageReadByHuman.Load(string(msgID)); readByHuman {
		if text != "" {
			SaveCustomerChatAndPrune(username, phone, "customer", text)
		}
		return
	}

	// Mark message read (Blue Ticks) strictly after the 1-second delay
	ctxRead, cancelRead := context.WithTimeout(context.Background(), 4*time.Second)
	_ = client.MarkRead(ctxRead, []types.MessageID{msgID}, msgTime, chatJID, senderJID)
	cancelRead()

	cfg := GetUserConfigRAM(username)

	// Transcribe voice note via Octopus Voice Hand (Gemini Live 2-model | Groq Whisper 2-model | Dual Rotation)
	aiInputText := text
	var voiceHandResult *VoiceNoteHandResult
	if audMsg != nil {
		audioBytes, dlErr := client.Download(context.Background(), audMsg)
		transcript := ""
		if dlErr == nil && len(audioBytes) > 0 {
			vh := ExecuteVoiceNoteHand(username, cfg, audioBytes, audMsg.GetMimetype(), audMsg.GetSeconds())
			voiceHandResult = &vh
			transcript = strings.TrimSpace(vh.Transcript)
		}
		if transcript != "" {
			text = fmt.Sprintf("🎙️ [Voice Note]: %s", transcript)
			aiInputText = transcript
		} else {
			text = fmt.Sprintf("🎙️ [Voice Note %ds — audio unclear]", audMsg.GetSeconds())
			aiInputText = fmt.Sprintf("Customer sent a %ds voice note that was unclear or silent. Politely ask them to type the product name or send a clearer voice note.", audMsg.GetSeconds())
		}
	}

	if strings.TrimSpace(aiInputText) == "" {
		return
	}

	// Check whether this is the customer's very first bot interaction so we share the single-message tip at the start
	isFirstTurn := !hasPriorAssistantReplyInRAM(phone)

	// Persist incoming customer message immediately into RAM (last 10 msgs/customer) BEFORE SSE push
	SaveCustomerChatAndPrune(username, phone, "customer", text)
	PushToLiveChat(username, phone, text, "incoming")
	go ExtractAndPersistCustomerDetails(username, phone, aiInputText)

	// Central Admin Feature Gating & Subscription Validity Check
	if allowed, reason := ramstore.Global.CanAutoReply(); !allowed {
		log.Printf("⏸️ [Auto-Reply Paused] +%s — %s", phone, reason)
		return
	}

	// Store Open / Holiday Mode Check
	vendor := ramstore.Global.GetVendor()
	if isOpen, closedMsg := vendor.IsStoreOpenNow(); !isOpen {
		closedNoticeMu.Lock()
		lastSent, exists := closedNoticeSent[phone]
		shouldSend := !exists || time.Since(lastSent) > 20*time.Minute
		if shouldSend {
			closedNoticeSent[phone] = time.Now()
		}
		closedNoticeMu.Unlock()
		if shouldSend {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("🕒 *%s — Store Currently Closed*\n\n%s", vendor.BusinessName, closedMsg))
			if strings.TrimSpace(vendor.OperatingHours) != "" {
				sb.WriteString(fmt.Sprintf("\n\n⏰ *Operating Hours:* %s", vendor.OperatingHours))
			}
			if strings.TrimSpace(vendor.NextOpenTime) != "" {
				sb.WriteString(fmt.Sprintf("\n🔔 *Reopens At:* %s", vendor.NextOpenTime))
			}
			sb.WriteString("\n\nAap apna message chhod sakte hain — store khulte hi hum aapko turant reply karenge! 🙏")
			reply := sb.String()
			_ = SendMessage(client, phone, reply)
			SaveCustomerChatAndPrune(username, phone, "assistant", reply)
			PushToLiveChat(username, phone, reply, "outgoing_ai")
		}
		return
	}

	// Natural typing indicator loop (Anti-Ban human-like pacing)
	stopTyping := make(chan struct{})
	go func() {
		_ = client.SendChatPresence(context.Background(), chatJID, types.ChatPresenceComposing, types.ChatPresenceMediaText)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopTyping:
				_ = client.SendChatPresence(context.Background(), chatJID, types.ChatPresencePaused, types.ChatPresenceMediaText)
				return
			case <-ticker.C:
				_ = client.SendChatPresence(context.Background(), chatJID, types.ChatPresenceComposing, types.ChatPresenceMediaText)
			}
		}
	}()

	octResp, err := GenerateOctopusAIReply(username, phone, aiInputText, cfg)
	// Slight natural human typing pause (450ms) before stopping typing & delivering message
	time.Sleep(450 * time.Millisecond)
	close(stopTyping)

	if err != nil || octResp == nil {
		log.Printf("⚠️ [Octopus AI] Error for +%s: %v", phone, err)
		return
	}
	if voiceHandResult != nil && voiceHandResult.Transcript != "" {
		octResp.Actions = append([]AIActionItem{{
			Hand: "voice_note_transcribe",
			Payload: &AIActionPayload{
				Query:    voiceHandResult.Transcript,
				Category: voiceHandResult.Provider + ":" + voiceHandResult.Model,
			},
		}}, octResp.Actions...)
	}

	// Execute Octopus Actions (cart variants/stock, QR, PDF, magic links, owner escalation)
	executeOctopusActions(username, client, phone, chatJID, octResp, cfg)

	// Guarantee polite single-message guidance on the very first reply to this customer
	replyText := strings.TrimSpace(octResp.AIReplyText)
	if replyText != "" && isFirstTurn && !replyAlreadyHasSingleMsgTip(replyText) {
		replyText += FirstTurnSingleMessageTip
	}

	// Deliver AI reply text using WhatsApp inbuilt quote ONLY when needed (humanoid flow)
	if replyText != "" {
		if shouldUseWhatsAppQuote(octResp, text) {
			if errSend := SendReplyMessage(client, phone, replyText, msgID, senderJID, rawMsg); errSend != nil {
				_ = SendMessage(client, phone, replyText)
			}
		} else {
			_ = SendMessage(client, phone, replyText)
		}
		SaveCustomerChatAndPrune(username, phone, "assistant", replyText)
		PushToLiveChat(username, phone, replyText, "outgoing_ai")
	}
}

func handleIncomingLocationPin(username string, client *whatsmeow.Client, senderPhone string, lat, lng float64) {
	addr := reverseGeocodeNominatim(lat, lng)
	if addr == "" {
		addr = fmt.Sprintf("GPS Location (%.5f, %.5f)", lat, lng)
	}
	_ = UpdateCustomerCoordinates(username, senderPhone, lat, lng, addr)

	bizLat, bizLng, maxRad, freeRad, delCharge, _ := GetBusinessLocation(username)
	distKM := CalculateDistance(bizLat, bizLng, lat, lng)

	var reply string
	if distKM > maxRad {
		reply = fmt.Sprintf("📍 *Location Received:* %s\n\nMaaf kijiye, aapki distance humare store se *%.1f km* hai aur humari maximum delivery range *%.1f km* tak hai. Kripya store owner se sampark karein. 🙏", addr, distKM, maxRad)
	} else if distKM <= freeRad {
		reply = fmt.Sprintf("📍 *Location Verified!* ✅\n\n• Address: %s\n• Distance: *%.1f km*\n• Delivery Charge: *FREE* 🎉\n\nAb aap apna order confirm kar sakte hain! 🛒", addr, distKM)
	} else {
		reply = fmt.Sprintf("📍 *Location Verified!* ✅\n\n• Address: %s\n• Distance: *%.1f km*\n• Delivery Charge: *₹%.2f*\n\nAb aap apna order confirm kar sakte hain! 🛒", addr, distKM, delCharge)
	}

	_ = SendMessage(client, senderPhone, reply)
	PushToLiveChat(username, senderPhone, reply, "outgoing_ai")
	go SaveCustomerChatAndPrune(username, senderPhone, "assistant", reply)
}

func executeOctopusActions(username string, client *whatsmeow.Client, phone string, chatJID types.JID, octResp *OctopusAIResponse, cfg *UserConfigRAM) {
	_ = chatJID
	_ = cfg
	for _, act := range octResp.Actions {
		switch act.Hand {
		case "send_catalog_images":
			cat := "all"
			if act.Payload != nil && strings.TrimSpace(act.Payload.Category) != "" {
				cat = strings.TrimSpace(act.Payload.Category)
			}
			go sendCatalogImagesFromRAM(client, phone, cat)

		case "update_cart":
			if act.Payload != nil && len(act.Payload.CartItems) > 0 {
				var items []AICartItem
				for _, ci := range act.Payload.CartItems {
					items = append(items, AICartItem{ProductID: ci.ID, Variant: ci.Variant, Qty: int(ci.Qty)})
				}
				rejected, _ := AddItemsToActiveCartWithStockCheck(username, phone, items)
				if len(rejected) > 0 {
					octResp.AIReplyText += "\n\n⚠️ *Stock Update:*\n• " + strings.Join(rejected, "\n• ")
				}
			}

		case "extract_profile":
			if act.Payload != nil && act.Payload.Profile != nil {
				_ = UpdateCustomerProfileFromData(username, phone, &AIProfileData{
					Name:          act.Payload.Profile.Name,
					Address:       act.Payload.Profile.Address,
					ContactNumber: act.Payload.Profile.Contact,
				})
			}

		// Central Brain controls 100% of Order Confirmation & Online Payment (QR/Link/Verification).
		// Legacy "generate_payment_qr" & "generate_bill_pdf" hands safely route to Central order submission too.
		case "submit_order", "generate_payment_qr", "generate_bill_pdf":
			mode := "ONLINE"
			if act.Hand == "generate_bill_pdf" {
				mode = "COD"
			}
			if act.Payload != nil && strings.TrimSpace(act.Payload.PaymentMode) != "" {
				mode = strings.TrimSpace(act.Payload.PaymentMode)
			}
			replyFromCentral, _, _ := SubmitWhatsAppOrderToCentral(client, username, phone, mode)
			if replyFromCentral == "__QR_IMAGE_SENT__" {
				// Strict QR-only rule: ONLY the Branded Banner QR image is sent (no follow-up text or payment link)
				octResp.AIReplyText = ""
			} else if strings.TrimSpace(replyFromCentral) != "" {
				octResp.AIReplyText = replyFromCentral
			}

		case "generate_store_link":
			go func() {
				time.Sleep(300 * time.Millisecond)
				_ = ExecuteGenerateStoreLinkHand(client, username, phone)
			}()

		case "clear_cart":
			_ = ClearActiveCart(username, phone)
			_ = SetCustomerState(username, phone, "chatting")

		case "cancel_order":
			ordID := GetCustomerCurrentOrderID(username, phone)
			if ordID != "" {
				StopPaymentVerificationPoller(ordID)
			}
			if ord := ramstore.Global.GetOrder(ordID); ord != nil {
				ord.OrderStatus = "CANCELLED"
				ord.PaymentStatus = "CANCELLED"
				central.Client.PushOrderAsync(ramstore.Global.UpsertOrder(ord))
			}
			_ = ClearActiveCart(username, phone)
			_ = SetCustomerState(username, phone, "chatting")
			_ = SetCustomerPaymentStatus(username, phone, "CANCELLED")

		case "escalate_to_human":
			_ = ramstore.Global.AddLead(phone, "Customer requested store owner assistance", "Owner Escalation")
			notifPhone := FormatPhone(ramstore.Global.GetVendor().NotificationPhone)
			if notifPhone != "" && client != nil && client.IsLoggedIn() {
				cust := ramstore.Global.GetCustomer(phone)
				cName := "Customer"
				if cust != nil && cust.FullName != "" {
					cName = cust.FullName
				}
				alert := fmt.Sprintf("🚨 *OWNER ASSISTANCE REQUESTED*\n\n• Customer: %s (+%s)\n• Store: %s\n\nCustomer WhatsApp par store owner se baat karna chahte hain.", cName, phone, ramstore.Global.GetVendor().BusinessName)
				go func() { _ = SendMessage(client, notifPhone, alert) }()
			}
		}
	}
}

func sendCatalogImagesFromRAM(client *whatsmeow.Client, phone, category string) {
	if client == nil || !client.IsLoggedIn() {
		return
	}
	products := ramstore.Global.ListProducts(true)
	catLower := strings.ToLower(strings.TrimSpace(category))
	sent := 0
	for _, p := range products {
		imgURLs := p.AllImageURLs()
		if len(imgURLs) == 0 {
			continue
		}
		if catLower != "" && catLower != "all" && !strings.Contains(strings.ToLower(p.Category), catLower) && !strings.Contains(strings.ToLower(p.Name), catLower) {
			continue
		}
		caption := fmt.Sprintf("🛍️ *%s*\n💰 Price: *₹%.2f*", p.Name, p.EffectivePrice())
		if p.UnitType != "" {
			caption += "/" + p.UnitType
		}
		caption += "\n📂 Category: " + p.Category
		if len(p.Variants) > 0 {
			var vStr []string
			for _, vr := range p.Variants {
				if vr.IsVariantAvailable(1) {
					vStr = append(vStr, fmt.Sprintf("%s (₹%.0f)", vr.DisplayLabel(), vr.EffectivePrice(p.EffectivePrice())))
				}
			}
			if len(vStr) > 0 {
				caption += "\n🏷️ Variants: " + strings.Join(vStr, ", ")
			}
		}
		if p.Description != "" {
			caption += "\n📝 " + p.Description
		}

		// Send up to 2 images per matched product (if specific product searched) or primary image for broad category
		maxImgs := 1
		if catLower != "" && catLower != "all" && strings.Contains(strings.ToLower(p.Name), catLower) {
			maxImgs = 3
		}
		for idx, u := range imgURLs {
			if idx >= maxImgs || sent >= 6 {
				break
			}
			c := ""
			if idx == 0 {
				c = caption
			}
			_ = SendImageFromURL(client, phone, u, c)
			sent++
			time.Sleep(350 * time.Millisecond)
		}
		if sent >= 6 {
			break
		}
	}
}

// --- Outbound Message Senders ---

func resolveTargetJID(phone string) (types.JID, string) {
	cleanPhone := FormatPhone(phone)
	lidCacheLock.RLock()
	if jid, ok := phoneToJID[cleanPhone]; ok {
		lidCacheLock.RUnlock()
		return jid, cleanPhone
	}
	lidCacheLock.RUnlock()

	server := types.DefaultUserServer
	if strings.HasPrefix(cleanPhone, "117") || strings.HasPrefix(cleanPhone, "223") || strings.HasPrefix(cleanPhone, "100") {
		server = "lid"
	}
	return types.NewJID(cleanPhone, server), cleanPhone
}

func SendMessage(client *whatsmeow.Client, phone string, message string) error {
	if client == nil || !client.IsLoggedIn() {
		return fmt.Errorf("whatsapp client is not logged in")
	}
	if err := EnsureConnected(client); err != nil {
		return err
	}
	targetJID, _ := resolveTargetJID(phone)
	msg := &waE2E.Message{Conversation: proto.String(message)}
	_, err := client.SendMessage(context.Background(), targetJID, msg)
	return err
}

func SendReplyMessage(client *whatsmeow.Client, phone string, replyText string, quotedID types.MessageID, quotedSender types.JID, quotedMsg *waE2E.Message) error {
	if client == nil || !client.IsLoggedIn() {
		return fmt.Errorf("whatsapp client is not logged in")
	}
	if err := EnsureConnected(client); err != nil {
		return err
	}
	targetJID, _ := resolveTargetJID(phone)
	var msg *waE2E.Message
	if quotedID != "" && quotedMsg != nil {
		msg = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(replyText),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:      proto.String(string(quotedID)),
					Participant:   proto.String(quotedSender.String()),
					QuotedMessage: quotedMsg,
				},
			},
		}
	} else {
		msg = &waE2E.Message{Conversation: proto.String(replyText)}
	}
	_, err := client.SendMessage(context.Background(), targetJID, msg)
	return err
}

func SendImageBytesWithThumb(client *whatsmeow.Client, phone string, imgBytes, thumbBytes []byte, mimeType, caption string) error {
	if client == nil || !client.IsLoggedIn() {
		return fmt.Errorf("whatsapp client is not logged in")
	}
	if err := EnsureConnected(client); err != nil {
		return err
	}
	uploaded, err := client.Upload(context.Background(), imgBytes, whatsmeow.MediaImage)
	if err != nil {
		return err
	}
	targetJID, _ := resolveTargetJID(phone)
	if mimeType == "" || !strings.HasPrefix(mimeType, "image/") {
		mimeType = http.DetectContentType(imgBytes)
		if !strings.HasPrefix(mimeType, "image/") {
			mimeType = "image/jpeg"
		}
	}
	if len(thumbBytes) == 0 {
		thumbBytes = UniversalThumbnail
	}
	msg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(imgBytes))),
			Caption:       proto.String(caption),
			JPEGThumbnail: thumbBytes,
		},
	}
	_, err = client.SendMessage(context.Background(), targetJID, msg)
	return err
}

func SendImageBytes(client *whatsmeow.Client, phone string, imgBytes []byte, caption string) error {
	thumb := generateWhatsAppJPEGThumbnail(imgBytes)
	return SendImageBytesWithThumb(client, phone, imgBytes, thumb, "", caption)
}

func SendImageFromURL(client *whatsmeow.Client, phone, imageURL, caption string) error {
	imgBytes, thumbBytes, mimeType, err := FetchImageWithThumbCache(imageURL)
	if err != nil {
		return err
	}
	return SendImageBytesWithThumb(client, phone, imgBytes, thumbBytes, mimeType, caption)
}
