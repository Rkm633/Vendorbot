package whatsapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"wa-server/ramstore"
)

// ChatMessage structure for Sliding Window
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type CachedChatSession struct {
	LastActivity time.Time
	Messages     []ChatMessage
}

type UserConfigRAM struct {
	Provider                string
	APIKey                  string
	AllAPIKeys              []string
	BusinessName            string
	BusinessType            string
	Language                string
	SystemPrompt            string
	Model                   string
	Temperature             float64
	AutoReply               bool
	ImportCatalog           bool
	CatalogText             string
	StorePolicies           string
	HasOnlinePayment        bool
	PaytmMid                string
	MerchantVpa             string
	PayeeName               string
	MaxDeliveryRadiusKM     float64
	FreeDeliveryRadiusKM    float64
	DeliveryChargeAmount    float64
	RequireCustomerLocation bool
	AllowCOD                bool
	IsOpen                  bool
	HolidayMode             bool
	HolidayMessage          string
	OperatingHours          string
	NextOpenTime            string
	MinOrderAmount          float64
	EstimatedDeliveryTime   string
	LastCached              time.Time
}

type AICartItem struct {
	ProductID string `json:"product_id"`
	Variant   string `json:"variant,omitempty"`
	Qty       int    `json:"qty"`
}

type AIProfileData struct {
	Address       string `json:"address,omitempty"`
	Name          string `json:"name,omitempty"`
	ContactNumber string `json:"contact_number,omitempty"`
}

type AIResponseStateJSON struct {
	Action          string         `json:"action"`
	ReplyToCustomer string         `json:"reply_to_customer"`
	CartItems       []AICartItem   `json:"cart_items,omitempty"`
	ProfileData     *AIProfileData `json:"profile_data,omitempty"`
}

// -----------------------------------------------------------------------------
// OCTOPUS AGENTIC ARCHITECTURE: MULTI-ACTION ROUTING JSON SCHEMA
// -----------------------------------------------------------------------------

type FlexInt int

func (fi *FlexInt) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*fi = FlexInt(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		s = strings.TrimSpace(s)
		if val, err2 := strconv.Atoi(s); err2 == nil {
			*fi = FlexInt(val)
			return nil
		}
	}
	var f float64
	if err := json.Unmarshal(b, &f); err == nil {
		*fi = FlexInt(int(f))
		return nil
	}
	*fi = 1
	return nil
}

type AICartItemPayload struct {
	ID      string  `json:"id"`
	Variant string  `json:"variant,omitempty"`
	Qty     FlexInt `json:"qty"`
}

type AIProfileDataPayload struct {
	Name                   string `json:"name,omitempty"`
	Address                string `json:"address,omitempty"`
	Contact                string `json:"contact,omitempty"`
	DeliveryAddress        string `json:"delivery_address,omitempty"`
	DeliveryContact10Digit string `json:"delivery_contact_10_digit,omitempty"`
}

type AIActionPayload struct {
	Category        string                `json:"category,omitempty"`
	ProductCategory string                `json:"product_category,omitempty"`
	OrderID         string                `json:"order_id,omitempty"`
	PaymentMode     string                `json:"payment_mode,omitempty"` // "COD" or "ONLINE"
	CartItems       []AICartItemPayload   `json:"cart_items,omitempty"`
	Profile         *AIProfileDataPayload `json:"profile,omitempty"`
	ProfileData     *AIProfileDataPayload `json:"profile_data,omitempty"`
}

type AIActionItem struct {
	Hand        string           `json:"hand"`
	Payload     *AIActionPayload `json:"payload,omitempty"`
	ExecuteHand string           `json:"execute_hand,omitempty"`
	Action      string           `json:"action,omitempty"`
}

type OctopusAIResponse struct {
	AIReplyText string         `json:"ai_reply_text"`
	Actions     []AIActionItem `json:"actions,omitempty"`

	ExecuteHand     string           `json:"execute_hand,omitempty"`
	HandPayload     *AIActionPayload `json:"hand_payload,omitempty"`
	Action          string           `json:"action,omitempty"`
	ReplyToCustomer string           `json:"reply_to_customer,omitempty"`
	CartItems       []AICartItem     `json:"cart_items,omitempty"`
	ProfileData     *AIProfileData   `json:"profile_data,omitempty"`
}

const SafeAIFallbackReply = "> ✨ *NAMASTE JI* 🙏\n> _Aapki sahayata ke liye hum hamesha hazir hain!_\n\nMaaf kijiye, main aapka sandesh theek se samajh nahi paya. Kripya apna sawal ya item ka naam dobara batayein! 😊🍕"

var jsonExtractorRegex = regexp.MustCompile(`(?s)\{.*\}`)

const octopusSystemPromptJSON = `You are the brain of the automated store sales & support agent for "%s" (a %s).
Store Policy: %s
Your output language MUST BE strictly: %s.

CRITICAL INSTRUCTION: You MUST output ONLY a valid, raw JSON object. Do NOT wrap the JSON in markdown code blocks (like ` + "```json" + `), asterisks, or any explanatory text. NEVER write asterisks around key names (NEVER write *ai_reply_text*: or *actions*:). Your response MUST strictly start with '{' and end with '}'.

You are the brain. You must understand the user's intent, write a beautifully designed, polite, human-like reply, and command the backend to execute one or more physical actions using this strict JSON schema:

{
  "ai_reply_text": "String (Your beautifully designed reply in %s with clean headings, rich emojis, and bold styling)",
  "actions": [
    {
      "hand": "String (Choose from: send_catalog_images, update_cart, extract_profile, submit_order, generate_store_link, clear_cart, cancel_order, escalate_to_human, chat_only)",
      "payload": {
        "category": "String (Optional product name or category, e.g. 'all' or 'Kaju Katli' or 'Snacks')",
        "payment_mode": "String (Required for submit_order: 'COD' or 'ONLINE')",
        "cart_items": [{"id": "String", "variant": "String (Optional exact variant label e.g. '500g')", "qty": 1}],
        "profile": {"name": "", "address": "", "contact": ""}
      }
    }
  ]
}

VISUAL DESIGN & BEAUTIFUL FORMATTING GUIDELINES (WHATSAPP AESTHETICS):
1. Do NOT place WhatsApp blockquote markers (>) on every line or on ordinary sentences. Use bold headings with friendly emojis and clean bullet points (• ).
2. When greeting or when the customer asks what is available, clearly list categories and key IN-STOCK products with prices from the live inventory provided below, and offer to send product photos.
3. If the customer asks for photos/images of any product or category, trigger: {"hand": "send_catalog_images", "payload": {"category": "<product name or category or all>"}}.
4. If the customer asks to speak with the store owner/malik, trigger {"hand": "escalate_to_human", "payload": {}} AND keep assisting them warmly without locking the chat.

MASTER WORKFLOW - CENTRAL BRAIN CONTROLLED CHECKOUT:
- PHASE 1 (Greeting & Browsing): Show available categories & IN-STOCK items from LIVE INVENTORY and offer product photos.
- PHASE 2 (Adding to Cart): Match items with live catalog IDs & trigger {"hand": "update_cart", "payload": {"cart_items": [{"id": "...", "variant": "...", "qty": 1}]}}.
- PHASE 3 (Profile Extraction & Summary): Save name/address/contact via {"hand": "extract_profile", "payload": {"profile": {"name": "", "address": "", "contact": ""}}}, show full order summary, and ask whether they prefer Online Payment or Cash on Delivery (COD).
- PHASE 4 (Submit Order to Central Brain): Once customer has items in cart + delivery address + chooses Online Payment or COD, trigger {"hand": "submit_order", "payload": {"payment_mode": "ONLINE"}} or {"hand": "submit_order", "payload": {"payment_mode": "COD"}}. Central Brain Node controls 100% of payment links/QR, payment verification, and final order confirmation + PDF receipt.
- PHASE 5 (Owner Escalation): Trigger {"hand": "escalate_to_human"} while continuing to assist.
- PHASE 6 (Omnichannel Storefront & Mall Magic Link): When user asks for website/store/mall link, trigger {"hand": "generate_store_link"}.

STRICT PAYMENT & SECURITY RULES:
1. NEVER ask customers for UTR numbers or payment screenshots, NEVER scan/verify any payment screenshot or UTR number, and NEVER mark an order as Paid or Confirmed on your own.
2. If a customer says they paid or sends a payment screenshot, politely inform them that online payments are verified automatically by our Central Payment System and final order confirmation + receipt will arrive automatically once verified.
3. Use ONLY the REAL-TIME RAM STORE STATE (current_state, shopping_cart, customer_profile, rag_results) injected below.
4. OUTPUT ONLY THE RAW JSON OBJECT. NO PRETEXT, NO POSTTEXT, NO MARKDOWN OUTSIDE JSON.`

// BuildUniversalSystemPrompt builds the Octopus system prompt dynamically from RAM vendor settings.
func BuildUniversalSystemPrompt(businessName, businessType, language, storePolicies, catalogContext string) string {
	bName := strings.TrimSpace(businessName)
	if bName == "" {
		bName = "Vendor Store"
	}
	bType := strings.TrimSpace(businessType)
	if bType == "" {
		bType = "Retail / Mall Store"
	}
	lang := strings.TrimSpace(language)
	if lang == "" {
		lang = "Hinglish"
	}
	policies := strings.TrimSpace(storePolicies)
	if policies == "" {
		policies = "Standard delivery and returns applicable."
	}

	finalPrompt := fmt.Sprintf(octopusSystemPromptJSON, bName, bType, policies, lang, lang)
	if strings.TrimSpace(catalogContext) != "" {
		finalPrompt += "\n\n" + strings.TrimSpace(catalogContext)
	}
	return finalPrompt
}

var (
	chatMemoryRAM    = make(map[string]*CachedChatSession)
	ramCacheLock     sync.Mutex
	keyRotationIndex = make(map[string]int)
	keyRotationLock  sync.Mutex
)

func init() {
	// Evict idle LLM chat windows after 15 minutes to keep 512MB Render container RAM clean
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for range ticker.C {
			ramCacheLock.Lock()
			now := time.Now()
			for sessionKey, session := range chatMemoryRAM {
				if session != nil && now.Sub(session.LastActivity) > 15*time.Minute {
					delete(chatMemoryRAM, sessionKey)
				}
			}
			ramCacheLock.Unlock()
		}
	}()
}

// InvalidateUserConfigCache is kept as a lightweight hook since GetUserConfigRAM reads live from ramstore.Global.
func InvalidateUserConfigCache(_ string) {}

// GetUserConfigRAM returns instant 0ms vendor AI/business/payment config directly from ramstore.Global.
func GetUserConfigRAM(_ string) *UserConfigRAM {
	v := ramstore.Global.GetVendor()
	a := ramstore.Global.GetAI()
	p := ramstore.Global.GetPayment()

	model := strings.TrimSpace(a.Model)
	if model == "" {
		model = "llama-3.3-70b-versatile"
	}
	provider := strings.TrimSpace(a.Provider)
	if provider == "" {
		provider = "groq"
	}
	temp := a.Temperature
	if temp <= 0 {
		temp = 0.7
	}
	payee := strings.TrimSpace(p.PayeeName)
	if payee == "" {
		payee = v.BusinessName
	}

	isOpen, _ := v.IsStoreOpenNow()
	return &UserConfigRAM{
		Provider:                provider,
		APIKey:                  strings.TrimSpace(a.APIKey),
		AllAPIKeys:              a.AllAPIKeys(),
		BusinessName:            v.BusinessName,
		BusinessType:            v.BusinessType,
		Language:                v.Language,
		SystemPrompt:            a.SystemPrompt,
		Model:                   model,
		Temperature:             temp,
		AutoReply:               a.AutoReply,
		ImportCatalog:           true,
		CatalogText:             GetCatalogContext(""),
		StorePolicies:           v.StorePolicies,
		HasOnlinePayment:        p.Enabled || (p.PaytmMID != "" && p.MerchantVPA != ""),
		PaytmMid:                strings.TrimSpace(p.PaytmMID),
		MerchantVpa:             strings.TrimSpace(p.MerchantVPA),
		PayeeName:               payee,
		MaxDeliveryRadiusKM:     v.MaxDeliveryRadiusKM,
		FreeDeliveryRadiusKM:    v.FreeDeliveryRadiusKM,
		DeliveryChargeAmount:    v.DeliveryChargeAmount,
		RequireCustomerLocation: v.RequireCustomerLocation,
		AllowCOD:                v.AllowCOD,
		IsOpen:                  isOpen,
		HolidayMode:             v.HolidayMode,
		HolidayMessage:          v.HolidayMessage,
		OperatingHours:          v.OperatingHours,
		NextOpenTime:            v.NextOpenTime,
		MinOrderAmount:          v.MinOrderAmount,
		EstimatedDeliveryTime:   v.EstimatedDeliveryTime,
		LastCached:              time.Now(),
	}
}

// collectCandidateAPIKeys builds a rotated, deduplicated fallback pool across vendor primary + backup keys + env fallbacks.
func collectCandidateAPIKeys(username string, cfg *UserConfigRAM) []string {
	seen := make(map[string]bool)
	var keys []string
	addTokens := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		re := regexp.MustCompile(`[\n,\s]+`)
		for _, t := range re.Split(raw, -1) {
			t = strings.TrimSpace(t)
			if t != "" && !seen[t] {
				seen[t] = true
				keys = append(keys, t)
			}
		}
	}
	if cfg != nil {
		addTokens(cfg.APIKey)
		for _, k := range cfg.AllAPIKeys {
			addTokens(k)
		}
	}
	for _, envKey := range []string{"GROQ_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY"} {
		addTokens(os.Getenv(envKey))
	}
	if len(keys) <= 1 {
		return keys
	}

	keyRotationLock.Lock()
	start := keyRotationIndex[username] % len(keys)
	keyRotationIndex[username] = (start + 1) % len(keys)
	keyRotationLock.Unlock()

	rotated := make([]string, 0, len(keys))
	rotated = append(rotated, keys[start:]...)
	rotated = append(rotated, keys[:start]...)
	return rotated
}

// callAIWithKeyFallback automatically switches to the next backup API key on 429 rate limits, 401, or network errors.
func callAIWithKeyFallback(username string, cfg *UserConfigRAM, sysPrompt string, history []ChatMessage) (string, error) {
	keys := collectCandidateAPIKeys(username, cfg)
	if len(keys) == 0 {
		return "", fmt.Errorf("no AI API key configured in vendor AI settings or environment")
	}
	var lastErr error
	for idx, key := range keys {
		res, err := CallAIModelWithHistory(cfg.Provider, cfg.Model, cfg.Temperature, key, sysPrompt, history)
		if err == nil && strings.TrimSpace(res) != "" {
			return res, nil
		}
		lastErr = err
		if idx < len(keys)-1 {
			log.Printf("🔄 [AI Multi-Key Fallback] Key #%d failed (%v) — switching automatically to backup key #%d...", idx+1, err, idx+2)
		}
	}
	return "", lastErr
}

func resolveAPIKey(username, cfgKey string) string {
	keys := collectCandidateAPIKeys(username, &UserConfigRAM{APIKey: cfgKey})
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func compactText(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "..."
	}
	return s
}

// updateSlidingWindowRAM keeps max 10 messages per customer in RAM, re-hydrating from ramstore.Global if evicted.
func updateSlidingWindowRAM(_, customerPhone, role, text string) []ChatMessage {
	cleanPhone := FormatPhone(customerPhone)
	sessionKey := cleanPhone

	ramCacheLock.Lock()
	defer ramCacheLock.Unlock()

	session, exists := chatMemoryRAM[sessionKey]
	if !exists || session == nil || len(session.Messages) == 0 {
		var seeded []ChatMessage
		for _, rec := range ramstore.Global.GetChatMessages(cleanPhone) {
			r := "user"
			if rec.Role == "assistant" || rec.Direction == "outgoing_ai" || rec.Direction == "outgoing_manual" {
				r = "assistant"
			}
			c := compactText(rec.Text, 400)
			if c != "" {
				seeded = append(seeded, ChatMessage{Role: r, Content: c})
			}
		}
		session = &CachedChatSession{
			LastActivity: time.Now(),
			Messages:     seeded,
		}
		chatMemoryRAM[sessionKey] = session
	}

	if text != "" {
		normalRole := role
		if normalRole == "customer" || normalRole == "incoming" {
			normalRole = "user"
		} else if normalRole == "outgoing" || normalRole == "outgoing_ai" {
			normalRole = "assistant"
		}
		cleanText := compactText(text, 400)
		dup := len(session.Messages) > 0 && session.Messages[len(session.Messages)-1].Role == normalRole && session.Messages[len(session.Messages)-1].Content == cleanText
		if !dup && cleanText != "" {
			session.Messages = append(session.Messages, ChatMessage{Role: normalRole, Content: cleanText})
		}
	}

	if len(session.Messages) > ramstore.MaxChatsPerCustomer {
		trimmed := make([]ChatMessage, ramstore.MaxChatsPerCustomer)
		copy(trimmed, session.Messages[len(session.Messages)-ramstore.MaxChatsPerCustomer:])
		session.Messages = trimmed
	}
	session.LastActivity = time.Now()

	copied := make([]ChatMessage, len(session.Messages))
	copy(copied, session.Messages)
	return copied
}

func getSlidingWindowRAM(username, customerPhone string) []ChatMessage {
	cleanPhone := FormatPhone(customerPhone)
	ramCacheLock.Lock()
	session, exists := chatMemoryRAM[cleanPhone]
	ramCacheLock.Unlock()
	if !exists || session == nil || len(session.Messages) == 0 {
		return updateSlidingWindowRAM(username, cleanPhone, "", "")
	}
	ramCacheLock.Lock()
	session.LastActivity = time.Now()
	copied := make([]ChatMessage, len(session.Messages))
	copy(copied, session.Messages)
	ramCacheLock.Unlock()
	return copied
}

// GetCatalogContext formats all in-stock RAM products grouped by category.
func GetCatalogContext(_ string) string {
	products := ramstore.Global.ListProducts(true)
	if len(products) == 0 {
		return ""
	}
	catMap := make(map[string][]ramstore.Product)
	var catOrder []string
	for _, p := range products {
		cat := strings.TrimSpace(p.Category)
		if cat == "" {
			cat = "General"
		}
		if _, ok := catMap[cat]; !ok {
			catOrder = append(catOrder, cat)
		}
		catMap[cat] = append(catMap[cat], p)
	}

	var sb strings.Builder
	sb.WriteString("Product Catalog (In Stock Items):\n")
	for _, cat := range catOrder {
		sb.WriteString(fmt.Sprintf("\nCategory: %s\n", cat))
		for _, p := range catMap[cat] {
			line := fmt.Sprintf("- product_id: %s | %s: Rs. %.2f", p.ID, p.Name, p.EffectivePrice())
			if unit := strings.TrimSpace(p.UnitType); unit != "" && unit != "custom" {
				line += "/" + unit
			}
			if desc := strings.TrimSpace(p.Description); desc != "" {
				line += " - " + desc
			}
			sb.WriteString(line + "\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

// Clean10DigitPhone extracts strictly a 10-digit Indian phone number.
func Clean10DigitPhone(raw string) string {
	digits := regexp.MustCompile(`\D`).ReplaceAllString(raw, "")
	if len(digits) >= 10 {
		return digits[len(digits)-10:]
	}
	return digits
}

// GetLiveInventoryRAG reads in-stock products directly from RAM for instant AI prompt injection.
func GetLiveInventoryRAG(_ string) string {
	products := ramstore.Global.ListProducts(true)
	if len(products) == 0 {
		return "Currently, all items are being updated in the inventory."
	}

	catMap := make(map[string][]ramstore.Product)
	var catOrder []string
	for _, p := range products {
		cat := strings.TrimSpace(p.Category)
		if cat == "" {
			cat = "General"
		}
		if _, ok := catMap[cat]; !ok {
			catOrder = append(catOrder, cat)
		}
		catMap[cat] = append(catMap[cat], p)
	}

	var b strings.Builder
	b.WriteString("STORE CATEGORIES & PRODUCTS AVAILABLE (LIVE IN-STOCK RAM INVENTORY — NEVER SUGGEST OUT-OF-STOCK ITEMS):\n")
	for _, c := range catOrder {
		b.WriteString(fmt.Sprintf("\n📂 Category: %s\n", c))
		for _, p := range catMap[c] {
			unitPart := ""
			if strings.TrimSpace(p.UnitType) != "" {
				unitPart = "/" + strings.TrimSpace(p.UnitType)
			}
			stockPart := ""
			if p.StockQty != nil {
				stockPart = fmt.Sprintf(", Stock: %d", *p.StockQty)
			}
			line := fmt.Sprintf("  • %s - ₹%.2f%s (ID: %s%s)", p.Name, p.EffectivePrice(), unitPart, p.ID, stockPart)
			if len(p.Variants) > 0 {
				var vParts []string
				for _, vr := range p.Variants {
					if !vr.IsVariantAvailable(1) {
						continue
					}
					vParts = append(vParts, fmt.Sprintf("%s: ₹%.2f", vr.DisplayLabel(), vr.EffectivePrice(p.EffectivePrice())))
				}
				if len(vParts) > 0 {
					line += " | Variants: [" + strings.Join(vParts, ", ") + "]"
				}
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// GetRAGCatalogResults searches in-stock RAM products matching customer keywords.
func GetRAGCatalogResults(_, userMsg string) string {
	products := ramstore.Global.ListProducts(true)
	if len(products) == 0 {
		return "No products available in catalog."
	}

	stopWords := map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "hai": true, "kya": true,
		"karo": true, "chahiye": true, "dena": true, "please": true, "mera": true,
		"meri": true, "mere": true, "bhai": true, "aap": true, "tum": true, "order": true,
		"mujhe": true, "dijiye": true, "bhejo": true, "hoga": true, "photos": true,
	}

	var keywords []string
	for _, t := range strings.Fields(strings.ToLower(userMsg)) {
		t = strings.Trim(t, ".,!?-_\"'#")
		if len(t) >= 3 && !stopWords[t] {
			keywords = append(keywords, t)
		}
	}

	var matched []ramstore.Product
	if len(keywords) > 0 {
		for _, p := range products {
			haystack := strings.ToLower(p.Name + " " + p.Category + " " + p.Description)
			for _, kw := range keywords {
				if strings.Contains(haystack, kw) {
					matched = append(matched, p)
					break
				}
			}
			if len(matched) >= 10 {
				break
			}
		}
	}
	if len(matched) == 0 {
		limit := len(products)
		if limit > 12 {
			limit = 12
		}
		matched = products[:limit]
	}

	var sb strings.Builder
	for _, p := range matched {
		hasImg := "No photo"
		if strings.TrimSpace(p.ThumbURL) != "" || strings.TrimSpace(p.ImageURL) != "" {
			hasImg = "Photo available"
		}
		sb.WriteString(fmt.Sprintf("- Product [id: \"%s\"]: %s (Category: %s, Price: Rs. %.2f, %s)\n",
			p.ID, p.Name, p.Category, p.EffectivePrice(), hasImg))
	}
	return strings.TrimSpace(sb.String())
}

// BuildOctopusContext builds real-time RAM state context string for the AI Brain.
func BuildOctopusContext(username, customerPhone, userMsg string) string {
	cleanPhone := FormatPhone(customerPhone)
	payStatus := GetCustomerPaymentStatus(username, cleanPhone)
	custState := GetCustomerState(username, cleanPhone)
	calcRes, _ := CalculateCartTotalAndFetchCredentials(username, cleanPhone)

	currentState := "BROWSING"
	if strings.EqualFold(payStatus, "PENDING") || strings.EqualFold(custState, "PAYMENT_PENDING") {
		currentState = "PAYMENT_PENDING"
	} else if calcRes != nil && len(calcRes.Items) > 0 {
		currentState = "CART_ACTIVE"
	}

	var cartItemsDesc []string
	var cartTotal float64
	if calcRes != nil {
		cartTotal = calcRes.TotalAmount
		for _, it := range calcRes.Items {
			varStr := ""
			if it.SelectedVariant != "" {
				varStr = ", Variant: " + it.SelectedVariant
			}
			cartItemsDesc = append(cartItemsDesc, fmt.Sprintf("    * %s (ID: %s%s, Qty: %d, Price: Rs. %.2f, Subtotal: Rs. %.2f)",
				it.ProductName, it.ProductID, varStr, it.Qty, it.UnitPrice, it.LineTotal))
		}
	}
	cartStr := "Empty (Total: Rs. 0.00)"
	if len(cartItemsDesc) > 0 {
		cartStr = fmt.Sprintf("Total Amount: Rs. %.2f\n  Items:\n%s", cartTotal, strings.Join(cartItemsDesc, "\n"))
	}

	custProfile, _ := GetCustomerProfile(username, cleanPhone)
	cName, cAddr, cPhone := "Not provided", "Not provided", "Not provided"
	if custProfile != nil {
		if custProfile.FullName != "" {
			cName = custProfile.FullName
		}
		if custProfile.DeliveryAddress != "" {
			cAddr = custProfile.DeliveryAddress
		}
		if custProfile.ContactPhone != "" {
			cPhone = custProfile.ContactPhone
		}
	}

	ragStr := GetRAGCatalogResults(username, userMsg)
	savedOrdID := GetCustomerCurrentOrderID(username, cleanPhone)

	return fmt.Sprintf(`REAL-TIME RAM STORE STATE (SYNCED WITH CENTRAL MALL BRAIN):
- current_state: %s
- shopping_cart: %s
- customer_profile:
    * Name: %s
    * WhatsApp Number: %s
    * Delivery Address: %s
    * Delivery Contact Number: %s
- saved_order_id: %s
- rag_results (Matching products/catalog):
%s`, currentState, cartStr, cName, cleanPhone, cAddr, cPhone, savedOrdID, ragStr)
}

func ExtractJSONSubstring(raw string) string {
	return strings.TrimSpace(jsonExtractorRegex.FindString(strings.TrimSpace(raw)))
}

func SanitizeAIReplyText(s string) string {
	res := strings.TrimSpace(s)
	if res == "" {
		return ""
	}
	delims := []string{"*actions*:", "**actions**:", "\"actions\":", "actions:", "[actions]"}
	for _, delim := range delims {
		if idx := strings.Index(strings.ToLower(res), delim); idx != -1 {
			res = strings.TrimSpace(res[:idx])
		}
	}
	for {
		trimmed := strings.TrimSpace(res)
		lower := strings.ToLower(trimmed)
		prefixMatched := false
		prefixes := []string{
			"*ai_reply_text*:", "**ai_reply_text**:", "ai_reply_text:", "\"ai_reply_text\":",
			"*reply_to_customer*:", "reply_to_customer:", "\"reply_to_customer\":",
		}
		for _, p := range prefixes {
			if strings.HasPrefix(lower, p) {
				res = strings.TrimSpace(trimmed[len(p):])
				prefixMatched = true
				break
			}
		}
		if !prefixMatched {
			break
		}
	}

	lines := strings.Split(res, "\n")
	var cleanedLines []string
	for _, l := range lines {
		trimL := strings.TrimSpace(l)
		if trimL == ">" || trimL == "> _" || trimL == "> *" {
			continue
		}
		if strings.HasPrefix(trimL, "> ") {
			after := strings.TrimSpace(trimL[2:])
			if strings.HasPrefix(after, "💳") || strings.HasPrefix(after, "🚨") || strings.HasPrefix(after, "🧾") {
				cleanedLines = append(cleanedLines, trimL)
			} else {
				cleanedLines = append(cleanedLines, after)
			}
		} else {
			cleanedLines = append(cleanedLines, l)
		}
	}
	res = strings.Join(cleanedLines, "\n")
	res = strings.Trim(res, " ,\"'}{][\n\r*`")
	if res == "" || strings.HasPrefix(res, "{") || strings.HasPrefix(res, "[") {
		return ""
	}
	return res
}

func tryExtractPseudoAIReply(raw string) string {
	lines := strings.Split(raw, "\n")
	var replyLines []string
	collecting := false
	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		lower := strings.ToLower(trimmedLine)
		if strings.HasPrefix(lower, "*actions*:") || strings.HasPrefix(lower, "actions:") || strings.HasPrefix(lower, "- actions:") || strings.HasPrefix(lower, "\"actions\":") {
			break
		}
		if strings.Contains(lower, "ai_reply_text") || strings.Contains(lower, "reply_to_customer") {
			if idx := strings.Index(trimmedLine, ":"); idx != -1 {
				val := strings.Trim(strings.TrimSpace(trimmedLine[idx+1:]), "\"* ")
				if val != "" {
					replyLines = append(replyLines, val)
				}
				collecting = true
				continue
			}
		}
		if collecting {
			if strings.HasPrefix(trimmedLine, "{") || strings.HasPrefix(trimmedLine, "}") || strings.HasPrefix(trimmedLine, "[") || strings.HasPrefix(trimmedLine, "`") {
				break
			}
			replyLines = append(replyLines, trimmedLine)
		}
	}
	return SanitizeAIReplyText(strings.TrimSpace(strings.Join(replyLines, "\n")))
}

func parsePseudoMarkdownOutput(raw string) *OctopusAIResponse {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	lower := strings.ToLower(trimmed)
	hasReplyKey := strings.Contains(lower, "ai_reply_text") || strings.Contains(lower, "reply_to_customer")
	hasActionsKey := strings.Contains(lower, "actions:") || strings.Contains(lower, "*actions*:") || strings.Contains(lower, "\"actions\":")
	if !hasReplyKey && !hasActionsKey {
		return nil
	}

	actionsIdx := -1
	for _, delim := range []string{"*actions*:", "**actions**:", "\"actions\":", "actions:"} {
		if idx := strings.Index(lower, delim); idx != -1 {
			if actionsIdx == -1 || idx < actionsIdx {
				actionsIdx = idx
			}
		}
	}

	replyPart := trimmed
	actionsPart := ""
	if actionsIdx != -1 {
		replyPart = strings.TrimSpace(trimmed[:actionsIdx])
		actionsPart = strings.TrimSpace(trimmed[actionsIdx:])
	}

	cleanReply := SanitizeAIReplyText(replyPart)
	if cleanReply == "" {
		cleanReply = tryExtractPseudoAIReply(replyPart)
	}

	var parsedActions []AIActionItem
	if actionsPart != "" {
		startArr := strings.Index(actionsPart, "[")
		endArr := strings.LastIndex(actionsPart, "]")
		if startArr != -1 && endArr != -1 && endArr > startArr {
			_ = json.Unmarshal([]byte(actionsPart[startArr:endArr+1]), &parsedActions)
		}
		if len(parsedActions) == 0 {
			startObj := strings.Index(actionsPart, "{")
			endObj := strings.LastIndex(actionsPart, "}")
			if startObj != -1 && endObj != -1 && endObj > startObj {
				var singleAct AIActionItem
				if err := json.Unmarshal([]byte(actionsPart[startObj:endObj+1]), &singleAct); err == nil && singleAct.Hand != "" {
					parsedActions = append(parsedActions, singleAct)
				}
			}
		}
	}

	if cleanReply == "" && len(parsedActions) == 0 {
		return nil
	}
	if cleanReply == "" {
		cleanReply = SafeAIFallbackReply
	}
	return &OctopusAIResponse{
		AIReplyText: cleanReply,
		Actions:     parsedActions,
		ExecuteHand: "chat_only",
	}
}

// ParseOctopusAIResponse parses the Octopus Agentic JSON response schema.
func ParseOctopusAIResponse(rawResponse string) (*OctopusAIResponse, error) {
	trimmed := strings.TrimSpace(rawResponse)
	if trimmed == "" {
		return &OctopusAIResponse{AIReplyText: SafeAIFallbackReply, Actions: []AIActionItem{}, ExecuteHand: "chat_only"}, fmt.Errorf("empty raw response")
	}

	var state OctopusAIResponse
	parsedStandard := false

	extracted := ExtractJSONSubstring(trimmed)
	if extracted != "" && (strings.Contains(extracted, `"ai_reply_text"`) || strings.Contains(extracted, `"reply_to_customer"`)) {
		if err := json.Unmarshal([]byte(extracted), &state); err == nil {
			parsedStandard = true
		}
	}

	if !parsedStandard {
		if pseudoState := parsePseudoMarkdownOutput(trimmed); pseudoState != nil {
			state = *pseudoState
			parsedStandard = true
		}
	}

	if !parsedStandard {
		return &OctopusAIResponse{AIReplyText: SafeAIFallbackReply, Actions: []AIActionItem{}, ExecuteHand: "chat_only"}, fmt.Errorf("no json found")
	}

	if strings.TrimSpace(state.AIReplyText) == "" && strings.TrimSpace(state.ReplyToCustomer) != "" {
		state.AIReplyText = state.ReplyToCustomer
	}
	state.AIReplyText = SanitizeAIReplyText(state.AIReplyText)
	if strings.TrimSpace(state.AIReplyText) == "" {
		state.AIReplyText = SafeAIFallbackReply
	}

	if len(state.Actions) == 0 {
		hand := strings.ToLower(strings.TrimSpace(state.ExecuteHand))
		if hand == "" {
			switch strings.ToLower(strings.TrimSpace(state.Action)) {
			case "add_to_cart":
				hand = "update_cart"
			case "update_profile":
				hand = "extract_profile"
			case "generate_qr":
				hand = "generate_payment_qr"
			case "check_payment_status":
				hand = "verify_payment"
			case "cancel_order":
				hand = "cancel_order"
			case "escalate_to_human", "human_agent", "support":
				hand = "escalate_to_human"
			default:
				hand = "chat_only"
			}
		}
		state.ExecuteHand = hand
		if state.HandPayload == nil {
			state.HandPayload = &AIActionPayload{}
		}
		if len(state.HandPayload.CartItems) == 0 && len(state.CartItems) > 0 {
			for _, ci := range state.CartItems {
				state.HandPayload.CartItems = append(state.HandPayload.CartItems, AICartItemPayload{ID: ci.ProductID, Qty: FlexInt(ci.Qty)})
			}
		}
		if state.HandPayload.Profile == nil && state.ProfileData != nil {
			state.HandPayload.Profile = &AIProfileDataPayload{
				Name:                   state.ProfileData.Name,
				Address:                state.ProfileData.Address,
				Contact:                state.ProfileData.ContactNumber,
				DeliveryAddress:        state.ProfileData.Address,
				DeliveryContact10Digit: state.ProfileData.ContactNumber,
			}
		}
		if hand != "" && hand != "chat_only" {
			state.Actions = append(state.Actions, AIActionItem{Hand: hand, Payload: state.HandPayload})
		}
	}

	for i := range state.Actions {
		act := &state.Actions[i]
		h := strings.ToLower(strings.TrimSpace(act.Hand))
		if h == "" && act.ExecuteHand != "" {
			h = strings.ToLower(strings.TrimSpace(act.ExecuteHand))
		}
		if h == "" && act.Action != "" {
			h = strings.ToLower(strings.TrimSpace(act.Action))
		}
		switch h {
		case "add_to_cart":
			h = "update_cart"
		case "update_profile":
			h = "extract_profile"
		case "generate_qr":
			h = "generate_payment_qr"
		case "check_payment_status":
			h = "verify_payment"
		case "human", "human_agent", "support":
			h = "escalate_to_human"
		}
		act.Hand = h
		if act.Payload != nil {
			if act.Payload.Category == "" && act.Payload.ProductCategory != "" {
				act.Payload.Category = act.Payload.ProductCategory
			}
			if act.Payload.Profile == nil && act.Payload.ProfileData != nil {
				act.Payload.Profile = act.Payload.ProfileData
			}
			if act.Payload.Profile != nil {
				if act.Payload.Profile.Address == "" && act.Payload.Profile.DeliveryAddress != "" {
					act.Payload.Profile.Address = act.Payload.Profile.DeliveryAddress
				}
				if act.Payload.Profile.Contact == "" && act.Payload.Profile.DeliveryContact10Digit != "" {
					act.Payload.Profile.Contact = act.Payload.Profile.DeliveryContact10Digit
				}
			}
		}
	}

	return &state, nil
}

// GenerateAIFeedbackApology generates a polite contextual apology if a backend hand fails.
func GenerateAIFeedbackApology(username, _, failedAction, errorDetail string, cfg *UserConfigRAM) (string, error) {
	if cfg == nil {
		cfg = GetUserConfigRAM(username)
	}
	selectedApiKey := resolveAPIKey(username, cfg.APIKey)
	if selectedApiKey == "" {
		return "", fmt.Errorf("no API key available")
	}

	lang := cfg.Language
	if lang == "" {
		lang = "Hinglish"
	}
	bizName := cfg.BusinessName
	if bizName == "" {
		bizName = "Our Store"
	}

	hiddenPrompt := fmt.Sprintf(`You are the polite store assistant for "%s".
SYSTEM INTERNAL ERROR: The action [%s] failed because [%s].
Apologize to the user contextually and offer an alternative in polite %s.
Do not mention system errors, code, databases, or technical terms. Return ONLY your apologetic message.`, bizName, failedAction, errorDetail, lang)

	apology, err := CallAIModelWithHistory(cfg.Provider, cfg.Model, 0.7, selectedApiKey, hiddenPrompt, []ChatMessage{{Role: "user", Content: "Please assist with my request."}})
	if err != nil {
		return "", err
	}
	return SanitizeAIReplyText(apology), nil
}

// GenerateAIContextualReply generates a dynamic contextual message strictly from the LLM.
func GenerateAIContextualReply(username, customerPhone, situationContext string, cfg *UserConfigRAM) (string, error) {
	if cfg == nil {
		cfg = GetUserConfigRAM(username)
	}
	selectedApiKey := resolveAPIKey(username, cfg.APIKey)
	if selectedApiKey == "" {
		return "", fmt.Errorf("no API key available")
	}

	bName := cfg.BusinessName
	if bName == "" {
		bName = "Our Store"
	}
	lang := cfg.Language
	if lang == "" {
		lang = "Hinglish"
	}

	systemInstruction := fmt.Sprintf(`You are the polite store assistant for "%s" (%s).
Output language MUST BE strictly: %s.
SITUATION UPDATE: %s
Write a clean, warm WhatsApp message using *bold* for key details and bullet points (• ). Output ONLY the raw message text.`, bName, cfg.BusinessType, lang, situationContext)

	history := getSlidingWindowRAM(username, customerPhone)
	if len(history) == 0 {
		history = []ChatMessage{{Role: "user", Content: "Please provide an update."}}
	}

	reply, err := CallAIModelWithHistory(cfg.Provider, cfg.Model, 0.7, selectedApiKey, systemInstruction, history)
	if err != nil {
		return "", err
	}
	return SanitizeAIReplyText(reply), nil
}

// GenerateOctopusAIReply runs the Master Octopus Agentic Brain strictly from RAM state with multi-key failover.
func GenerateOctopusAIReply(username, customerPhone, incomingText string, cfg *UserConfigRAM) (*OctopusAIResponse, error) {
	cleanPhone := FormatPhone(customerPhone)
	if cfg == nil {
		cfg = GetUserConfigRAM(username)
	}

	if len(collectCandidateAPIKeys(username, cfg)) == 0 {
		return nil, fmt.Errorf("no AI API key configured in vendor AI settings or environment")
	}

	history := getSlidingWindowRAM(username, cleanPhone)
	if len(history) == 0 || history[len(history)-1].Content != incomingText {
		history = updateSlidingWindowRAM(username, cleanPhone, "user", incomingText)
	}

	bName := cfg.BusinessName
	if bName == "" {
		bName = "Vendor Store"
	}
	bType := cfg.BusinessType
	if bType == "" {
		bType = "Retail / Mall Store"
	}
	lang := cfg.Language
	if lang == "" {
		lang = "Hinglish"
	}

	liveInventory := GetLiveInventoryRAG(username)
	inventoryBlock := fmt.Sprintf("Here is the LIVE IN-STOCK inventory you MUST use to answer the customer:\n%s\nNever suggest or add out-of-stock items. When a product has variants (e.g. 500g, 1kg), pass the chosen variant in cart_items[].variant.", liveInventory)

	var deliveryRuleBlock string
	if cfg.RequireCustomerLocation {
		deliveryRuleBlock = fmt.Sprintf("HYPERLOCAL DELIVERY LOCATION RULE (STRICT): Customer must attach their WhatsApp location pin before checkout. Max Radius = %.1f km, Free up to %.1f km, Delivery Charge = ₹%.2f, Estimated Delivery = %s.", cfg.MaxDeliveryRadiusKM, cfg.FreeDeliveryRadiusKM, cfg.DeliveryChargeAmount, cfg.EstimatedDeliveryTime)
	} else {
		deliveryRuleBlock = fmt.Sprintf("HYPERLOCAL DELIVERY RULE: Typed text address accepted. Max Radius = %.1f km, Free up to %.1f km, Delivery Charge = ₹%.2f, Estimated Delivery = %s.", cfg.MaxDeliveryRadiusKM, cfg.FreeDeliveryRadiusKM, cfg.DeliveryChargeAmount, cfg.EstimatedDeliveryTime)
	}
	if cfg.MinOrderAmount > 0 {
		deliveryRuleBlock += fmt.Sprintf(" MINIMUM ORDER AMOUNT: ₹%.2f (politely ask customer to add more items if cart is below minimum).", cfg.MinOrderAmount)
	}
	if cfg.OperatingHours != "" {
		deliveryRuleBlock += fmt.Sprintf(" OPERATING HOURS: %s.", cfg.OperatingHours)
	}

	paymentPolicyBlock := "PAYMENT METHOD POLICY: Cash on Delivery (COD) and Online Payment are available. Online payment link/QR, payment verification, and final order confirmation are 100% handled by Central Brain Node. Trigger submit_order with payment_mode 'COD' or 'ONLINE'."
	if !cfg.AllowCOD {
		paymentPolicyBlock = "PAYMENT METHOD POLICY: Cash on Delivery is NOT available; customer must choose Online Payment. Trigger submit_order with payment_mode 'ONLINE' so Central Brain Node provides the payment link/QR and verifies payment."
	}

	contextStr := BuildOctopusContext(username, cleanPhone, incomingText)
	masterPrompt := fmt.Sprintf(octopusSystemPromptJSON, bName, bType, cfg.StorePolicies, lang, lang)
	if strings.TrimSpace(cfg.SystemPrompt) != "" {
		masterPrompt += "\n\nADDITIONAL VENDOR INSTRUCTIONS:\n" + strings.TrimSpace(cfg.SystemPrompt)
	}
	masterPrompt += "\n\n" + deliveryRuleBlock + "\n" + paymentPolicyBlock + "\n\n" + inventoryBlock + "\n\n" + contextStr

	aiResponse, err := callAIWithKeyFallback(username, cfg, masterPrompt, history)
	if err != nil || aiResponse == "" {
		log.Printf("⚠️ GenerateOctopusAIReply error for +%s: %v", cleanPhone, err)
		return &OctopusAIResponse{AIReplyText: SafeAIFallbackReply, Actions: []AIActionItem{}, ExecuteHand: "chat_only"}, err
	}

	parsed, errParse := ParseOctopusAIResponse(aiResponse)
	if errParse != nil || parsed == nil {
		return &OctopusAIResponse{AIReplyText: SafeAIFallbackReply, Actions: []AIActionItem{}, ExecuteHand: "chat_only"}, nil
	}
	return parsed, nil
}

// GenerateAIReply runs the playground/testing AI call using RAM state.
func GenerateAIReply(username, customerPhone, incomingText, provider, apiKey, systemPrompt string) (*OctopusAIResponse, error) {
	cfg := GetUserConfigRAM(username)
	if strings.TrimSpace(provider) != "" {
		cfg.Provider = provider
	}
	if strings.TrimSpace(apiKey) != "" {
		cfg.APIKey = apiKey
	}
	if strings.TrimSpace(systemPrompt) != "" {
		cfg.SystemPrompt = systemPrompt
	}
	return GenerateOctopusAIReply(username, customerPhone, incomingText, cfg)
}

// AsyncCentralSync saves only the last 10 customer/assistant messages strictly in bot RAM (zero Central Node chat/lead sync).
func AsyncCentralSync(username, customerPhone, incomingText, aiReplyText string) {
	go func() {
		cleanPhone := FormatPhone(customerPhone)
		if cleanPhone == "" {
			return
		}
		if incomingText != "" {
			SaveCustomerChatAndPrune(username, cleanPhone, "customer", incomingText)
		}
		if aiReplyText != "" {
			SaveCustomerChatAndPrune(username, cleanPhone, "assistant", aiReplyText)
		}
	}()
}

// CallAIModelWithHistory calls Groq, Gemini, OpenAI, DeepSeek, or OpenRouter with structured chat history.
func CallAIModelWithHistory(provider, model string, temperature float64, rawApiKey, sysPrompt string, history []ChatMessage) (string, error) {
	apiKey := strings.TrimSpace(rawApiKey)
	if apiKey == "" {
		return "", fmt.Errorf("empty API key")
	}

	prov := strings.ToLower(strings.TrimSpace(provider))
	if prov == "chatgpt" {
		prov = "openai"
	}
	if strings.HasPrefix(apiKey, "gsk_") {
		prov = "groq"
	} else if strings.HasPrefix(apiKey, "sk-") {
		prov = "openai"
	} else if strings.HasPrefix(apiKey, "AIzaSy") {
		prov = "gemini"
	}
	if prov == "" {
		prov = "groq"
	}

	cleanModel := strings.TrimSpace(model)
	if isNumericOrPhone(cleanModel) {
		cleanModel = ""
	}

	if prov == "gemini" {
		res, err := callGeminiAPIWithHistory(apiKey, cleanModel, temperature, sysPrompt, history)
		if err == nil && res != "" {
			return res, nil
		}
		return "", err
	}
	return callOpenAIFormatWithHistory(prov, cleanModel, temperature, apiKey, sysPrompt, history)
}

func isNumericOrPhone(s string) bool {
	if s == "" {
		return false
	}
	clean := regexp.MustCompile(`\D`).ReplaceAllString(s, "")
	return len(clean) >= 8 && (len(clean) == len(s) || strings.HasPrefix(clean, "117") || strings.HasPrefix(clean, "223") || strings.HasPrefix(clean, "91"))
}

func callOpenAIFormatWithHistory(provider, model string, temperature float64, apiKey, sysPrompt string, history []ChatMessage) (string, error) {
	endpoint := "https://api.groq.com/openai/v1/chat/completions"
	var candidateModels []string

	switch provider {
	case "openai":
		endpoint = "https://api.openai.com/v1/chat/completions"
		candidateModels = []string{"gpt-4o-mini", "gpt-4o"}
	case "deepseek":
		endpoint = "https://api.deepseek.com/chat/completions"
		candidateModels = []string{"deepseek-chat"}
	case "openrouter":
		endpoint = "https://openrouter.ai/api/v1/chat/completions"
		candidateModels = []string{"meta-llama/llama-3.1-8b-instruct:free"}
	default:
		endpoint = "https://api.groq.com/openai/v1/chat/completions"
		candidateModels = []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant"}
	}

	if model != "" && !isNumericOrPhone(model) {
		candidateModels = append([]string{model}, candidateModels...)
	}

	messages := []ChatMessage{{Role: "system", Content: sysPrompt}}
	for _, m := range history {
		role := m.Role
		if role == "customer" {
			role = "user"
		} else if role == "outgoing" || role == "outgoing_ai" {
			role = "assistant"
		}
		if strings.TrimSpace(m.Content) != "" {
			messages = append(messages, ChatMessage{Role: role, Content: m.Content})
		}
	}

	var lastErr error
	for _, m := range candidateModels {
		payload := map[string]interface{}{
			"model":       m,
			"messages":    messages,
			"temperature": temperature,
		}
		if provider == "groq" || provider == "openai" || provider == "deepseek" {
			payload["response_format"] = map[string]string{"type": "json_object"}
		}
		body, _ := json.Marshal(payload)

		req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("User-Agent", "VendorWhatsAppBotNode/2.0")

		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var result struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(respBytes, &result); err == nil && len(result.Choices) > 0 && result.Choices[0].Message.Content != "" {
				return strings.TrimSpace(result.Choices[0].Message.Content), nil
			}
		}
		lastErr = fmt.Errorf("model %s returned status %d", m, resp.StatusCode)
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("empty AI reply received")
}

func callGeminiAPIWithHistory(apiKey, model string, temperature float64, sysPrompt string, history []ChatMessage) (string, error) {
	candidateModels := []string{"gemini-2.0-flash", "gemini-1.5-flash"}
	reqModel := strings.TrimSpace(model)
	if reqModel != "" && !isNumericOrPhone(reqModel) && !strings.Contains(reqModel, "gemini-1.0") {
		candidateModels = append([]string{reqModel}, candidateModels...)
	}

	var contents []map[string]interface{}
	for _, m := range history {
		role := "user"
		if m.Role == "assistant" || m.Role == "outgoing" || m.Role == "outgoing_ai" {
			role = "model"
		}
		if strings.TrimSpace(m.Content) != "" {
			contents = append(contents, map[string]interface{}{
				"role":  role,
				"parts": []map[string]string{{"text": m.Content}},
			})
		}
	}
	if len(contents) == 0 {
		return "", fmt.Errorf("empty chat history for Gemini")
	}

	var lastErr error
	for _, mdl := range candidateModels {
		endpoint := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", mdl, apiKey)
		payload := map[string]interface{}{
			"system_instruction": map[string]interface{}{
				"parts": []map[string]string{{"text": sysPrompt}},
			},
			"contents": contents,
			"generationConfig": map[string]interface{}{
				"temperature":        temperature,
				"response_mime_type": "application/json",
			},
		}
		body, _ := json.Marshal(payload)

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Post(endpoint, "application/json", bytes.NewBuffer(body))
		if err != nil {
			lastErr = err
			continue
		}
		respBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var result struct {
				Candidates []struct {
					Content struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"content"`
				} `json:"candidates"`
			}
			if err := json.Unmarshal(respBytes, &result); err == nil && len(result.Candidates) > 0 && len(result.Candidates[0].Content.Parts) > 0 {
				return strings.TrimSpace(result.Candidates[0].Content.Parts[0].Text), nil
			}
		}
		lastErr = fmt.Errorf("gemini status %d for model %s", resp.StatusCode, mdl)
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("empty response from Gemini")
}
