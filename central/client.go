package central

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"wa-server/ramstore"
)

type outboundJob struct {
	Path      string
	Method    string
	Payload   interface{}
	CreatedAt time.Time
	Attempts  int
}

// CentralOrderSubmission is sent by the Vendor Bot to Central Brain Node whenever a customer confirms checkout (COD or ONLINE).
// The bot never locally marks orders PAID or generates payment QRs — Central Brain owns 100% of payment links, QR, and verification.
type CentralOrderSubmission struct {
	Event           string              `json:"event"`
	Source          string              `json:"source"`
	CustomerPhone   string              `json:"customer_phone"`
	JID             string              `json:"jid"`
	CustomerName    string              `json:"customer_name"`
	DeliveryPhone   string              `json:"delivery_phone"`
	DeliveryAddress string              `json:"delivery_address"`
	Latitude        float64             `json:"latitude,omitempty"`
	Longitude       float64             `json:"longitude,omitempty"`
	PaymentMode     string              `json:"payment_mode"` // "COD" or "ONLINE"
	Items           []ramstore.CartItem `json:"items"`
	Subtotal        float64             `json:"subtotal"`
	DeliveryFee     float64             `json:"delivery_fee"`
	TotalAmount     float64             `json:"total_amount"`
	Timestamp       string              `json:"timestamp"`
}

// CentralOrderResponse models Central Brain Node's authoritative response with order reference and online payment link/QR/instructions.
type CentralOrderResponse struct {
	Status              string  `json:"status"`
	OrderID             string  `json:"order_id"`
	OrderStatus         string  `json:"order_status"`
	PaymentMode         string  `json:"payment_mode"`
	PaymentStatus       string  `json:"payment_status"`
	TotalAmount         float64 `json:"total_amount"`
	PaymentLink         string  `json:"payment_link,omitempty"`
	PaymentQRURL        string  `json:"payment_qr_url,omitempty"`
	PaymentInstructions string  `json:"payment_instructions,omitempty"`
	Message             string  `json:"message,omitempty"`
}

// SyncClient manages event-driven bi-directional HTTP communication, bounded retry queue, platform URL auto-detection,
// and graceful shutdown event flushing between this stateless Vendor Bot Node and the Central Brain Node.
// Zero periodic polling, zero recurring heartbeat, and zero customer chat sync.
type SyncClient struct {
	mu           sync.RWMutex
	baseURL      string
	apiKey       string
	botPublicURL string
	httpClient   *http.Client
	queue        chan outboundJob
	retryMu      sync.Mutex
	retryBuffer  []outboundJob
	workerOnce   sync.Once
	inFlightWG   sync.WaitGroup
}

const maxRetryBufferSize = 256

// Client is the singleton Central Brain Node sync client.
var Client = &SyncClient{
	httpClient: &http.Client{Timeout: 20 * time.Second},
	queue:      make(chan outboundJob, 512),
}

// detectPlatformPublicURL auto-discovers this bot container's public URL on Render, Railway, Koyeb, Fly.io, or Cloud Run.
func detectPlatformPublicURL() string {
	candidates := []string{
		os.Getenv("BOT_PUBLIC_URL"),
		os.Getenv("RENDER_EXTERNAL_URL"),
		os.Getenv("RAILWAY_STATIC_URL"),
		os.Getenv("RAILWAY_PUBLIC_DOMAIN"),
		os.Getenv("KOYEB_PUBLIC_DOMAIN"),
		os.Getenv("RENDER_EXTERNAL_HOSTNAME"),
	}
	for _, raw := range candidates {
		h := strings.TrimRight(strings.TrimSpace(raw), "/")
		if h == "" {
			continue
		}
		if strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
			return h
		}
		return "https://" + h
	}
	if appName := strings.TrimSpace(os.Getenv("FLY_APP_NAME")); appName != "" {
		return "https://" + appName + ".fly.dev"
	}
	if appName := strings.TrimSpace(os.Getenv("KOYEB_APP_NAME")); appName != "" {
		return "https://" + appName + ".koyeb.app"
	}
	return ""
}

// InitFromEnv initializes Central Brain Node credentials and auto-detects the platform public URL.
func InitFromEnv() {
	centralURL := strings.TrimRight(strings.TrimSpace(os.Getenv("CENTRAL_NODE_URL")), "/")
	apiKey := strings.TrimSpace(os.Getenv("VENDOR_API_KEY"))
	botPublicURL := detectPlatformPublicURL()

	Client.Configure(centralURL, apiKey, botPublicURL, 0)
	if botPublicURL != "" {
		log.Printf("🌐 [Public URL Auto-Detected] %s", botPublicURL)
	}
}

// DetectPublicURLFromRequest learns the container's public URL from incoming reverse-proxy headers
// if platform environment variables did not inject it (e.g. custom domains or reverse proxies).
func (c *SyncClient) DetectPublicURLFromRequest(r *http.Request) {
	if r == nil {
		return
	}
	c.mu.RLock()
	existing := c.botPublicURL
	c.mu.RUnlock()
	if existing != "" {
		return
	}

	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return
	}
	lowerHost := strings.ToLower(host)
	if strings.HasPrefix(lowerHost, "localhost") || strings.HasPrefix(lowerHost, "127.0.0.1") || strings.HasPrefix(lowerHost, "0.0.0.0") {
		return
	}
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		proto = "https"
	}
	detected := strings.TrimRight(proto+"://"+host, "/")

	c.mu.Lock()
	if c.botPublicURL == "" {
		c.botPublicURL = detected
	}
	c.mu.Unlock()

	ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
		st.BotPublicURL = detected
	})
	log.Printf("🌐 [Public URL Learned From Host] %s", detected)
}

// Configure updates Central Brain Node target URL and vendor API key.
func (c *SyncClient) Configure(centralURL, apiKey, botPublicURL string, _ time.Duration) {
	c.mu.Lock()
	c.baseURL = strings.TrimRight(strings.TrimSpace(centralURL), "/")
	c.apiKey = strings.TrimSpace(apiKey)
	if strings.TrimSpace(botPublicURL) != "" {
		c.botPublicURL = strings.TrimRight(strings.TrimSpace(botPublicURL), "/")
	}
	configured := c.baseURL != "" && c.apiKey != ""
	pub := c.botPublicURL
	c.mu.Unlock()

	ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
		st.CentralURL = c.baseURL
		st.BotPublicURL = pub
		st.Configured = configured
	})

	c.startWorker()
}

// IsConfigured returns true if CENTRAL_NODE_URL and VENDOR_API_KEY are set.
func (c *SyncClient) IsConfigured() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL != "" && c.apiKey != ""
}

// GetBotPublicURL returns the publicly reachable base URL of this vendor bot container.
func (c *SyncClient) GetBotPublicURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.botPublicURL
}

// VerifyWebhookAuth checks inbound sync & remote-control calls from Central Brain Node against VENDOR_API_KEY.
func (c *SyncClient) VerifyWebhookAuth(r *http.Request) bool {
	c.mu.RLock()
	expected := c.apiKey
	c.mu.RUnlock()

	if expected == "" {
		return true
	}

	provided := strings.TrimSpace(r.Header.Get("X-Vendor-API-Key"))
	if provided == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			provided = strings.TrimSpace(auth[7:])
		}
	}
	if provided == "" {
		provided = strings.TrimSpace(r.URL.Query().Get("api_key"))
	}
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func (c *SyncClient) updateQueueMetrics() {
	c.retryMu.Lock()
	retryLen := len(c.retryBuffer)
	c.retryMu.Unlock()
	qLen := len(c.queue)
	ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
		st.QueueDepth = qLen
		st.RetryBufferDepth = retryLen
	})
}

func (c *SyncClient) startWorker() {
	c.workerOnce.Do(func() {
		go func() {
			for job := range c.queue {
				c.inFlightWG.Add(1)
				c.processJob(job)
				c.inFlightWG.Done()
				c.updateQueueMetrics()
			}
		}()
	})
}

func (c *SyncClient) processJob(job outboundJob) {
	if !c.IsConfigured() {
		return
	}
	// Exponential backoff: 1s, 2s, 4s, 8s (max 4 inline attempts)
	backoff := 1 * time.Second
	for attempt := 1; attempt <= 4; attempt++ {
		_, _, err := c.doRequest(job.Method, job.Path, job.Payload)
		if err == nil {
			ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
				st.OutboundPushed++
				st.LastSyncAt = time.Now()
				st.Connected = true
				st.LastError = ""
			})
			return
		}
		log.Printf("⚠️ [Central Queue Attempt %d/4] %s %s failed: %v", attempt, job.Method, job.Path, err)
		if attempt < 4 {
			time.Sleep(backoff)
			backoff *= 2
		} else {
			job.Attempts += attempt
			c.storeInRetryBuffer(job)
			ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
				st.LastError = err.Error()
			})
		}
	}
}

func (c *SyncClient) storeInRetryBuffer(job outboundJob) {
	c.retryMu.Lock()
	defer c.retryMu.Unlock()
	if len(c.retryBuffer) >= maxRetryBufferSize {
		// Evict oldest to stay strictly bounded in 512MB RAM
		c.retryBuffer = c.retryBuffer[1:]
	}
	c.retryBuffer = append(c.retryBuffer, job)
}

// ReplayRetryBuffer retries buffered events that failed while Central Brain Node was temporarily unreachable.
func (c *SyncClient) ReplayRetryBuffer() int {
	if !c.IsConfigured() {
		return 0
	}
	c.retryMu.Lock()
	if len(c.retryBuffer) == 0 {
		c.retryMu.Unlock()
		return 0
	}
	pending := make([]outboundJob, len(c.retryBuffer))
	copy(pending, c.retryBuffer)
	c.retryBuffer = nil
	c.retryMu.Unlock()

	delivered := 0
	var stillFailed []outboundJob
	for _, job := range pending {
		_, _, err := c.doRequest(job.Method, job.Path, job.Payload)
		if err == nil {
			delivered++
			ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
				st.OutboundPushed++
				st.LastSyncAt = time.Now()
				st.Connected = true
				st.LastError = ""
			})
		} else {
			stillFailed = append(stillFailed, job)
		}
	}

	if len(stillFailed) > 0 {
		c.retryMu.Lock()
		c.retryBuffer = append(stillFailed, c.retryBuffer...)
		if len(c.retryBuffer) > maxRetryBufferSize {
			c.retryBuffer = c.retryBuffer[len(c.retryBuffer)-maxRetryBufferSize:]
		}
		c.retryMu.Unlock()
	}
	c.updateQueueMetrics()
	if delivered > 0 {
		log.Printf("✅ [Central Retry Buffer] Replayed %d buffered events to Central Brain Node (%d remaining).", delivered, len(stillFailed))
	}
	return delivered
}

// FlushPendingEvents drains the outbound event queue and retry buffer synchronously before container shutdown.
func (c *SyncClient) FlushPendingEvents(timeout time.Duration) {
	if !c.IsConfigured() {
		return
	}
	log.Printf("📤 [Graceful Shutdown] Flushing pending Central Brain Node events (queue=%d)...", len(c.queue))
	deadline := time.Now().Add(timeout)

	// Drain any queued jobs immediately
	for time.Now().Before(deadline) {
		select {
		case job := <-c.queue:
			_, _, err := c.doRequest(job.Method, job.Path, job.Payload)
			if err == nil {
				ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
					st.OutboundPushed++
					st.LastSyncAt = time.Now()
				})
			} else {
				c.storeInRetryBuffer(job)
			}
		default:
			goto doneDrain
		}
	}
doneDrain:
	// Wait briefly for any in-flight worker job
	doneCh := make(chan struct{})
	go func() {
		c.inFlightWG.Wait()
		close(doneCh)
	}()
	rem := time.Until(deadline)
	if rem > 0 {
		select {
		case <-doneCh:
		case <-time.After(rem):
		}
	}

	// One final replay pass on any retry-buffered events if time permits
	if time.Now().Before(deadline) {
		c.ReplayRetryBuffer()
	}
	c.updateQueueMetrics()
}

func (c *SyncClient) doRequest(method, path string, payload interface{}) ([]byte, int, error) {
	c.mu.RLock()
	baseURL := c.baseURL
	apiKey := c.apiKey
	c.mu.RUnlock()

	if baseURL == "" || apiKey == "" {
		return nil, 0, fmt.Errorf("CENTRAL_NODE_URL or VENDOR_API_KEY not configured")
	}

	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(b)
	}

	url := baseURL + path
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Vendor-API-Key", apiKey)
	req.Header.Set("User-Agent", "VendorWhatsAppBotNode/2.3")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return respBytes, resp.StatusCode, fmt.Errorf("central node HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	return respBytes, resp.StatusCode, nil
}

// BootstrapResponse models the one-time JSON returned by Central Brain Node on startup or manual dashboard sync.
// Customer chats are strictly RAM-only and are never requested from Central Brain Node.
type BootstrapResponse struct {
	Vendor          *ramstore.VendorProfile        `json:"vendor"`
	Business        *ramstore.VendorProfile        `json:"business"`
	AI              *ramstore.AIConfig             `json:"ai"`
	Payment         *ramstore.PaymentConfig        `json:"payment"`
	Products        []ramstore.Product             `json:"products"`
	Customers       []ramstore.Customer            `json:"customers"`
	ActiveCarts     map[string][]ramstore.CartItem `json:"active_carts"`
	Orders          []ramstore.Order               `json:"orders"`
	WhatsAppSession *BootstrapWhatsAppSession      `json:"whatsapp_session"`
}

// BootstrapWhatsAppSession carries the persistent whatsmeow session blob stored in Central Node's DB.
type BootstrapWhatsAppSession struct {
	HasSession        bool   `json:"has_session"`
	Status            string `json:"status"`
	DeviceJID         string `json:"device_jid"`
	Phone             string `json:"phone"`
	PushName          string `json:"push_name"`
	SessionBlobBase64 string `json:"session_blob_base64"`
}

// Bootstrap fetches vendor data + WhatsApp session state once on startup (or manual Sync button) and hydrates RAM.
func (c *SyncClient) Bootstrap() (*BootstrapWhatsAppSession, error) {
	if !c.IsConfigured() {
		err := fmt.Errorf("CENTRAL_NODE_URL and VENDOR_API_KEY are not set; running in standby mode")
		ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
			st.Configured = false
			st.Connected = false
			st.LastError = err.Error()
		})
		return nil, err
	}

	reqBody := map[string]interface{}{
		"bot_public_url": c.GetBotPublicURL(),
		"node_version":   "2.3-zero-polling-zero-chat-sync",
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
	}

	var respBytes []byte
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		respBytes, _, err = c.doRequest("POST", "/api/vendor-bot/bootstrap", reqBody)
		if err != nil {
			respBytes, _, err = c.doRequest("GET", "/api/vendor-bot/bootstrap", nil)
		}
		if err == nil {
			break
		}
		log.Printf("⏳ [Central Bootstrap Attempt %d/3] %v", attempt, err)
		time.Sleep(time.Duration(attempt*2) * time.Second)
	}

	if err != nil {
		ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
			st.Connected = false
			st.LastError = err.Error()
		})
		return nil, err
	}

	decoded, warnings, err := ramstore.DecodeBootstrapPayload(respBytes)
	if err != nil {
		ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
			st.Connected = false
			st.LastError = "invalid bootstrap JSON: " + err.Error()
		})
		return nil, fmt.Errorf("invalid bootstrap JSON: %w", err)
	}
	for _, w := range warnings {
		log.Printf("⚠️ [Central Bootstrap Section Warning] %s", w)
	}

	ramstore.Global.HydrateFromBootstrap(decoded)

	var waSession *BootstrapWhatsAppSession
	if decoded.WhatsAppSession != nil {
		waSession = &BootstrapWhatsAppSession{
			HasSession:        decoded.WhatsAppSession.HasSession,
			Status:            decoded.WhatsAppSession.Status,
			DeviceJID:         decoded.WhatsAppSession.DeviceJID,
			Phone:             decoded.WhatsAppSession.Phone,
			PushName:          decoded.WhatsAppSession.PushName,
			SessionBlobBase64: decoded.WhatsAppSession.SessionBlobBase64,
		}
	}

	// Once Central Node is reachable, replay any queued retry events
	go c.ReplayRetryBuffer()

	stats := ramstore.Global.Stats()
	log.Printf("✅ [Central Brain Node Synced] Products: %d | Customers: %d | Active Carts: %d | Orders: %d | WA Session: %v | Warnings: %d",
		stats["products"], stats["customers"], stats["active_carts"], stats["orders"],
		waSession != nil && waSession.HasSession, len(warnings))

	return waSession, nil
}

// SaveWhatsAppSession synchronously uploads a validated non-empty gzip+base64 whatsmeow session snapshot to Central Brain Node.
// Safety guard: refuses to overwrite Central Node's valid session with an empty or disconnected blob.
func (c *SyncClient) SaveWhatsAppSession(deviceJID, phone, pushName, status, blobBase64 string) error {
	if !c.IsConfigured() {
		return nil
	}
	if strings.TrimSpace(blobBase64) == "" || strings.TrimSpace(deviceJID) == "" {
		return fmt.Errorf("refusing to overwrite central session with empty blob or missing device_jid")
	}
	payload := map[string]interface{}{
		"device_jid":          deviceJID,
		"phone":               phone,
		"push_name":           pushName,
		"status":              status,
		"has_session":         true,
		"session_blob_base64": blobBase64,
		"bot_public_url":      c.GetBotPublicURL(),
		"updated_at":          time.Now().UTC().Format(time.RFC3339),
	}
	_, _, err := c.doRequest("POST", "/api/vendor-bot/whatsapp/session", payload)
	if err != nil {
		log.Printf("⚠️ [Central Session Sync] Failed to persist WhatsApp session to Central Node: %v", err)
		return err
	}
	ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
		ws.HasSession = true
		ws.NeedsLogin = false
		ws.DeviceJID = deviceJID
		ws.Phone = phone
		ws.PushName = pushName
		ws.LastSyncedAt = time.Now()
	})
	ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
		st.OutboundPushed++
		st.LastSyncAt = time.Now()
	})
	log.Printf("💾 [Central Session Sync] WhatsMeow session snapshot persisted to Central Brain Node (%d KB).", len(blobBase64)/1024)
	return nil
}

// DeleteWhatsAppSession notifies Central Brain Node that the vendor explicitly unlinked WhatsApp.
func (c *SyncClient) DeleteWhatsAppSession() error {
	if !c.IsConfigured() {
		return nil
	}
	_, _, err := c.doRequest("DELETE", "/api/vendor-bot/whatsapp/session", nil)
	if err != nil {
		_, _, err = c.doRequest("POST", "/api/vendor-bot/whatsapp/session", map[string]interface{}{
			"has_session":         false,
			"status":              "Disconnected",
			"session_blob_base64": "",
		})
	}
	return err
}

// SubmitOrderToCentral sends a customer's WhatsApp order details (cart items, name, phone, address, payment mode)
// to Central Brain Node. Central Brain owns 100% of order creation, online payment QR/link generation, payment verification,
// and triggers final confirmation back via POST /api/sync/order.
func (c *SyncClient) SubmitOrderToCentral(req CentralOrderSubmission) (*CentralOrderResponse, error) {
	if req.Event == "" {
		req.Event = "order.requested"
	}
	if req.Source == "" {
		req.Source = "whatsapp"
	}
	if req.Timestamp == "" {
		req.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	req.CustomerPhone = ramstore.NormalizePhone(req.CustomerPhone)
	req.DeliveryPhone = ramstore.NormalizePhone(req.DeliveryPhone)
	if req.DeliveryPhone == "" {
		req.DeliveryPhone = req.CustomerPhone
	}
	req.PaymentMode = strings.ToUpper(strings.TrimSpace(req.PaymentMode))
	if req.PaymentMode != "COD" && req.PaymentMode != "ONLINE" {
		req.PaymentMode = "ONLINE"
	}

	if !c.IsConfigured() {
		return nil, fmt.Errorf("central brain node is not configured")
	}

	respBytes, _, err := c.doRequest("POST", "/api/vendor-bot/events/order", req)
	if err != nil {
		// Queue for retry so Central Brain never loses the customer's checkout submission
		c.enqueue("POST", "/api/vendor-bot/events/order", req)
		return nil, err
	}

	var envelope struct {
		CentralOrderResponse
		Data  *CentralOrderResponse `json:"data"`
		Order *CentralOrderResponse `json:"order"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		return &CentralOrderResponse{
			Status:        "submitted",
			PaymentMode:   req.PaymentMode,
			PaymentStatus: "PENDING",
			OrderStatus:   "PENDING_CONFIRMATION",
			TotalAmount:   req.TotalAmount,
			Message:       "Order request submitted to Central Brain Node.",
		}, nil
	}

	result := envelope.CentralOrderResponse
	if envelope.Data != nil && (envelope.Data.OrderID != "" || envelope.Data.PaymentLink != "" || envelope.Data.PaymentQRURL != "" || envelope.Data.Message != "") {
		result = *envelope.Data
	} else if envelope.Order != nil && (envelope.Order.OrderID != "" || envelope.Order.PaymentLink != "" || envelope.Order.PaymentQRURL != "" || envelope.Order.Message != "") {
		result = *envelope.Order
	}
	if result.PaymentMode == "" {
		result.PaymentMode = req.PaymentMode
	}
	if result.TotalAmount <= 0 {
		result.TotalAmount = req.TotalAmount
	}

	ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
		st.OutboundPushed++
		st.LastSyncAt = time.Now()
	})
	return &result, nil
}

func (c *SyncClient) enqueue(method, path string, payload interface{}) {
	if !c.IsConfigured() {
		return
	}
	c.startWorker()
	job := outboundJob{Method: method, Path: path, Payload: payload, CreatedAt: time.Now()}
	select {
	case c.queue <- job:
	default:
		// Queue full -> spill into bounded retry buffer
		c.storeInRetryBuffer(job)
	}
	c.updateQueueMetrics()
}

// --- Essential Business-Event-Only Async Pushers (Zero Chat Sync, Zero Polling) ---

func (c *SyncClient) PushCustomerAsync(cust *ramstore.Customer) {
	if cust == nil {
		return
	}
	c.enqueue("POST", "/api/vendor-bot/events/customer", map[string]interface{}{
		"event":     "customer.upserted",
		"source":    "whatsapp",
		"customer":  cust,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (c *SyncClient) PushCartAsync(phone, jid string, items []ramstore.CartItem, total float64) {
	c.enqueue("POST", "/api/vendor-bot/events/cart", map[string]interface{}{
		"event":          "cart.updated",
		"source":         "whatsapp",
		"customer_phone": ramstore.NormalizePhone(phone),
		"jid":            jid,
		"items":          items,
		"total_amount":   total,
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
	})
}

func (c *SyncClient) PushOrderAsync(ord *ramstore.Order) {
	if ord == nil {
		return
	}
	c.enqueue("POST", "/api/vendor-bot/events/order", map[string]interface{}{
		"event":     "order.upserted",
		"source":    ord.Source,
		"order":     ord,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (c *SyncClient) PushMagicLinkAsync(token, jid, phone string, expiresAt time.Time) {
	c.enqueue("POST", "/api/vendor-bot/events/magic-link", map[string]interface{}{
		"event":          "magic_link.created",
		"token":          token,
		"jid":            jid,
		"customer_phone": ramstore.NormalizePhone(phone),
		"expires_at":     expiresAt.UTC().Format(time.RFC3339),
	})
}
