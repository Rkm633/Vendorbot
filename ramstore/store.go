package ramstore

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VendorProfile holds the vendor's store identity, schedule, admin gating, storefront URLs, and delivery settings in RAM.
type VendorProfile struct {
	VendorID                string  `json:"vendor_id"`
	Username                string  `json:"username"`
	BusinessName            string  `json:"business_name"`
	BusinessType            string  `json:"business_type"`
	Language                string  `json:"language"`
	StorePolicies           string  `json:"store_policies"`
	StoreSlug               string  `json:"store_slug"`
	StoreURL                string  `json:"store_url"`
	MallURL                 string  `json:"mall_url"`
	NotificationPhone       string  `json:"notification_phone"`
	Latitude                float64 `json:"latitude"`
	Longitude               float64 `json:"longitude"`
	MaxDeliveryRadiusKM     float64 `json:"max_delivery_radius_km"`
	FreeDeliveryRadiusKM    float64 `json:"free_delivery_radius_km"`
	DeliveryChargeAmount    float64 `json:"delivery_charge_amount"`
	RequireCustomerLocation bool    `json:"require_customer_location"`
	AllowCOD                bool    `json:"allow_cod"`

	// Store operating status, schedule & checkout rules
	IsOpen                *bool   `json:"is_open,omitempty"`
	HolidayMode           bool    `json:"holiday_mode"`
	HolidayMessage        string  `json:"holiday_message,omitempty"`
	OperatingHours        string  `json:"operating_hours,omitempty"`
	NextOpenTime          string  `json:"next_open_time,omitempty"`
	MinOrderAmount        float64 `json:"min_order_amount"`
	EstimatedDeliveryTime string  `json:"estimated_delivery_time,omitempty"`

	// Central Admin feature gating & subscription controls
	SubscriptionActive      *bool  `json:"subscription_active,omitempty"`
	SubscriptionValidUntil  string `json:"subscription_valid_until,omitempty"`
	AdminAIAutoReplyAllowed *bool  `json:"admin_ai_auto_reply_allowed,omitempty"`
	AdminPauseReason        string `json:"admin_pause_reason,omitempty"`
}

// BoolPtr returns a pointer to a boolean value.
func BoolPtr(v bool) *bool { return &v }

// IntPtr returns a pointer to an int value.
func IntPtr(v int) *int { return &v }

// IsStoreOpenNow evaluates store open/closed and holiday mode flags.
func (v VendorProfile) IsStoreOpenNow() (bool, string) {
	if v.HolidayMode {
		msg := strings.TrimSpace(v.HolidayMessage)
		if msg == "" {
			msg = "Store is currently closed on holiday mode."
		}
		return false, msg
	}
	if v.IsOpen != nil && !*v.IsOpen {
		return false, "Store is currently closed."
	}
	return true, ""
}

// IsSubscriptionValid evaluates subscription active flag and expiry timestamp (RFC3339 or YYYY-MM-DD).
func (v VendorProfile) IsSubscriptionValid() (bool, string) {
	if v.SubscriptionActive != nil && !*v.SubscriptionActive {
		return false, "Vendor subscription is inactive."
	}
	raw := strings.TrimSpace(v.SubscriptionValidUntil)
	if raw == "" {
		return true, ""
	}
	layouts := []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			if layout == "2006-01-02" {
				t = t.Add(24*time.Hour - time.Second)
			}
			if time.Now().After(t) {
				return false, fmt.Sprintf("Subscription expired on %s.", raw)
			}
			return true, ""
		}
	}
	return true, ""
}

// AIConfig holds the vendor's LLM auto-responder settings & multi-key fallback pool in RAM.
type AIConfig struct {
	Provider      string   `json:"provider"`
	APIKey        string   `json:"api_key"`
	APIKeys       []string `json:"api_keys,omitempty"`
	BackupAPIKeys []string `json:"backup_api_keys,omitempty"`
	Model         string   `json:"model"`
	Temperature   float64  `json:"temperature"`
	SystemPrompt  string   `json:"system_prompt"`
	AutoReply     bool     `json:"auto_reply"`
	ImportCatalog bool     `json:"import_catalog"`
}

// AllAPIKeys returns a deduplicated ordered slice of primary + backup API keys.
func (a AIConfig) AllAPIKeys() []string {
	seen := make(map[string]bool)
	var out []string
	addTokens := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		re := regexp.MustCompile(`[\n,\s]+`)
		for _, tok := range re.Split(raw, -1) {
			tok = strings.TrimSpace(tok)
			if tok != "" && !seen[tok] {
				seen[tok] = true
				out = append(out, tok)
			}
		}
	}
	addTokens(a.APIKey)
	for _, k := range a.APIKeys {
		addTokens(k)
	}
	for _, k := range a.BackupAPIKeys {
		addTokens(k)
	}
	return out
}

// PaymentConfig holds UPI / Paytm / Gateway payment details synced from Central Brain Node in RAM.
type PaymentConfig struct {
	Enabled             bool   `json:"enabled"`
	Gateway             string `json:"gateway,omitempty"`
	MerchantPhone       string `json:"merchant_phone"`
	MerchantVPA         string `json:"merchant_vpa"`
	PaytmMID            string `json:"paytm_mid"`
	PayeeName           string `json:"payee_name"`
	CloudinaryCloudName string `json:"cloudinary_cloud_name"`
}

// ProductVariant represents a weight/size/pack variant with optional custom pricing and stock quantity.
type ProductVariant struct {
	Name      string  `json:"name"`
	Weight    string  `json:"weight,omitempty"`
	Size      string  `json:"size,omitempty"`
	Price     float64 `json:"price,omitempty"`
	SalePrice float64 `json:"sale_price,omitempty"`
	MRP       float64 `json:"mrp,omitempty"`
	StockQty  *int    `json:"stock_qty,omitempty"`
	InStock   *bool   `json:"in_stock,omitempty"`
	SKU       string  `json:"sku,omitempty"`
}

// DisplayLabel returns the human-readable variant label.
func (pv ProductVariant) DisplayLabel() string {
	if s := strings.TrimSpace(pv.Name); s != "" {
		return s
	}
	if s := strings.TrimSpace(pv.Weight); s != "" {
		return s
	}
	return strings.TrimSpace(pv.Size)
}

// EffectivePrice returns the variant price or falls back to the parent product price.
func (pv ProductVariant) EffectivePrice(parentPrice float64) float64 {
	if pv.SalePrice > 0 {
		return pv.SalePrice
	}
	if pv.Price > 0 {
		return pv.Price
	}
	if pv.MRP > 0 {
		return pv.MRP
	}
	return parentPrice
}

// IsVariantAvailable checks variant-level stock status & quantity.
func (pv ProductVariant) IsVariantAvailable(requestedQty int) bool {
	if pv.InStock != nil && !*pv.InStock {
		return false
	}
	if pv.StockQty != nil {
		if *pv.StockQty <= 0 {
			return false
		}
		if requestedQty > 0 && requestedQty > *pv.StockQty {
			return false
		}
	}
	return true
}

// VariantList unmarshals seamlessly from either ["500g","1kg"] or [{"name":"500g","price":120,"stock_qty":10}].
type VariantList []ProductVariant

func (vl *VariantList) UnmarshalJSON(data []byte) error {
	*vl = decodeVariantsRaw(data)
	return nil
}

// Product represents a vendor catalog item cached in RAM for 0ms AI RAG, variant selection, and stock validation.
type Product struct {
	ID                  string      `json:"id"`
	Name                string      `json:"name"`
	Price               float64     `json:"price"`
	SalePrice           float64     `json:"sale_price"`
	MRP                 float64     `json:"mrp"`
	Description         string      `json:"description"`
	Category            string      `json:"category"`
	UnitType            string      `json:"unit_type"`
	Variants            VariantList `json:"variants,omitempty"`
	InStock             bool        `json:"in_stock"`
	StockQty            *int        `json:"stock_qty,omitempty"`
	ImageURL            string      `json:"image_url"`
	Images              []string    `json:"images,omitempty"`
	ThumbURL            string      `json:"thumb_url"`
	WhatsAppCatalogLink string      `json:"whatsapp_catalog_link"`
}

// EffectivePrice returns the active selling price of a product.
func (p Product) EffectivePrice() float64 {
	if p.SalePrice > 0 {
		return p.SalePrice
	}
	if p.Price > 0 {
		return p.Price
	}
	return p.MRP
}

// AllImageURLs returns a deduplicated list of product images for catalog browsing.
func (p Product) AllImageURLs() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	add(p.ImageURL)
	for _, u := range p.Images {
		add(u)
	}
	if len(out) == 0 {
		add(p.ThumbURL)
	}
	return out
}

// IsProductInStock checks both boolean InStock and StockQty (plus variant stock if variants exist).
func (p Product) IsProductInStock() bool {
	if !p.InStock {
		return false
	}
	if p.StockQty != nil && *p.StockQty <= 0 {
		return false
	}
	if len(p.Variants) > 0 {
		anyAvail := false
		for _, v := range p.Variants {
			if v.IsVariantAvailable(1) {
				anyAvail = true
				break
			}
		}
		return anyAvail
	}
	return true
}

// ResolveVariantAndStock validates stock and resolves exact unit price & variant label.
func (p Product) ResolveVariantAndStock(requestedVariant string, qty int) (ok bool, unitPrice float64, variantLabel string, reason string) {
	if qty <= 0 {
		qty = 1
	}
	if !p.InStock {
		return false, 0, "", fmt.Sprintf("%s is currently out of stock", p.Name)
	}
	if p.StockQty != nil && (*p.StockQty <= 0 || qty > *p.StockQty) {
		if *p.StockQty <= 0 {
			return false, 0, "", fmt.Sprintf("%s is currently out of stock", p.Name)
		}
		return false, 0, "", fmt.Sprintf("Only %d %s of %s available in stock", *p.StockQty, p.UnitType, p.Name)
	}

	reqLower := strings.ToLower(strings.TrimSpace(requestedVariant))
	if len(p.Variants) > 0 {
		var matched *ProductVariant
		if reqLower != "" {
			for i := range p.Variants {
				lbl := strings.ToLower(p.Variants[i].DisplayLabel())
				w := strings.ToLower(strings.TrimSpace(p.Variants[i].Weight))
				sz := strings.ToLower(strings.TrimSpace(p.Variants[i].Size))
				if lbl == reqLower || w == reqLower || sz == reqLower || (lbl != "" && strings.Contains(lbl, reqLower)) {
					matched = &p.Variants[i]
					break
				}
			}
		}
		if matched == nil && reqLower == "" {
			// Default to first available variant
			for i := range p.Variants {
				if p.Variants[i].IsVariantAvailable(qty) {
					matched = &p.Variants[i]
					break
				}
			}
		}
		if matched != nil {
			if !matched.IsVariantAvailable(qty) {
				return false, 0, matched.DisplayLabel(), fmt.Sprintf("%s (%s) is out of stock or insufficient stock for qty %d", p.Name, matched.DisplayLabel(), qty)
			}
			return true, matched.EffectivePrice(p.EffectivePrice()), matched.DisplayLabel(), ""
		}
	}

	return true, p.EffectivePrice(), strings.TrimSpace(requestedVariant), ""
}

// Customer holds customer identity, saved address, GPS coordinates, and checkout state in RAM.
type Customer struct {
	WANumber         string    `json:"wa_number"`
	JID              string    `json:"jid"`
	FullName         string    `json:"full_name"`
	DeliveryAddress  string    `json:"delivery_address"`
	ContactPhone     string    `json:"contact_phone"`
	Latitude         float64   `json:"latitude"`
	Longitude        float64   `json:"longitude"`
	LastOrderDetails string    `json:"last_order_details"`
	CurrentState     string    `json:"current_state"`  // "chatting", "PAYMENT_PENDING", "ORDER_CONFIRMED"
	PaymentStatus    string    `json:"payment_status"` // "UNPAID", "PENDING", "PAID", "COD_PENDING", "CANCELLED"
	CurrentOrderID   string    `json:"current_order_id"`
	Source           string    `json:"source"` // "whatsapp" or "website"
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// CartItem represents a single product+variant line in a customer's active shopping cart.
type CartItem struct {
	ProductID       string    `json:"product_id"`
	ProductName     string    `json:"product_name"`
	UnitPrice       float64   `json:"unit_price"`
	UnitType        string    `json:"unit_type,omitempty"`
	Qty             int       `json:"qty"`
	SelectedVariant string    `json:"selected_variant,omitempty"`
	LineTotal       float64   `json:"line_total"`
	Source          string    `json:"source"` // "whatsapp" or "website"
	UpdatedAt       time.Time `json:"updated_at"`
}

// Order represents a confirmed or in-progress order synced between WhatsApp, RAM, Website, and Delivery Portal.
type Order struct {
	OrderID              string     `json:"order_id"`
	CustomerPhone        string     `json:"customer_phone"`
	JID                  string     `json:"jid"`
	CustomerName         string     `json:"customer_name"`
	DeliveryPhone        string     `json:"delivery_phone"`
	DeliveryAddress      string     `json:"delivery_address"`
	Items                []CartItem `json:"items"`
	Subtotal             float64    `json:"subtotal"`
	DeliveryFee          float64    `json:"delivery_fee"`
	TotalAmount          float64    `json:"total_amount"`
	PaymentMode          string     `json:"payment_mode"`   // "COD", "ONLINE"
	PaymentStatus        string     `json:"payment_status"` // "PENDING", "PAID", "COD_PENDING", "PAYMENT_FAILED"
	OrderStatus          string     `json:"order_status"`   // "PENDING_CENTRAL", "PENDING_PAYMENT", "CONFIRMED", "PROCESSING", "PACKED", "DELIVERY_PARTNER_ASSIGNED", "DISPATCHED", "OUT_FOR_DELIVERY", "DELIVERED", "CANCELLED"
	Source               string     `json:"source"`         // "whatsapp", "website", "delivery_portal"
	TxnID                string     `json:"txn_id,omitempty"`
	PaymentLink          string     `json:"payment_link,omitempty"`
	PaymentQRURL         string     `json:"payment_qr_url,omitempty"`
	DeliveryPartnerName  string     `json:"delivery_partner_name,omitempty"`
	DeliveryPartnerPhone string     `json:"delivery_partner_phone,omitempty"`
	DeliveryOTP          string     `json:"delivery_otp,omitempty"`
	EstimatedDelivery    string     `json:"estimated_delivery,omitempty"`
	StatusUpdatedBy      string     `json:"status_updated_by,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// ChatRecord holds a single message in the per-customer sliding window (max 10 messages).
type ChatRecord struct {
	ID            int64     `json:"id"`
	CustomerPhone string    `json:"customer_phone"`
	Role          string    `json:"role"`      // "customer" or "assistant"
	Direction     string    `json:"direction"` // "incoming", "outgoing_ai", "outgoing_manual", "outgoing_bulk"
	Text          string    `json:"text"`
	Time          string    `json:"time"`
	CreatedAt     time.Time `json:"created_at"`
}

// Lead represents a captured sales inquiry or owner escalation stored in RAM and pushed to Central Node.
type Lead struct {
	ID            string    `json:"id"`
	CustomerPhone string    `json:"customer_phone"`
	InquiryText   string    `json:"inquiry_text"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// BulkLogItem tracks recent broadcast deliveries in RAM.
type BulkLogItem struct {
	CampaignID string `json:"campaign_id"`
	Phone      string `json:"phone"`
	Status     string `json:"status"`
	Note       string `json:"note"`
	Time       string `json:"time"`
}

// WhatsAppSessionState tracks whether Central Node had an existing WhatsApp session or login is required.
type WhatsAppSessionState struct {
	HasSession        bool      `json:"has_session"`
	NeedsLogin        bool      `json:"needs_login"`
	Status            string    `json:"status"` // "Disconnected", "Needs_Login", "QR_Ready", "Connecting", "Connected"
	DeviceJID         string    `json:"device_jid"`
	Phone             string    `json:"phone"`
	PushName          string    `json:"push_name"`
	QRCode            string    `json:"qr_code,omitempty"`
	SessionBlobBase64 string    `json:"-"`
	LastSyncedAt      time.Time `json:"last_synced_at,omitempty"`
}

// SyncStatus tracks connectivity, auto-detected public URL, queue health, and sync telemetry with Central Brain Node.
type SyncStatus struct {
	CentralURL       string    `json:"central_url"`
	BotPublicURL     string    `json:"bot_public_url"`
	Configured       bool      `json:"configured"`
	Connected        bool      `json:"connected"`
	Bootstrapped     bool      `json:"bootstrapped"`
	LastBootstrapAt  time.Time `json:"last_bootstrap_at,omitempty"`
	LastSyncAt       time.Time `json:"last_sync_at,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	OutboundPushed   int64     `json:"outbound_pushed"`
	InboundReceived  int64     `json:"inbound_received"`
	QueueDepth       int       `json:"queue_depth"`
	RetryBufferDepth int       `json:"retry_buffer_depth"`
}

// BootstrapData is the one-time startup/manual payload hydrated from Central Brain Node.
// Customer chat messages are strictly RAM-only and are never fetched from or pushed to Central Node.
type BootstrapData struct {
	Vendor          *VendorProfile        `json:"vendor"`
	AI              *AIConfig             `json:"ai"`
	Payment         *PaymentConfig        `json:"payment"`
	Products        []Product             `json:"products"`
	Customers       []Customer            `json:"customers"`
	ActiveCarts     map[string][]CartItem `json:"active_carts"` // key: normalized customer phone
	Orders          []Order               `json:"orders"`
	WhatsAppSession *WhatsAppSessionState `json:"whatsapp_session"`
}

// Store is the single-vendor, zero-database RAM store powering 0ms WhatsApp AI replies.
type Store struct {
	mu           sync.RWMutex
	vendor       VendorProfile
	ai           AIConfig
	payment      PaymentConfig
	products     map[string]Product
	productOrder []string
	customers    map[string]*Customer
	activeCarts  map[string][]CartItem
	orders       map[string]*Order
	orderIDs     []string
	chats        map[string][]ChatRecord
	leads        []Lead
	bulkLogs     []BulkLogItem
	waSession    WhatsAppSessionState
	syncStatus   SyncStatus
	chatSeq      int64
}

// Global is the singleton RAM store for this vendor bot node.
var Global = NewStore()

// NewStore initializes an empty thread-safe RAM store awaiting Central Brain Node hydration.
func NewStore() *Store {
	return &Store{
		vendor: VendorProfile{
			AllowCOD:                true,
			IsOpen:                  BoolPtr(true),
			SubscriptionActive:      BoolPtr(true),
			AdminAIAutoReplyAllowed: BoolPtr(true),
		},
		ai: AIConfig{
			Provider:      "",
			Model:         "",
			Temperature:   0.7,
			AutoReply:     true,
			ImportCatalog: true,
		},
		payment: PaymentConfig{
			Enabled: false,
		},
		products:    make(map[string]Product),
		customers:   make(map[string]*Customer),
		activeCarts: make(map[string][]CartItem),
		orders:      make(map[string]*Order),
		chats:       make(map[string][]ChatRecord),
		waSession: WhatsAppSessionState{
			Status:     "Needs_Login",
			NeedsLogin: true,
		},
	}
}

var nonDigitsRe = regexp.MustCompile(`\D`)

// NormalizePhone cleans phone strings and applies Indian 91 prefix for 10-digit numbers.
func NormalizePhone(phone string) string {
	clean := nonDigitsRe.ReplaceAllString(strings.TrimSpace(phone), "")
	if clean == "" {
		return ""
	}
	if len(clean) == 10 && (clean[0] == '6' || clean[0] == '7' || clean[0] == '8' || clean[0] == '9') {
		return "91" + clean
	}
	if len(clean) == 11 && clean[0] == '0' {
		return "91" + clean[1:]
	}
	return clean
}

// CanAutoReply checks Central Admin feature gating, subscription validity, and vendor auto-reply toggle.
func (s *Store) CanAutoReply() (bool, string) {
	s.mu.RLock()
	v := s.vendor
	a := s.ai
	s.mu.RUnlock()

	if v.AdminAIAutoReplyAllowed != nil && !*v.AdminAIAutoReplyAllowed {
		reason := strings.TrimSpace(v.AdminPauseReason)
		if reason == "" {
			reason = "AI auto-reply disabled by Central Admin."
		}
		return false, reason
	}
	if ok, reason := v.IsSubscriptionValid(); !ok {
		return false, reason
	}
	if !a.AutoReply {
		return false, "Auto-reply paused by vendor settings."
	}
	return true, ""
}

// HydrateFromBootstrap atomically replaces/merges RAM state from Central Brain Node's bootstrap/reconciliation payload.
func (s *Store) HydrateFromBootstrap(data *BootstrapData) {
	if data == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if data.Vendor != nil {
		v := *data.Vendor
		if v.Username == "" {
			v.Username = s.vendor.Username
		}
		if v.BusinessName == "" {
			v.BusinessName = s.vendor.BusinessName
		}
		if v.IsOpen == nil {
			if s.vendor.IsOpen != nil {
				v.IsOpen = s.vendor.IsOpen
			} else {
				v.IsOpen = BoolPtr(true)
			}
		}
		if v.SubscriptionActive == nil {
			if s.vendor.SubscriptionActive != nil {
				v.SubscriptionActive = s.vendor.SubscriptionActive
			} else {
				v.SubscriptionActive = BoolPtr(true)
			}
		}
		if v.AdminAIAutoReplyAllowed == nil {
			if s.vendor.AdminAIAutoReplyAllowed != nil {
				v.AdminAIAutoReplyAllowed = s.vendor.AdminAIAutoReplyAllowed
			} else {
				v.AdminAIAutoReplyAllowed = BoolPtr(true)
			}
		}
		s.vendor = v
	}

	if data.AI != nil {
		a := *data.AI
		if a.Temperature <= 0 {
			a.Temperature = 0.7
		}
		s.ai = a
	}

	if data.Payment != nil {
		p := *data.Payment
		if !p.Enabled && (p.PaytmMID != "" || p.MerchantVPA != "" || p.Gateway != "") {
			p.Enabled = true
		}
		if p.PayeeName == "" {
			p.PayeeName = s.vendor.BusinessName
		}
		s.payment = p
	}

	if data.Products != nil {
		s.products = make(map[string]Product, len(data.Products))
		s.productOrder = make([]string, 0, len(data.Products))
		for i, p := range data.Products {
			if strings.TrimSpace(p.ID) == "" {
				p.ID = strconv.Itoa(i + 1)
			}
			if strings.TrimSpace(p.Category) == "" {
				p.Category = "General"
			}
			s.products[p.ID] = p
			s.productOrder = append(s.productOrder, p.ID)
		}
	}

	for _, c := range data.Customers {
		phone := NormalizePhone(c.WANumber)
		if phone == "" && strings.Contains(c.JID, "@") {
			phone = NormalizePhone(strings.Split(c.JID, "@")[0])
		}
		if phone == "" {
			continue
		}
		cp := c
		cp.WANumber = phone
		if cp.JID == "" {
			cp.JID = phone + "@s.whatsapp.net"
		}
		if cp.CurrentState == "" {
			cp.CurrentState = "chatting"
		}
		if cp.PaymentStatus == "" {
			cp.PaymentStatus = "UNPAID"
		}
		if cp.UpdatedAt.IsZero() {
			cp.UpdatedAt = time.Now()
		}
		s.customers[phone] = &cp
	}

	// Reconcile active carts from Central Brain Node
	for rawPhone, items := range data.ActiveCarts {
		phone := NormalizePhone(rawPhone)
		if phone == "" {
			continue
		}
		cleanItems := make([]CartItem, 0, len(items))
		for _, it := range items {
			if it.Qty <= 0 {
				it.Qty = 1
			}
			if it.UnitPrice <= 0 {
				if p, ok := s.findProductLocked(it.ProductID, it.ProductName); ok {
					_, resolvedPrice, resolvedVar, _ := p.ResolveVariantAndStock(it.SelectedVariant, it.Qty)
					if resolvedPrice > 0 {
						it.UnitPrice = resolvedPrice
					} else {
						it.UnitPrice = p.EffectivePrice()
					}
					if it.SelectedVariant == "" {
						it.SelectedVariant = resolvedVar
					}
					if it.ProductName == "" {
						it.ProductName = p.Name
					}
					if it.UnitType == "" {
						it.UnitType = p.UnitType
					}
					it.ProductID = p.ID
				}
			}
			if it.LineTotal <= 0 {
				it.LineTotal = it.UnitPrice * float64(it.Qty)
			}
			if it.UpdatedAt.IsZero() {
				it.UpdatedAt = time.Now()
			}
			cleanItems = append(cleanItems, it)
		}
		if len(cleanItems) == 0 {
			delete(s.activeCarts, phone)
		} else {
			s.activeCarts[phone] = cleanItems
		}
	}

	// Reconcile orders from Central Brain Node (Central is authoritative source of truth)
	for _, ord := range data.Orders {
		if strings.TrimSpace(ord.OrderID) == "" {
			continue
		}
		s.upsertOrderLocked(&ord, true)
	}

	if data.WhatsAppSession != nil {
		ws := *data.WhatsAppSession
		// Only hydrate session metadata if not already connected locally with a valid session
		if !s.waSession.HasSession || s.waSession.Status == "Needs_Login" || ws.HasSession {
			ws.NeedsLogin = !ws.HasSession
			if ws.HasSession && ws.Status == "" {
				ws.Status = "Connecting"
			} else if !ws.HasSession && ws.Status == "" {
				ws.Status = "Needs_Login"
			}
			s.waSession = ws
		}
	}

	now := time.Now()
	s.syncStatus.Bootstrapped = true
	s.syncStatus.Connected = true
	s.syncStatus.LastBootstrapAt = now
	s.syncStatus.LastSyncAt = now
	s.syncStatus.LastError = ""
}

// --- Vendor, AI, Payment getters & setters ---

func (s *Store) GetVendor() VendorProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.vendor
}

func (s *Store) UpdateVendor(v VendorProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.VendorID != "" {
		s.vendor.VendorID = v.VendorID
	}
	if v.Username != "" {
		s.vendor.Username = v.Username
	}
	if v.BusinessName != "" {
		s.vendor.BusinessName = v.BusinessName
	}
	if v.BusinessType != "" {
		s.vendor.BusinessType = v.BusinessType
	}
	if v.Language != "" {
		s.vendor.Language = v.Language
	}
	if v.StorePolicies != "" {
		s.vendor.StorePolicies = v.StorePolicies
	}
	if v.StoreSlug != "" {
		s.vendor.StoreSlug = v.StoreSlug
	}
	if v.StoreURL != "" {
		s.vendor.StoreURL = v.StoreURL
	}
	if v.MallURL != "" {
		s.vendor.MallURL = v.MallURL
	}
	if v.NotificationPhone != "" {
		s.vendor.NotificationPhone = v.NotificationPhone
	}
	if v.Latitude != 0 {
		s.vendor.Latitude = v.Latitude
	}
	if v.Longitude != 0 {
		s.vendor.Longitude = v.Longitude
	}
	if v.MaxDeliveryRadiusKM > 0 {
		s.vendor.MaxDeliveryRadiusKM = v.MaxDeliveryRadiusKM
	}
	if v.FreeDeliveryRadiusKM >= 0 {
		s.vendor.FreeDeliveryRadiusKM = v.FreeDeliveryRadiusKM
	}
	if v.DeliveryChargeAmount >= 0 {
		s.vendor.DeliveryChargeAmount = v.DeliveryChargeAmount
	}
	if v.OperatingHours != "" {
		s.vendor.OperatingHours = v.OperatingHours
	}
	if v.NextOpenTime != "" {
		s.vendor.NextOpenTime = v.NextOpenTime
	}
	if v.HolidayMessage != "" {
		s.vendor.HolidayMessage = v.HolidayMessage
	}
	if v.MinOrderAmount >= 0 {
		s.vendor.MinOrderAmount = v.MinOrderAmount
	}
	if v.EstimatedDeliveryTime != "" {
		s.vendor.EstimatedDeliveryTime = v.EstimatedDeliveryTime
	}
	if v.IsOpen != nil {
		s.vendor.IsOpen = v.IsOpen
	}
	s.vendor.HolidayMode = v.HolidayMode
	if v.SubscriptionActive != nil {
		s.vendor.SubscriptionActive = v.SubscriptionActive
	}
	if v.SubscriptionValidUntil != "" {
		s.vendor.SubscriptionValidUntil = v.SubscriptionValidUntil
	}
	if v.AdminAIAutoReplyAllowed != nil {
		s.vendor.AdminAIAutoReplyAllowed = v.AdminAIAutoReplyAllowed
	}
	if v.AdminPauseReason != "" {
		s.vendor.AdminPauseReason = v.AdminPauseReason
	}
	s.vendor.RequireCustomerLocation = v.RequireCustomerLocation
	s.vendor.AllowCOD = v.AllowCOD
}

func (s *Store) GetAI() AIConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ai
}

func (s *Store) SetAI(a AIConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Provider != "" {
		s.ai.Provider = a.Provider
	}
	if a.APIKey != "" {
		s.ai.APIKey = a.APIKey
	}
	if len(a.APIKeys) > 0 {
		s.ai.APIKeys = a.APIKeys
	}
	if len(a.BackupAPIKeys) > 0 {
		s.ai.BackupAPIKeys = a.BackupAPIKeys
	}
	if a.Model != "" {
		s.ai.Model = a.Model
	}
	if a.Temperature > 0 {
		s.ai.Temperature = a.Temperature
	}
	s.ai.SystemPrompt = a.SystemPrompt
	s.ai.AutoReply = a.AutoReply
	s.ai.ImportCatalog = a.ImportCatalog
}

func (s *Store) GetPayment() PaymentConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.payment
}

func (s *Store) SetPayment(p PaymentConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.CloudinaryCloudName == "" {
		p.CloudinaryCloudName = s.payment.CloudinaryCloudName
	}
	if p.PayeeName == "" {
		p.PayeeName = s.vendor.BusinessName
	}
	if !p.Enabled && (p.PaytmMID != "" || p.MerchantVPA != "" || p.Gateway != "") {
		p.Enabled = true
	}
	s.payment = p
}

// --- Products ---

func (s *Store) ReplaceProducts(items []Product) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.products = make(map[string]Product, len(items))
	s.productOrder = make([]string, 0, len(items))
	for i, p := range items {
		if strings.TrimSpace(p.ID) == "" {
			p.ID = strconv.Itoa(i + 1)
		}
		if strings.TrimSpace(p.Category) == "" {
			p.Category = "General"
		}
		s.products[p.ID] = p
		s.productOrder = append(s.productOrder, p.ID)
	}
}

func (s *Store) UpsertProduct(p Product) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(p.ID) == "" {
		p.ID = fmt.Sprintf("prod_%d", time.Now().UnixNano())
	}
	if strings.TrimSpace(p.Category) == "" {
		p.Category = "General"
	}
	if _, exists := s.products[p.ID]; !exists {
		s.productOrder = append(s.productOrder, p.ID)
	}
	s.products[p.ID] = p
}

func (s *Store) DeleteProduct(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.products, id)
	next := make([]string, 0, len(s.productOrder))
	for _, pid := range s.productOrder {
		if pid != id {
			next = append(next, pid)
		}
	}
	s.productOrder = next
}

func (s *Store) ListProducts(inStockOnly bool) []Product {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Product, 0, len(s.productOrder))
	for _, id := range s.productOrder {
		if p, ok := s.products[id]; ok {
			if inStockOnly && !p.IsProductInStock() {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) FindProduct(idOrName string) (Product, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findProductLocked(idOrName, idOrName)
}

func (s *Store) findProductLocked(id, name string) (Product, bool) {
	cleanID := strings.TrimSpace(id)
	if cleanID != "" {
		if p, ok := s.products[cleanID]; ok {
			return p, true
		}
	}
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if lowerName == "" {
		lowerName = strings.ToLower(cleanID)
	}
	if lowerName == "" {
		return Product{}, false
	}
	for _, p := range s.products {
		if strings.ToLower(strings.TrimSpace(p.Name)) == lowerName || strings.ToLower(strings.TrimSpace(p.ID)) == lowerName {
			return p, true
		}
	}
	for _, p := range s.products {
		if strings.Contains(strings.ToLower(p.Name), lowerName) {
			return p, true
		}
	}
	return Product{}, false
}

// --- Customers ---

func (s *Store) GetCustomer(phone string) *Customer {
	cleanPhone := NormalizePhone(phone)
	if cleanPhone == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.customers[cleanPhone]
	if !ok || c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// UpsertCustomer saves/updates a customer in RAM.
func (s *Store) UpsertCustomer(c *Customer) *Customer {
	saved, _ := s.UpsertCustomerWithChangeCheck(c)
	return saved
}

// UpsertCustomerWithChangeCheck saves/updates a customer in RAM and returns profileChanged=true ONLY when:
//   - the customer is brand new, OR
//   - FullName, ContactPhone, DeliveryAddress, Latitude, or Longitude actually changed.
//
// This guarantees the bot never spams Central Brain Node with duplicate customer sync requests on unchanged messages.
func (s *Store) UpsertCustomerWithChangeCheck(c *Customer) (*Customer, bool) {
	if c == nil {
		return nil, false
	}
	cleanPhone := NormalizePhone(c.WANumber)
	if cleanPhone == "" && strings.Contains(c.JID, "@") {
		cleanPhone = NormalizePhone(strings.Split(c.JID, "@")[0])
	}
	if cleanPhone == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, exists := s.customers[cleanPhone]
	if !exists || existing == nil {
		now := time.Now()
		cp := *c
		cp.WANumber = cleanPhone
		cp.FullName = strings.TrimSpace(cp.FullName)
		cp.DeliveryAddress = strings.TrimSpace(cp.DeliveryAddress)
		cp.ContactPhone = NormalizePhone(cp.ContactPhone)
		if cp.JID == "" {
			cp.JID = cleanPhone + "@s.whatsapp.net"
		}
		if cp.CurrentState == "" {
			cp.CurrentState = "chatting"
		}
		if cp.PaymentStatus == "" {
			cp.PaymentStatus = "UNPAID"
		}
		if cp.Source == "" {
			cp.Source = "whatsapp"
		}
		if cp.CreatedAt.IsZero() {
			cp.CreatedAt = now
		}
		cp.UpdatedAt = now
		s.customers[cleanPhone] = &cp
		res := cp
		return &res, true
	}

	profileChanged := false
	if jid := strings.TrimSpace(c.JID); jid != "" && jid != existing.JID {
		existing.JID = jid
	}
	if name := strings.TrimSpace(c.FullName); name != "" && name != existing.FullName {
		existing.FullName = name
		profileChanged = true
	}
	if addr := strings.TrimSpace(c.DeliveryAddress); addr != "" && addr != existing.DeliveryAddress {
		existing.DeliveryAddress = addr
		profileChanged = true
	}
	if contact := NormalizePhone(c.ContactPhone); contact != "" && contact != NormalizePhone(existing.ContactPhone) {
		existing.ContactPhone = contact
		profileChanged = true
	}
	if c.Latitude != 0 && c.Latitude != existing.Latitude {
		existing.Latitude = c.Latitude
		profileChanged = true
	}
	if c.Longitude != 0 && c.Longitude != existing.Longitude {
		existing.Longitude = c.Longitude
		profileChanged = true
	}
	if strings.TrimSpace(c.LastOrderDetails) != "" {
		existing.LastOrderDetails = strings.TrimSpace(c.LastOrderDetails)
	}
	if strings.TrimSpace(c.CurrentState) != "" {
		existing.CurrentState = strings.TrimSpace(c.CurrentState)
	}
	if strings.TrimSpace(c.PaymentStatus) != "" {
		existing.PaymentStatus = strings.TrimSpace(c.PaymentStatus)
	}
	if c.CurrentOrderID != "" {
		existing.CurrentOrderID = c.CurrentOrderID
	}
	if c.Source != "" {
		existing.Source = c.Source
	}
	existing.UpdatedAt = time.Now()

	res := *existing
	return &res, profileChanged
}

func (s *Store) SetCustomerState(phone, state string) *Customer {
	cleanPhone := NormalizePhone(phone)
	if cleanPhone == "" {
		return nil
	}
	if state == "" {
		state = "chatting"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.customers[cleanPhone]
	if !ok || c == nil {
		c = &Customer{
			WANumber:      cleanPhone,
			JID:           cleanPhone + "@s.whatsapp.net",
			PaymentStatus: "UNPAID",
			Source:        "whatsapp",
			CreatedAt:     time.Now(),
		}
		s.customers[cleanPhone] = c
	}
	c.CurrentState = state
	c.UpdatedAt = time.Now()
	cp := *c
	return &cp
}

func (s *Store) SetCustomerPaymentStatus(phone, status string) *Customer {
	cleanPhone := NormalizePhone(phone)
	if cleanPhone == "" {
		return nil
	}
	if status == "" {
		status = "UNPAID"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.customers[cleanPhone]
	if !ok || c == nil {
		c = &Customer{
			WANumber:     cleanPhone,
			JID:          cleanPhone + "@s.whatsapp.net",
			CurrentState: "chatting",
			Source:       "whatsapp",
			CreatedAt:    time.Now(),
		}
		s.customers[cleanPhone] = c
	}
	c.PaymentStatus = status
	c.UpdatedAt = time.Now()
	cp := *c
	return &cp
}

func (s *Store) SetCustomerCurrentOrderID(phone, orderID string) *Customer {
	cleanPhone := NormalizePhone(phone)
	if cleanPhone == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.customers[cleanPhone]
	if !ok || c == nil {
		c = &Customer{
			WANumber:      cleanPhone,
			JID:           cleanPhone + "@s.whatsapp.net",
			CurrentState:  "chatting",
			PaymentStatus: "UNPAID",
			Source:        "whatsapp",
			CreatedAt:     time.Now(),
		}
		s.customers[cleanPhone] = c
	}
	c.CurrentOrderID = orderID
	c.UpdatedAt = time.Now()
	cp := *c
	return &cp
}

func (s *Store) ListCustomers() []Customer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Customer, 0, len(s.customers))
	for _, c := range s.customers {
		if c != nil {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// --- Active Carts (Variant-Aware + Stock Validated) ---

func cartItemKey(productID, variant string) string {
	return strings.TrimSpace(productID) + "::" + strings.ToLower(strings.TrimSpace(variant))
}

func (s *Store) GetCart(phone string) []CartItem {
	cleanPhone := NormalizePhone(phone)
	if cleanPhone == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := s.activeCarts[cleanPhone]
	if len(items) == 0 {
		return nil
	}
	out := make([]CartItem, len(items))
	copy(out, items)
	return out
}

// UpsertCartItems merges/overwrites product+variant quantities for a customer cart in RAM.
// Validates stock when adding via WhatsApp (replaceAll == false) so out-of-stock items cannot be added.
func (s *Store) UpsertCartItems(phone string, incoming []CartItem, replaceAll bool) ([]CartItem, float64) {
	items, total, _ := s.UpsertCartItemsValidated(phone, incoming, replaceAll, !replaceAll)
	return items, total
}

// UpsertCartItemsValidated returns updated cart items, subtotal, and any stock-rejection messages.
func (s *Store) UpsertCartItemsValidated(phone string, incoming []CartItem, replaceAll bool, enforceStock bool) ([]CartItem, float64, []string) {
	cleanPhone := NormalizePhone(phone)
	if cleanPhone == "" {
		return nil, 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	byKey := make(map[string]CartItem)
	if !replaceAll {
		for _, ex := range s.activeCarts[cleanPhone] {
			if now.Sub(ex.UpdatedAt) <= 2*time.Hour {
				byKey[cartItemKey(ex.ProductID, ex.SelectedVariant)] = ex
			}
		}
	}

	var rejected []string
	for _, it := range incoming {
		prod, found := s.findProductLocked(it.ProductID, it.ProductName)
		pid := strings.TrimSpace(it.ProductID)
		pName := strings.TrimSpace(it.ProductName)
		unitPrice := it.UnitPrice
		unitType := strings.TrimSpace(it.UnitType)
		variantLabel := strings.TrimSpace(it.SelectedVariant)

		if found {
			pid = prod.ID
			pName = prod.Name
			if unitType == "" {
				unitType = prod.UnitType
			}
			if it.Qty > 0 {
				avail, resolvedPrice, resolvedVar, reason := prod.ResolveVariantAndStock(variantLabel, it.Qty)
				if enforceStock && !avail {
					rejected = append(rejected, reason)
					continue
				}
				if unitPrice <= 0 && resolvedPrice > 0 {
					unitPrice = resolvedPrice
				}
				if variantLabel == "" && resolvedVar != "" {
					variantLabel = resolvedVar
				}
			}
		}
		if pid == "" {
			continue
		}
		if pName == "" {
			pName = "Product #" + pid
		}
		key := cartItemKey(pid, variantLabel)
		if it.Qty <= 0 {
			delete(byKey, key)
			continue
		}
		src := it.Source
		if src == "" {
			src = "whatsapp"
		}
		byKey[key] = CartItem{
			ProductID:       pid,
			ProductName:     pName,
			UnitPrice:       unitPrice,
			UnitType:        unitType,
			Qty:             it.Qty,
			SelectedVariant: variantLabel,
			LineTotal:       unitPrice * float64(it.Qty),
			Source:          src,
			UpdatedAt:       now,
		}
	}

	result := make([]CartItem, 0, len(byKey))
	var total float64
	for _, it := range byKey {
		result = append(result, it)
		total += it.LineTotal
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ProductID == result[j].ProductID {
			return result[i].SelectedVariant < result[j].SelectedVariant
		}
		return result[i].ProductID < result[j].ProductID
	})

	if len(result) == 0 {
		delete(s.activeCarts, cleanPhone)
	} else {
		s.activeCarts[cleanPhone] = result
	}

	cp := make([]CartItem, len(result))
	copy(cp, result)
	return cp, total, rejected
}

func (s *Store) ClearCart(phone string) {
	cleanPhone := NormalizePhone(phone)
	if cleanPhone == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.activeCarts, cleanPhone)
	if c, ok := s.customers[cleanPhone]; ok && c != nil {
		c.CurrentOrderID = ""
		c.UpdatedAt = time.Now()
	}
}

func (s *Store) ListActiveCarts() map[string][]CartItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]CartItem, len(s.activeCarts))
	for k, v := range s.activeCarts {
		cp := make([]CartItem, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// --- Orders (Central Node Authoritative Fulfillment Status) ---

var orderStatusRank = map[string]int{
	"IN_PROGRESS":               1,
	"PENDING":                   1,
	"NEW":                       2,
	"CONFIRMED":                 2,
	"PROCESSING":                3,
	"PACKED":                    4,
	"DELIVERY_PARTNER_ASSIGNED": 5,
	"DISPATCHED":                5,
	"OUT_FOR_DELIVERY":          6,
	"DELIVERED":                 7,
	"CANCELLED":                 8,
}

func (s *Store) upsertOrderLocked(ord *Order, fromCentral bool) (*Order, bool) {
	if ord == nil || strings.TrimSpace(ord.OrderID) == "" {
		return nil, false
	}

	key := strings.ToUpper(strings.TrimSpace(ord.OrderID))
	o := *ord
	o.OrderID = strings.TrimSpace(ord.OrderID)
	o.CustomerPhone = NormalizePhone(o.CustomerPhone)
	if o.JID == "" && o.CustomerPhone != "" {
		o.JID = o.CustomerPhone + "@s.whatsapp.net"
	}
	if o.Source == "" {
		if fromCentral {
			o.Source = "website"
		} else {
			o.Source = "whatsapp"
		}
	}
	if o.OrderStatus == "" {
		o.OrderStatus = "CONFIRMED"
	} else {
		o.OrderStatus = strings.ToUpper(strings.TrimSpace(o.OrderStatus))
	}
	if o.PaymentStatus == "" {
		o.PaymentStatus = "PENDING"
	} else {
		o.PaymentStatus = strings.ToUpper(strings.TrimSpace(o.PaymentStatus))
	}

	now := time.Now()
	statusChanged := false

	if existing, ok := s.orders[key]; ok && existing != nil {
		if o.CreatedAt.IsZero() {
			o.CreatedAt = existing.CreatedAt
		}
		if len(o.Items) == 0 {
			o.Items = existing.Items
		}
		if o.TotalAmount <= 0 {
			o.TotalAmount = existing.TotalAmount
		}
		if o.Subtotal <= 0 {
			o.Subtotal = existing.Subtotal
		}
		if o.DeliveryFee <= 0 && existing.DeliveryFee > 0 {
			o.DeliveryFee = existing.DeliveryFee
		}
		if o.CustomerName == "" {
			o.CustomerName = existing.CustomerName
		}
		if o.DeliveryAddress == "" {
			o.DeliveryAddress = existing.DeliveryAddress
		}
		if o.DeliveryPhone == "" {
			o.DeliveryPhone = existing.DeliveryPhone
		}
		if o.CustomerPhone == "" {
			o.CustomerPhone = existing.CustomerPhone
		}
		if o.JID == "" {
			o.JID = existing.JID
		}
		if o.TxnID == "" {
			o.TxnID = existing.TxnID
		}
		if o.PaymentLink == "" {
			o.PaymentLink = existing.PaymentLink
		}
		if o.PaymentQRURL == "" {
			o.PaymentQRURL = existing.PaymentQRURL
		}
		if o.DeliveryPartnerName == "" {
			o.DeliveryPartnerName = existing.DeliveryPartnerName
		}
		if o.DeliveryPartnerPhone == "" {
			o.DeliveryPartnerPhone = existing.DeliveryPartnerPhone
		}
		if o.DeliveryOTP == "" {
			o.DeliveryOTP = existing.DeliveryOTP
		}
		if o.EstimatedDelivery == "" {
			o.EstimatedDelivery = existing.EstimatedDelivery
		}

		// Central Node is ALWAYS source of truth for order fulfillment status.
		// Never allow a stale local bot write to downgrade a Central-managed order status.
		if !fromCentral && existing.StatusUpdatedBy == "central" {
			existingRank := orderStatusRank[strings.ToUpper(existing.OrderStatus)]
			incomingRank := orderStatusRank[strings.ToUpper(o.OrderStatus)]
			if existingRank >= incomingRank {
				o.OrderStatus = existing.OrderStatus
				o.StatusUpdatedBy = existing.StatusUpdatedBy
			}
			if strings.EqualFold(existing.PaymentStatus, "PAID") && !strings.EqualFold(o.PaymentStatus, "PAID") {
				o.PaymentStatus = existing.PaymentStatus
			}
		} else if fromCentral {
			o.StatusUpdatedBy = "central"
		}

		if !strings.EqualFold(existing.OrderStatus, o.OrderStatus) ||
			(o.DeliveryPartnerName != "" && o.DeliveryPartnerName != existing.DeliveryPartnerName) {
			statusChanged = true
		}
	} else {
		if o.CreatedAt.IsZero() {
			o.CreatedAt = now
		}
		if fromCentral {
			o.StatusUpdatedBy = "central"
		} else if o.StatusUpdatedBy == "" {
			o.StatusUpdatedBy = "whatsapp"
		}
		s.orderIDs = append(s.orderIDs, key)
		statusChanged = true
	}

	o.UpdatedAt = now
	s.orders[key] = &o

	// Keep RAM bounded to last 500 orders
	if len(s.orderIDs) > 500 {
		oldest := s.orderIDs[0]
		s.orderIDs = s.orderIDs[1:]
		delete(s.orders, oldest)
	}

	cp := o
	return &cp, statusChanged
}

// UpsertOrder saves a local bot order while respecting Central-authoritative fulfillment status.
func (s *Store) UpsertOrder(ord *Order) *Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, _ := s.upsertOrderLocked(ord, false)
	return saved
}

// UpsertOrderFromCentral saves an authoritative order/delivery update from Central Brain Node or Delivery Portal.
// Returns (savedOrder, statusOrPartnerChanged).
func (s *Store) UpsertOrderFromCentral(ord *Order) (*Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertOrderLocked(ord, true)
}

func (s *Store) GetOrder(orderID string) *Order {
	key := strings.ToUpper(strings.TrimSpace(orderID))
	if key == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if o, ok := s.orders[key]; ok && o != nil {
		cp := *o
		return &cp
	}
	for k, o := range s.orders {
		if strings.HasSuffix(k, key) && o != nil {
			cp := *o
			return &cp
		}
	}
	return nil
}

func (s *Store) GetLatestOrderByPhone(phone string) *Order {
	cleanPhone := NormalizePhone(phone)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *Order
	for _, o := range s.orders {
		if o == nil {
			continue
		}
		if cleanPhone == "" || o.CustomerPhone == cleanPhone {
			if best == nil || o.UpdatedAt.After(best.UpdatedAt) {
				best = o
			}
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

func (s *Store) ListOrders() []Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Order, 0, len(s.orders))
	for _, o := range s.orders {
		if o != nil {
			out = append(out, *o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// --- Chats (Sliding Window strictly max 10 per customer in RAM, zero Central sync) ---

const MaxChatsPerCustomer = 10

func (s *Store) AppendChat(phone, role, direction, text string) ChatRecord {
	cleanPhone := NormalizePhone(phone)
	cleanText := strings.TrimSpace(text)
	if cleanPhone == "" || cleanText == "" {
		return ChatRecord{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.chats[cleanPhone]
	now := time.Now()
	// Prevent back-to-back duplicate saves (e.g., immediate save + post-AI sync) from consuming 2 of the 10 RAM slots
	if n := len(existing); n > 0 {
		last := existing[n-1]
		if last.Role == role && last.Text == cleanText && now.Sub(last.CreatedAt) < 10*time.Second {
			return last
		}
	}

	s.chatSeq++
	rec := ChatRecord{
		ID:            s.chatSeq,
		CustomerPhone: cleanPhone,
		Role:          role,
		Direction:     direction,
		Text:          cleanText,
		Time:          now.Format("15:04"),
		CreatedAt:     now,
	}
	list := append(existing, rec)
	if len(list) > MaxChatsPerCustomer {
		// Copy into a fresh 10-element slice so dropped older messages are immediately garbage-collected
		trimmed := make([]ChatRecord, MaxChatsPerCustomer)
		copy(trimmed, list[len(list)-MaxChatsPerCustomer:])
		list = trimmed
	}
	s.chats[cleanPhone] = list
	return rec
}

func (s *Store) GetChatMessages(phone string) []ChatRecord {
	cleanPhone := NormalizePhone(phone)
	if cleanPhone == "" {
		return []ChatRecord{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.chats[cleanPhone]
	out := make([]ChatRecord, len(list))
	copy(out, list)
	return out
}

type ConversationSummary struct {
	CustomerPhone string `json:"customer_phone"`
	CustomerName  string `json:"customer_name"`
	LastMessage   string `json:"last_message"`
	LastRole      string `json:"last_role"`
	LastTime      string `json:"last_time"`
	TotalCount    int    `json:"total_count"`
	UpdatedAt     int64  `json:"updated_at"`
}

func (s *Store) ListConversations() []ConversationSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ConversationSummary, 0, len(s.chats))
	for phone, msgs := range s.chats {
		if len(msgs) == 0 {
			continue
		}
		last := msgs[len(msgs)-1]
		name := ""
		if c, ok := s.customers[phone]; ok && c != nil {
			name = c.FullName
		}
		out = append(out, ConversationSummary{
			CustomerPhone: phone,
			CustomerName:  name,
			LastMessage:   last.Text,
			LastRole:      last.Role,
			LastTime:      last.Time,
			TotalCount:    len(msgs),
			UpdatedAt:     last.CreatedAt.Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

// --- Leads & Bulk Logs ---

func (s *Store) AddLead(phone, inquiryText, status string) Lead {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := Lead{
		ID:            fmt.Sprintf("lead_%d", time.Now().UnixNano()),
		CustomerPhone: NormalizePhone(phone),
		InquiryText:   strings.TrimSpace(inquiryText),
		Status:        status,
		CreatedAt:     time.Now(),
	}
	s.leads = append([]Lead{l}, s.leads...)
	if len(s.leads) > 200 {
		s.leads = s.leads[:200]
	}
	return l
}

func (s *Store) ListLeads() []Lead {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Lead, len(s.leads))
	copy(out, s.leads)
	return out
}

func (s *Store) AddBulkLog(item BulkLogItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bulkLogs = append([]BulkLogItem{item}, s.bulkLogs...)
	if len(s.bulkLogs) > 100 {
		s.bulkLogs = s.bulkLogs[:100]
	}
}

func (s *Store) ListBulkLogs() []BulkLogItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]BulkLogItem, len(s.bulkLogs))
	copy(out, s.bulkLogs)
	return out
}

// --- WhatsApp Session & Sync Status ---

func (s *Store) GetWhatsAppSession() WhatsAppSessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.waSession
}

func (s *Store) UpdateWhatsAppSession(fn func(*WhatsAppSessionState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.waSession)
}

func (s *Store) GetSyncStatus() SyncStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.syncStatus
}

func (s *Store) UpdateSyncStatus(fn func(*SyncStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.syncStatus)
}

// Stats returns counts of all RAM-cached vendor entities for the overview dashboard.
func (s *Store) Stats() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inStock := 0
	for _, p := range s.products {
		if p.IsProductInStock() {
			inStock++
		}
	}
	return map[string]int{
		"products":          len(s.products),
		"in_stock_products": inStock,
		"customers":         len(s.customers),
		"active_carts":      len(s.activeCarts),
		"orders":            len(s.orders),
		"conversations":     len(s.chats),
		"leads":             len(s.leads),
	}
}
