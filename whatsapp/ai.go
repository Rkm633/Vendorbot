package whatsapp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
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
// GROQ-ONLY 3-MODEL POOL & FULL TPD/TPM SCHEDULER ACROSS ALL KEYS × MODELS
// -----------------------------------------------------------------------------

// Strictly active Groq models ONLY (condition applies exclusively to Groq API).
var groqAllowedModels = []string{
	"qwen/qwen3.8-27b",
	"openai/gpt-oss-20b",
	"openai/gpt-oss-120b",
}

// MaxLLMHistoryMessages caps the sliding window sent to the LLM to total 6 recent
// messages (user + assistant combined) to slash input tokens while RAM still keeps
// ramstore.MaxChatsPerCustomer (10) messages for the Live Chat dashboard.
const MaxLLMHistoryMessages = 6

type groqSlotState struct {
	tpmCooldownUntil  time.Time
	tpdCooldownUntil  time.Time
	authDisabledUntil time.Time
	tokensUsedToday   int64
	requestsToday     int64
	dayUTC            string
}

type groqCandidateSlot struct {
	keyIndex  int
	apiKey    string
	keyHash   string
	model     string
	tokensDay int64
	tpmUntil  time.Time
}

type groqPoolManager struct {
	mu     sync.Mutex
	slots  map[string]*groqSlotState
	cursor uint64
}

var groqPool = &groqPoolManager{
	slots: make(map[string]*groqSlotState),
}

func shortKeyHash(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:6])
}

func isGroqModelAllowed(m string) bool {
	m = strings.ToLower(strings.TrimSpace(m))
	for _, allowed := range groqAllowedModels {
		if m == allowed {
			return true
		}
	}
	return false
}

// isGroqProvider strictly detects whether a provider/key pair targets Groq API.
func isGroqProvider(provider, apiKey string) bool {
	k := strings.TrimSpace(apiKey)
	if strings.HasPrefix(k, "gsk_") {
		return true
	}
	if strings.HasPrefix(k, "AIza") || strings.HasPrefix(k, "sk-or-") || strings.HasPrefix(k, "sk-") {
		return false
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	return p == "" || p == "groq"
}

func (pm *groqPoolManager) getSlotLocked(kHash, model string, now time.Time) *groqSlotState {
	day := now.UTC().Format("2006-01-02")
	id := kHash + "|" + model
	st, ok := pm.slots[id]
	if !ok || st == nil {
		st = &groqSlotState{dayUTC: day}
		pm.slots[id] = st
		return st
	}
	// Reset daily TPD usage & TPD cooldown cleanly at UTC midnight
	if st.dayUTC != day {
		st.dayUTC = day
		st.tokensUsedToday = 0
		st.requestsToday = 0
		st.tpdCooldownUntil = time.Time{}
	}
	return st
}

// buildCandidates builds the full (active Groq keys × 3 Groq models) matrix split into:
//   - ready: immediately usable slots balanced across all keys & all 3 models for full TPD utilization
//   - tpmDelayed: non-TPD slots temporarily on short TPM cooldown (sorted by soonest expiry)
func (pm *groqPoolManager) buildCandidates(groqKeys []string) (ready []groqCandidateSlot, tpmDelayed []groqCandidateSlot) {
	now := time.Now()
	pm.mu.Lock()
	defer pm.mu.Unlock()

	total := len(groqKeys) * len(groqAllowedModels)
	if total == 0 {
		return nil, nil
	}

	// Build interleaved matrix: iterate models across all keys so every key × every model gets TPD coverage
	matrix := make([]groqCandidateSlot, 0, total)
	for mIdx, model := range groqAllowedModels {
		_ = mIdx
		for kIdx, key := range groqKeys {
			matrix = append(matrix, groqCandidateSlot{
				keyIndex: kIdx,
				apiKey:   key,
				keyHash:  shortKeyHash(key),
				model:    model,
			})
		}
	}

	// Round-robin offset across the K × 3 matrix so consecutive requests spread across all models & keys
	start := int(pm.cursor % uint64(total))
	pm.cursor++

	rotated := make([]groqCandidateSlot, 0, total)
	rotated = append(rotated, matrix[start:]...)
	rotated = append(rotated, matrix[:start]...)

	for _, slot := range rotated {
		st := pm.getSlotLocked(slot.keyHash, slot.model, now)
		if now.Before(st.authDisabledUntil) {
			continue
		}
		if now.Before(st.tpdCooldownUntil) {
			continue
		}
		slot.tokensDay = st.tokensUsedToday
		slot.tpmUntil = st.tpmCooldownUntil
		if now.Before(st.tpmCooldownUntil) {
			tpmDelayed = append(tpmDelayed, slot)
		} else {
			ready = append(ready, slot)
		}
	}

	// Sort ready slots gently by lowest daily token usage (keeping round-robin tie-break)
	// so every model on every active key drains its full TPD evenly!
	sort.SliceStable(ready, func(i, j int) bool {
		// Bucket by 5k tokens so round-robin rotation still spreads burst TPM cleanly
		return (ready[i].tokensDay / 5000) < (ready[j].tokensDay / 5000)
	})

	sort.SliceStable(tpmDelayed, func(i, j int) bool {
		return tpmDelayed[i].tpmUntil.Before(tpmDelayed[j].tpmUntil)
	})

	return ready, tpmDelayed
}

func (pm *groqPoolManager) recordSuccess(kHash, model string, totalTokens int64, remainingTokens int, resetTokensDur time.Duration) {
	now := time.Now()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	st := pm.getSlotLocked(kHash, model, now)
	st.tokensUsedToday += totalTokens
	st.requestsToday++
	st.tpmCooldownUntil = time.Time{}
	// Proactively pace if Groq header reports near-zero remaining TPM tokens (< 800)
	if remainingTokens >= 0 && remainingTokens < 800 {
		wait := resetTokensDur
		if wait <= 0 || wait > 75*time.Second {
			wait = 15 * time.Second
		}
		st.tpmCooldownUntil = now.Add(wait)
	}
}

func (pm *groqPoolManager) markTPMCooldown(kHash, model string, dur time.Duration) {
	if dur <= 0 {
		dur = 65 * time.Second
	}
	now := time.Now()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	st := pm.getSlotLocked(kHash, model, now)
	st.tpmCooldownUntil = now.Add(dur)
}

func (pm *groqPoolManager) markTPDExhausted(kHash, model string, dur time.Duration) {
	now := time.Now()
	nextMidnightUTC := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 1, 0, 0, time.UTC)
	until := nextMidnightUTC
	if dur > 0 {
		cand := now.Add(dur)
		if cand.After(now.Add(10*time.Minute)) && cand.Before(until) {
			until = cand
		}
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	st := pm.getSlotLocked(kHash, model, now)
	st.tpdCooldownUntil = until
}

func (pm *groqPoolManager) markAuthDisabledForKey(kHash string, dur time.Duration) {
	if dur <= 0 {
		dur = 30 * time.Minute
	}
	now := time.Now()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, mdl := range groqAllowedModels {
		st := pm.getSlotLocked(kHash, mdl, now)
		st.authDisabledUntil = now.Add(dur)
	}
}

// parseResetDuration parses Groq headers like "12", "45.2s", "1m20s", "2h15m3s", or "500ms".
func parseResetDuration(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return 0
}

// classifyGroq429 distinguishes daily TPD/RPD exhaustion vs short-term TPM/RPM throttling.
func classifyGroq429(headers http.Header, body []byte) (isTPD bool, cooldown time.Duration) {
	lower := strings.ToLower(string(body))
	retryAfter := parseResetDuration(headers.Get("Retry-After"))
	resetTok := parseResetDuration(headers.Get("x-ratelimit-reset-tokens"))
	resetReq := parseResetDuration(headers.Get("x-ratelimit-reset-requests"))

	// Check explicit daily quota markers in Groq error payload
	if strings.Contains(lower, "tokens per day") ||
		strings.Contains(lower, "requests per day") ||
		strings.Contains(lower, "(tpd)") ||
		strings.Contains(lower, " tpd") ||
		strings.Contains(lower, "(rpd)") ||
		strings.Contains(lower, " rpd") ||
		strings.Contains(lower, "daily") ||
		retryAfter >= 5*time.Minute {
		cd := retryAfter
		if cd < 30*time.Minute {
			cd = 2 * time.Hour
		}
		return true, cd
	}

	// Short-term TPM / RPM cooldown
	cd := retryAfter
	if cd <= 0 {
		if resetTok > 0 && resetTok < 5*time.Minute {
			cd = resetTok
		} else if resetReq > 0 && resetReq < 5*time.Minute {
			cd = resetReq
		}
	}
	if cd <= 0 {
		cd = 65 * time.Second
	} else {
		cd += 2 * time.Second
	}
	return false, cd
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
	Query           string                `json:"query,omitempty"`
	Queries         []string              `json:"queries,omitempty"`
	Categories      []string              `json:"categories,omitempty"`
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
	QuoteReply  bool           `json:"quote_reply,omitempty"`
	Actions     []AIActionItem `json:"actions,omitempty"`

	ExecuteHand     string           `json:"execute_hand,omitempty"`
	HandPayload     *AIActionPayload `json:"hand_payload,omitempty"`
	Action          string           `json:"action,omitempty"`
	ReplyToCustomer string           `json:"reply_to_customer,omitempty"`
	CartItems       []AICartItem     `json:"cart_items,omitempty"`
	ProfileData     *AIProfileData   `json:"profile_data,omitempty"`
}

const SafeAIFallbackReply = "Namaste ji! 🙏✨\nMaaf kijiye, main aapka message theek se samajh nahi paya 😅 Kripya apna *item ka naam* ya *sawal* ek baar aur batayein — main turant help karta hoon! 😊🛍️"

var (
	jsonExtractorRegex = regexp.MustCompile(`(?s)\{.*\}`)
	thinkTagRegex      = regexp.MustCompile(`(?is)<think>.*?</think>`)
	whitespaceCollapse = regexp.MustCompile(`\n{3,}`)
	nonDigitRegex      = regexp.MustCompile(`\D`)
	keySplitRegex      = regexp.MustCompile(`[\n,\s]+`)
)

// Ultra-lean, token-optimized system prompt (~60% smaller than legacy prompt) with
// warm humanoid tone, rich WhatsApp *bold* + emoji guidance, and smart conditional quote_reply.
const octopusSystemPromptJSON = `You are the warm, lively, human-like WhatsApp shop assistant for "%s" (%s).
Policy: %s
Reply strictly in: %s.

OUTPUT ONLY RAW JSON (no markdown fences, no text outside {}):
{"ai_reply_text":"...","quote_reply":false,"actions":[{"hand":"...","payload":{}}]}

TONE & WHATSAPP STYLE (HUMANOID + ENGAGING):
• Sound like a friendly, caring real shopkeeper — natural, warm, enthusiastic, never robotic or stiff.
• Use *bold* generously for *item names*, *₹prices*, *totals*, *variants*, and key call-to-actions.
• Use rich, expressive emojis naturally across greetings, bullets, and sign-offs (✨🛍️🔥😋💰📦🙌💛).
• Keep replies crisp, scannable, and mobile-friendly with clean "• " bullets and _short italics_ where nice.
• Use WhatsApp blockquote (> ) ONLY sparingly for a highlight box (like > 🧾 *Total: ₹...* or > 💳 *Payment Note*), NEVER on ordinary lines.
• Set "quote_reply":true ONLY when directly answering a specific question/price/item/address/complaint so WhatsApp swipe-quote adds clarity; keep "quote_reply":false for normal greetings, simple replies, and smooth chat flow.

HANDS (include only needed actions; empty [] for pure chat):
• search_catalog: {"queries":["keyword1","keyword2"],"categories":["CategoryName"]} — ONLY when customer describes an indirect need/use-case or requests items NOT visible in INV below (uses CATS to fetch exact in-stock items in 1 lookup).
• send_catalog_images: {"category":"item name|category|all"} — when user asks for photos/pics.
• update_cart: {"cart_items":[{"id":"EXACT_ID","variant":"exact variant if any","qty":1}]} — ONLY from IN-STOCK INV/CLEAR_MATCH below. For multi-item lists, add all CLEAR_MATCH items immediately and politely ask customer to pick brand/size for VARIETY_OPTIONS.
• extract_profile: {"profile":{"name":"","address":"","contact":""}} — save customer name/address/10-digit phone.
• submit_order: {"payment_mode":"COD"|"ONLINE"} — when cart has items + address is known + customer chooses COD or Online.
• generate_store_link: {} — when user asks for website/store/mall link.
• clear_cart: {} | cancel_order: {} | escalate_to_human: {} (notify owner & keep helping warmly).

RULES:
1. NEVER suggest/add out-of-stock items. Only quote items/prices/IDs present in INV/CLEAR_MATCH/VARIETY_OPTIONS/SEARCH_RESULTS. If customer describes a need (e.g. "snacks for guests", "cough relief") not shown in INV, call search_catalog hand with relevant product words & categories from CATS.
2. NEVER ask for UTR or payment screenshots, NEVER verify screenshots, NEVER mark orders Paid. If user says they paid, warmly reassure that our Central Payment System verifies online payments automatically and sends confirmation + PDF receipt right away.
3. On greeting/menu ask, warmly welcome & highlight top categories/popular items with *₹prices* and offer photos!
4. Before submit_order, show a crisp *Order Summary* (items, total, address) and ask *Online Payment* 💳 or *COD* 💵 (if COD allowed).
5. On a customer's FIRST message (new chat), warmly ask them to write their full message together in one message instead of sending small pieces.`

// BuildUniversalSystemPrompt builds the Octopus system prompt dynamically from RAM vendor settings.
func BuildUniversalSystemPrompt(businessName, businessType, language, storePolicies, catalogContext string) string {
	bName := strings.TrimSpace(businessName)
	if bName == "" {
		bName = "Vendor Store"
	}
	bType := strings.TrimSpace(businessType)
	if bType == "" {
		bType = "Retail Store"
	}
	lang := strings.TrimSpace(language)
	if lang == "" {
		lang = "Hinglish"
	}
	policies := compactText(storePolicies, 180)
	if policies == "" {
		policies = "Standard delivery & returns."
	}

	finalPrompt := fmt.Sprintf(octopusSystemPromptJSON, bName, bType, policies, lang)
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

	provider := strings.TrimSpace(a.Provider)
	if provider == "" {
		provider = "groq"
	}
	model := strings.TrimSpace(a.Model)
	if strings.EqualFold(provider, "groq") {
		if !isGroqModelAllowed(model) {
			model = groqAllowedModels[0]
		}
	} else if strings.EqualFold(provider, "gemini") || strings.EqualFold(provider, "google") || strings.EqualFold(provider, "gemma") {
		if !isGeminiModelAllowed(model) {
			model = geminiAllowedModels[0]
		}
	} else if model == "" {
		model = groqAllowedModels[0]
	}

	temp := a.Temperature
	if temp <= 0 {
		temp = 0.72
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
		CatalogText:             "", // Built lazily & compactly per-turn by GetSmartCompactInventory
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

// collectCandidateAPIKeys builds a deduplicated pool across vendor primary + backup keys + env fallbacks.
func collectCandidateAPIKeys(username string, cfg *UserConfigRAM) []string {
	seen := make(map[string]bool)
	var keys []string
	addTokens := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		for _, t := range keySplitRegex.Split(raw, -1) {
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

	// Preserve deterministic order for Groq & Gemini pools (they manage their own key × model rotation matrices);
	// for other providers, rotate start index per request.
	if cfg != nil {
		p := strings.ToLower(strings.TrimSpace(cfg.Provider))
		if p == "" || p == "groq" || p == "gemini" || p == "google" || p == "gemma" {
			return keys
		}
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

// callAIWithKeyFallback implements the 3-mode AI scheduling engine:
//  1. Dual Mode (Groq + Gemini both configured): Alternates ("bari-bari") between Groq (3 models)
//     and Gemini (4 models) across all active keys with instant cross-provider failover on TPM/RPM/TPD/429.
//  2. Groq-Only Mode: Preserves strict 3-model Groq TPD/TPM pool (qwen3.8-27b, gpt-oss-20b, gpt-oss-120b).
//  3. Gemini-Only Mode: Strictly uses 4 models (gemma-4-31b-it, gemma-4-26b-a4b-it, gemini-3.1-flash-lite, gemini-3.5-flash-lite).
func callAIWithKeyFallback(username string, cfg *UserConfigRAM, sysPrompt string, history []ChatMessage, jsonMode bool) (string, error) {
	groqKeys, geminiKeys, otherKeys := ClassifyConfiguredAIKeys(username, cfg)
	if len(groqKeys) == 0 && len(geminiKeys) == 0 && len(otherKeys) == 0 {
		return "", fmt.Errorf("no AI API key configured in vendor AI settings or environment")
	}

	var lastErr error

	// Case 1: Dual Mode (BOTH Groq & Gemini keys configured -> alternating rotation across all keys × models)
	if len(groqKeys) > 0 && len(geminiKeys) > 0 {
		res, err := callDualGroqGeminiPoolWithHistory(groqKeys, geminiKeys, cfg.Temperature, sysPrompt, history, jsonMode)
		if err == nil && strings.TrimSpace(res) != "" {
			return res, nil
		}
		lastErr = err
		if len(otherKeys) > 0 {
			log.Printf("🔄 [AI Fallback] All Groq + Gemini slots unavailable (%v) — falling back to backup key...", err)
		}
	} else if len(groqKeys) > 0 {
		// Case 2: Groq-Only Mode (exact existing 3-model Groq TPD/TPM architecture)
		res, err := callGroqPoolWithHistory(groqKeys, cfg.Temperature, sysPrompt, history, jsonMode)
		if err == nil && strings.TrimSpace(res) != "" {
			return res, nil
		}
		lastErr = err
		if len(otherKeys) > 0 {
			log.Printf("🔄 [AI Fallback] All Groq slots unavailable (%v) — falling back to backup key...", err)
		}
	} else if len(geminiKeys) > 0 {
		// Case 3: Gemini-Only Mode (strictly 4 allowed Gemini/Gemma models across all active Gemini keys)
		res, err := callGeminiPoolWithHistory(geminiKeys, cfg.Temperature, sysPrompt, history, jsonMode)
		if err == nil && strings.TrimSpace(res) != "" {
			return res, nil
		}
		lastErr = err
		if len(otherKeys) > 0 {
			log.Printf("🔄 [AI Fallback] All Gemini slots unavailable (%v) — falling back to backup key...", err)
		}
	}

	// Backup Non-Groq/Non-Gemini Providers (OpenAI / DeepSeek / OpenRouter)
	for idx, key := range otherKeys {
		res, err := callNonGroqModelWithHistory(cfg.Provider, cfg.Model, cfg.Temperature, key, sysPrompt, history, jsonMode)
		if err == nil && strings.TrimSpace(res) != "" {
			return res, nil
		}
		lastErr = err
		if idx < len(otherKeys)-1 {
			log.Printf("🔄 [AI Multi-Key Fallback] Key #%d failed (%v) — switching automatically to backup key #%d...", idx+1, err, idx+2)
		}
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("all AI keys failed to return a reply")
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
	if s == "" {
		return ""
	}
	s = whitespaceCollapse.ReplaceAllString(s, "\n")
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}

func formatRupeeCompact(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
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
			limit := 260
			if rec.Role == "assistant" || rec.Direction == "outgoing_ai" || rec.Direction == "outgoing_manual" {
				r = "assistant"
				limit = 220
			}
			c := compactText(rec.Text, limit)
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
		limit := 400
		if normalRole == "customer" || normalRole == "incoming" {
			normalRole = "user"
		} else if normalRole == "outgoing" || normalRole == "outgoing_ai" || normalRole == "assistant" {
			normalRole = "assistant"
			limit = 240
		}
		cleanText := compactText(text, limit)
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

// buildCompactLLMHistory slices the sliding window to strictly MaxLLMHistoryMessages total
// messages and compacts older bot replies so prompt history stays ultra-light on tokens.
func buildCompactLLMHistory(full []ChatMessage, maxTotal int) []ChatMessage {
	if maxTotal <= 0 {
		maxTotal = MaxLLMHistoryMessages
	}
	start := 0
	if len(full) > maxTotal {
		start = len(full) - maxTotal
	}
	sliced := full[start:]
	out := make([]ChatMessage, 0, len(sliced))
	for idx, m := range sliced {
		role := m.Role
		if role == "customer" || role == "incoming" {
			role = "user"
		} else if role == "outgoing" || role == "outgoing_ai" {
			role = "assistant"
		}
		isLatest := idx == len(sliced)-1
		maxLen := 200
		if role == "assistant" {
			// Older bot replies contain formatted lists/emojis — compact aggressively!
			maxLen = 160
		} else if isLatest {
			// Keep the customer's current turn full (up to 500 runes) so zero detail is lost
			maxLen = 500
		}
		txt := compactText(strings.ReplaceAll(m.Content, "\n\n", "\n"), maxLen)
		if txt != "" {
			out = append(out, ChatMessage{Role: role, Content: txt})
		}
	}
	return out
}

// GetCatalogContext formats all in-stock RAM products grouped by category (compact).
func GetCatalogContext(_ string) string {
	products := ramstore.Global.ListProducts(true)
	if len(products) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("INV (ID|Name|Cat|Price|Vars):\n")
	for _, p := range products {
		sb.WriteString(formatCompactProductLine(p) + "\n")
	}
	return strings.TrimSpace(sb.String())
}

// Clean10DigitPhone extracts strictly a 10-digit Indian phone number.
func Clean10DigitPhone(raw string) string {
	digits := nonDigitRegex.ReplaceAllString(raw, "")
	if len(digits) >= 10 {
		return digits[len(digits)-10:]
	}
	return digits
}

// formatCompactProductLine serializes one product in ~12-18 tokens instead of ~45-55 tokens.
func formatCompactProductLine(p ramstore.Product) string {
	cat := strings.TrimSpace(p.Category)
	if cat == "" {
		cat = "General"
	}
	unitPart := ""
	if u := strings.TrimSpace(p.UnitType); u != "" && u != "custom" && u != "piece" {
		unitPart = "/" + u
	}
	var sb strings.Builder
	sb.WriteString(p.ID)
	sb.WriteString("|")
	sb.WriteString(strings.TrimSpace(p.Name))
	sb.WriteString("|")
	sb.WriteString(cat)
	sb.WriteString("|₹")
	sb.WriteString(formatRupeeCompact(p.EffectivePrice()))
	sb.WriteString(unitPart)

	if len(p.Variants) > 0 {
		var vParts []string
		for _, vr := range p.Variants {
			if !vr.IsVariantAvailable(1) {
				continue
			}
			vParts = append(vParts, vr.DisplayLabel()+"="+formatRupeeCompact(vr.EffectivePrice(p.EffectivePrice())))
		}
		if len(vParts) > 0 {
			sb.WriteString("|v:")
			sb.WriteString(strings.Join(vParts, ","))
		}
	}
	if strings.TrimSpace(p.ThumbURL) != "" || strings.TrimSpace(p.ImageURL) != "" || len(p.Images) > 0 {
		sb.WriteString("|img")
	}
	return sb.String()
}

var ragStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "hai": true, "kya": true,
	"karo": true, "chahiye": true, "dena": true, "please": true, "mera": true,
	"meri": true, "mere": true, "bhai": true, "aap": true, "tum": true, "order": true,
	"mujhe": true, "dijiye": true, "bhejo": true, "hoga": true, "photos": true,
	"photo": true, "pic": true, "image": true, "dikhao": true, "price": true,
	"rate": true, "kitna": true, "kitne": true, "menu": true, "list": true,
	"available": true, "item": true, "items": true, "product": true, "products": true,
	"haan": true, "han": true, "yes": true, "nahi": true, "hello": true, "namaste": true,
	"confirm": true, "address": true, "delivery": true, "online": true, "payment": true,
}

// GetSmartCompactInventory delegates to the 4-Layer Static Hybrid Catalog Search Engine
// (Hinglish synonyms + phonetic/fuzzy matching + per-line multi-item splitting + checkout skip).
func GetSmartCompactInventory(userMsg string, history []ChatMessage, cartProductIDs map[string]bool) string {
	return BuildSmartCatalogContext(userMsg, history, cartProductIDs)
}

// GetLiveInventoryRAG kept for backwards compatibility, returning the compact inventory format.
func GetLiveInventoryRAG(_ string) string {
	return GetSmartCompactInventory("", nil, nil)
}

// GetRAGCatalogResults kept for backwards compatibility, returning compact keyword-matched items.
func GetRAGCatalogResults(_, userMsg string) string {
	return GetSmartCompactInventory(userMsg, nil, nil)
}

// BuildOctopusContext builds a compact real-time RAM state block (zero wasted boilerplate tokens).
func BuildOctopusContext(username, customerPhone, _ string) string {
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

	var cartParts []string
	var cartTotal float64
	if calcRes != nil {
		cartTotal = calcRes.TotalAmount
		for _, it := range calcRes.Items {
			vPart := ""
			if it.SelectedVariant != "" {
				vPart = "[" + it.SelectedVariant + "]"
			}
			cartParts = append(cartParts, fmt.Sprintf("%s %s%s x%d=₹%s",
				it.ProductID, it.ProductName, vPart, it.Qty, formatRupeeCompact(it.LineTotal)))
		}
	}

	cartLine := "CART:empty"
	if len(cartParts) > 0 {
		cartLine = fmt.Sprintf("CART(₹%s): %s", formatRupeeCompact(cartTotal), strings.Join(cartParts, "; "))
	}

	custProfile, _ := GetCustomerProfile(username, cleanPhone)
	var profParts []string
	if custProfile != nil {
		if strings.TrimSpace(custProfile.FullName) != "" {
			profParts = append(profParts, "Name="+strings.TrimSpace(custProfile.FullName))
		}
		if strings.TrimSpace(custProfile.DeliveryAddress) != "" {
			profParts = append(profParts, "Addr="+compactText(custProfile.DeliveryAddress, 120))
		}
		if strings.TrimSpace(custProfile.ContactPhone) != "" {
			profParts = append(profParts, "Ph="+strings.TrimSpace(custProfile.ContactPhone))
		}
	}
	profParts = append(profParts, "WA="+cleanPhone)

	var sb strings.Builder
	sb.WriteString("STATE:" + currentState + " | " + cartLine)
	if ordID := strings.TrimSpace(GetCustomerCurrentOrderID(username, cleanPhone)); ordID != "" {
		sb.WriteString(" | ORD:" + ordID)
	}
	sb.WriteString("\nCUST:" + strings.Join(profParts, " | "))
	return sb.String()
}

func stripThinkBlocks(raw string) string {
	return strings.TrimSpace(thinkTagRegex.ReplaceAllString(raw, ""))
}

func ExtractJSONSubstring(raw string) string {
	cleaned := stripThinkBlocks(raw)
	return strings.TrimSpace(jsonExtractorRegex.FindString(cleaned))
}

func SanitizeAIReplyText(s string) string {
	res := stripThinkBlocks(s)
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
			// Keep WhatsApp text blockquote ONLY on highlight callouts (totals, payment, urgent alerts)
			if strings.HasPrefix(after, "💳") || strings.HasPrefix(after, "🚨") ||
				strings.HasPrefix(after, "🧾") || strings.HasPrefix(after, "📦") ||
				strings.HasPrefix(after, "🛒") || strings.HasPrefix(after, "💰") ||
				strings.HasPrefix(after, "🎉") || strings.HasPrefix(after, "✨") {
				cleanedLines = append(cleanedLines, trimL)
			} else {
				cleanedLines = append(cleanedLines, after)
			}
		} else {
			cleanedLines = append(cleanedLines, l)
		}
	}
	res = strings.Join(cleanedLines, "\n")
	// Preserve valid WhatsApp *bold* markers at start/end (do NOT trim '*')
	res = strings.Trim(res, " ,\"'}{][\n\r`")
	if res == "" || strings.HasPrefix(res, "{") || strings.HasPrefix(res, "[") {
		return ""
	}
	return res
}

func tryExtractPseudoAIReply(raw string) string {
	lines := strings.Split(stripThinkBlocks(raw), "\n")
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
				val := strings.Trim(strings.TrimSpace(trimmedLine[idx+1:]), "\" ")
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
	trimmed := stripThinkBlocks(raw)
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
	trimmed := stripThinkBlocks(rawResponse)
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
		case "search_catalog", "find_products", "browse_category", "lookup_catalog", "search_products":
			h = "search_catalog"
		}
		act.Hand = h
		if act.Payload != nil {
			if act.Payload.Category == "" && act.Payload.ProductCategory != "" {
				act.Payload.Category = act.Payload.ProductCategory
			}
			if act.Payload.Query != "" {
				act.Payload.Queries = appendUniqueStrings(act.Payload.Queries, act.Payload.Query)
			}
			if act.Hand == "search_catalog" && act.Payload.Category != "" {
				act.Payload.Categories = appendUniqueStrings(act.Payload.Categories, act.Payload.Category)
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
	lang := cfg.Language
	if lang == "" {
		lang = "Hinglish"
	}
	bizName := cfg.BusinessName
	if bizName == "" {
		bizName = "Our Store"
	}

	hiddenPrompt := fmt.Sprintf(`Warm shop assistant for "%s". Action [%s] failed (%s). Apologize warmly in %s with helpful next step, *bold* key words & friendly emojis. Never mention tech errors. Output ONLY the message text.`, bizName, failedAction, compactText(errorDetail, 120), lang)

	apology, err := callAIWithKeyFallback(username, cfg, hiddenPrompt, []ChatMessage{{Role: "user", Content: "Please help."}}, false)
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
	bName := cfg.BusinessName
	if bName == "" {
		bName = "Our Store"
	}
	lang := cfg.Language
	if lang == "" {
		lang = "Hinglish"
	}

	systemInstruction := fmt.Sprintf(`Warm shop assistant for "%s" (%s). Language: %s. Update: %s. Write a short, engaging WhatsApp message using *bold* & friendly emojis. Output ONLY the message text.`, bName, cfg.BusinessType, lang, compactText(situationContext, 240))

	history := buildCompactLLMHistory(getSlidingWindowRAM(username, customerPhone), 4)
	if len(history) == 0 {
		history = []ChatMessage{{Role: "user", Content: "Please update me."}}
	}

	reply, err := callAIWithKeyFallback(username, cfg, systemInstruction, history, false)
	if err != nil {
		return "", err
	}
	return SanitizeAIReplyText(reply), nil
}

// GenerateOctopusAIReply runs the Master Octopus Agentic Brain strictly from RAM state
// with token-optimized single-pass smart RAG & Groq 3-model × multi-key TPD failover.
func GenerateOctopusAIReply(username, customerPhone, incomingText string, cfg *UserConfigRAM) (*OctopusAIResponse, error) {
	cleanPhone := FormatPhone(customerPhone)
	if cfg == nil {
		cfg = GetUserConfigRAM(username)
	}

	if len(collectCandidateAPIKeys(username, cfg)) == 0 {
		return nil, fmt.Errorf("no AI API key configured in vendor AI settings or environment")
	}

	cleanQueryForRouting := stripVoiceNoteMarkers(incomingText)
	fullHistory := getSlidingWindowRAM(username, cleanPhone)
	if len(fullHistory) == 0 || stripVoiceNoteMarkers(fullHistory[len(fullHistory)-1].Content) != compactText(cleanQueryForRouting, 400) {
		fullHistory = updateSlidingWindowRAM(username, cleanPhone, "user", cleanQueryForRouting)
	}

	// Strictly limit LLM sliding window to MaxLLMHistoryMessages (6 total messages)
	llmHistory := buildCompactLLMHistory(fullHistory, MaxLLMHistoryMessages)

	bName := strings.TrimSpace(cfg.BusinessName)
	if bName == "" {
		bName = "Vendor Store"
	}
	bType := strings.TrimSpace(cfg.BusinessType)
	if bType == "" {
		bType = "Retail Store"
	}
	lang := strings.TrimSpace(cfg.Language)
	if lang == "" {
		lang = "Hinglish"
	}
	policies := compactText(cfg.StorePolicies, 180)
	if policies == "" {
		policies = "Standard delivery & returns."
	}

	// Collect active cart product IDs so smart inventory ALWAYS keeps cart items in context
	cartProductIDs := make(map[string]bool)
	if calcRes, _ := CalculateCartTotalAndFetchCredentials(username, cleanPhone); calcRes != nil {
		for _, it := range calcRes.Items {
			if it.ProductID != "" {
				cartProductIDs[it.ProductID] = true
			}
		}
	}

	// Compact store delivery & payment rule line (~35 tokens instead of ~150 tokens)
	locMode := "Text addr OK"
	if cfg.RequireCustomerLocation {
		locMode = "WhatsApp location pin required"
	}
	payMode := "COD+ONLINE"
	if !cfg.AllowCOD {
		payMode = "ONLINE ONLY (no COD)"
	}
	var ruleParts []string
	ruleParts = append(ruleParts, locMode)
	if cfg.MaxDeliveryRadiusKM > 0 {
		ruleParts = append(ruleParts, fmt.Sprintf("Max%.0fkm/Free%.0fkm/Fee₹%s", cfg.MaxDeliveryRadiusKM, cfg.FreeDeliveryRadiusKM, formatRupeeCompact(cfg.DeliveryChargeAmount)))
	}
	if cfg.MinOrderAmount > 0 {
		ruleParts = append(ruleParts, "MinOrd₹"+formatRupeeCompact(cfg.MinOrderAmount))
	}
	if strings.TrimSpace(cfg.EstimatedDeliveryTime) != "" {
		ruleParts = append(ruleParts, "ETA:"+strings.TrimSpace(cfg.EstimatedDeliveryTime))
	}
	if strings.TrimSpace(cfg.OperatingHours) != "" {
		ruleParts = append(ruleParts, "Hrs:"+strings.TrimSpace(cfg.OperatingHours))
	}
	ruleParts = append(ruleParts, "Pay:"+payMode)

	// Optional Gemini Decision 1 Smart Router (active ONLY when Gemini key(s) are configured — 0 output tokens!)
	// Saves 50-70% tokens by skipping catalog on non-shopping turns and pre-routing vague Hinglish categories.
	_, geminiKeys, _ := ClassifyConfiguredAIKeys(username, cfg)
	var routeDec DecisionRouteResult
	if len(geminiKeys) > 0 {
		routeDec = RunGeminiDecisionSmartRoute(geminiKeys, cleanQueryForRouting, len(cartProductIDs) > 0)
	}

	// Static prefix first (prompt-cache friendly!), dynamic per-customer state + smart RAG at the end!
	var promptBuilder strings.Builder
	promptBuilder.WriteString(fmt.Sprintf(octopusSystemPromptJSON, bName, bType, policies, lang))
	if extra := compactText(cfg.SystemPrompt, 280); extra != "" {
		promptBuilder.WriteString("\nVENDOR NOTE: " + extra)
	}
	promptBuilder.WriteString("\nSTORE: " + strings.Join(ruleParts, " | "))
	promptBuilder.WriteString("\n\n" + BuildOctopusContext(username, cleanPhone, cleanQueryForRouting))

	if routeDec.UsedDecisionAPI && routeDec.NeedsEscalation {
		promptBuilder.WriteString("\nROUTER_HINT: Customer needs human/owner help — include escalate_to_human action and reply with empathy.")
	}

	if routeDec.UsedDecisionAPI && routeDec.SkipCatalog {
		// Huge token savings: skip full catalog on non-shopping turns (address, payment status, complaint, chitchat)
		promptBuilder.WriteString("\n\nINV: (Catalog omitted by smart router — non-shopping turn. Use search_catalog only if user asks for a new product.)")
	} else {
		invBlock := GetSmartCompactInventory(cleanQueryForRouting, llmHistory, cartProductIDs)
		promptBuilder.WriteString("\n\n" + invBlock)
		if routeDec.UsedDecisionAPI && routeDec.MatchedCategory != "" &&
			!strings.Contains(invBlock, "MULTI_ITEM_MATCH") &&
			!strings.Contains(invBlock, "checkout mode") &&
			(strings.Contains(invBlock, "HINT: 0 direct keyword hits") || !strings.Contains(strings.ToLower(invBlock), "|"+strings.ToLower(routeDec.MatchedCategory)+"|")) {
			// Pre-inject exact matched category from RAM on Pass 1 -> saves an entire 2nd LLM round-trip!
			catBlock := ExecuteSearchCatalogHand(nil, []string{routeDec.MatchedCategory})
			promptBuilder.WriteString("\n\n" + catBlock)
		}
	}

	aiResponse, err := callAIWithKeyFallback(username, cfg, promptBuilder.String(), llmHistory, true)
	if err != nil || aiResponse == "" {
		log.Printf("⚠️ GenerateOctopusAIReply error for +%s: %v", cleanPhone, err)
		return &OctopusAIResponse{AIReplyText: SafeAIFallbackReply, Actions: []AIActionItem{}, ExecuteHand: "chat_only"}, err
	}

	parsed, errParse := ParseOctopusAIResponse(aiResponse)
	if errParse != nil || parsed == nil {
		return &OctopusAIResponse{AIReplyText: SafeAIFallbackReply, Actions: []AIActionItem{}, ExecuteHand: "chat_only"}, nil
	}
	if routeDec.UsedDecisionAPI && routeDec.NeedsQuoteReply {
		parsed.QuoteReply = true
	}

	// Layer 4: On-Demand `search_catalog` Hand interception (STRICTLY max 1 follow-up pass — never loops!)
	var lookupQueries []string
	var lookupCategories []string
	hasSearchHand := false
	for _, act := range parsed.Actions {
		if act.Hand == "search_catalog" {
			hasSearchHand = true
			if act.Payload != nil {
				lookupQueries = appendUniqueStrings(lookupQueries, act.Payload.Queries...)
				if strings.TrimSpace(act.Payload.Query) != "" {
					lookupQueries = appendUniqueStrings(lookupQueries, strings.TrimSpace(act.Payload.Query))
				}
				lookupCategories = appendUniqueStrings(lookupCategories, act.Payload.Categories...)
				if strings.TrimSpace(act.Payload.Category) != "" {
					lookupCategories = appendUniqueStrings(lookupCategories, strings.TrimSpace(act.Payload.Category))
				}
			}
		}
	}

	if hasSearchHand && (len(lookupQueries) > 0 || len(lookupCategories) > 0) {
		searchResultBlock := ExecuteSearchCatalogHand(lookupQueries, lookupCategories)
		log.Printf("🔎 [Octopus search_catalog] queries=%v categories=%v -> resolved in RAM", lookupQueries, lookupCategories)

		followUpPrompt := promptBuilder.String() + "\n\n" + searchResultBlock +
			"\n\nFINAL PASS: Now answer the customer warmly using SEARCH_RESULTS above. Do NOT call search_catalog again."

		aiResponse2, err2 := callAIWithKeyFallback(username, cfg, followUpPrompt, llmHistory, true)
		if err2 == nil && strings.TrimSpace(aiResponse2) != "" {
			if parsed2, errP2 := ParseOctopusAIResponse(aiResponse2); errP2 == nil && parsed2 != nil {
				if routeDec.UsedDecisionAPI && routeDec.NeedsQuoteReply {
					parsed2.QuoteReply = true
				}
				filtered := make([]AIActionItem, 0, len(parsed2.Actions))
				for _, act := range parsed2.Actions {
					if act.Hand != "search_catalog" {
						filtered = append(filtered, act)
					}
				}
				parsed2.Actions = filtered
				return parsed2, nil
			}
		}
		// If 2nd pass fails transiently, strip search_catalog so caller doesn't fail and return 1st reply cleanly
		filtered := make([]AIActionItem, 0, len(parsed.Actions))
		for _, act := range parsed.Actions {
			if act.Hand != "search_catalog" {
				filtered = append(filtered, act)
			}
		}
		parsed.Actions = filtered
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

// CallAIModelWithHistory calls Groq (strictly 3-model TPD/TPM rotation), Gemini (strictly 4-model pool), OpenAI, DeepSeek, or OpenRouter.
func CallAIModelWithHistory(provider, model string, temperature float64, rawApiKey, sysPrompt string, history []ChatMessage) (string, error) {
	apiKey := strings.TrimSpace(rawApiKey)
	if apiKey == "" {
		return "", fmt.Errorf("empty API key")
	}

	if isGroqProvider(provider, apiKey) {
		return callGroqPoolWithHistory([]string{apiKey}, temperature, sysPrompt, history, true)
	}
	if isGeminiProvider(provider, apiKey) {
		return callGeminiPoolWithHistory([]string{apiKey}, temperature, sysPrompt, history, true)
	}
	return callNonGroqModelWithHistory(provider, model, temperature, apiKey, sysPrompt, history, true)
}

func isNumericOrPhone(s string) bool {
	if s == "" {
		return false
	}
	clean := nonDigitRegex.ReplaceAllString(s, "")
	return len(clean) >= 8 && (len(clean) == len(s) || strings.HasPrefix(clean, "117") || strings.HasPrefix(clean, "223") || strings.HasPrefix(clean, "91"))
}

func buildOpenAIMessages(sysPrompt string, history []ChatMessage) []ChatMessage {
	messages := make([]ChatMessage, 0, len(history)+1)
	messages = append(messages, ChatMessage{Role: "system", Content: sysPrompt})
	for _, m := range history {
		role := m.Role
		if role == "customer" || role == "incoming" {
			role = "user"
		} else if role == "outgoing" || role == "outgoing_ai" {
			role = "assistant"
		}
		if strings.TrimSpace(m.Content) != "" {
			messages = append(messages, ChatMessage{Role: role, Content: m.Content})
		}
	}
	return messages
}

// callGroqPoolWithHistory strictly uses ONLY the 3 active Groq models across ALL active Groq keys,
// immediately switching models/keys on TPM 429 and tracking TPD exhaustion so every key × model's
// daily TPD is utilized to the fullest!
func callGroqPoolWithHistory(groqKeys []string, temperature float64, sysPrompt string, history []ChatMessage, jsonMode bool) (string, error) {
	if len(groqKeys) == 0 {
		return "", fmt.Errorf("no Groq API keys available")
	}
	messages := buildOpenAIMessages(sysPrompt, history)
	ready, tpmDelayed := groqPool.buildCandidates(groqKeys)

	var lastErr error
	trySlot := func(slot groqCandidateSlot) (string, bool, error) {
		text, tokens, status, headers, body, err := callGroqSingleModel(slot.apiKey, slot.model, temperature, messages, jsonMode)
		if err == nil && strings.TrimSpace(text) != "" {
			remTok := -1
			if rawRem := strings.TrimSpace(headers.Get("x-ratelimit-remaining-tokens")); rawRem != "" {
				if parsedRem, pErr := strconv.Atoi(rawRem); pErr == nil {
					remTok = parsedRem
				}
			}
			resetTok := parseResetDuration(headers.Get("x-ratelimit-reset-tokens"))
			groqPool.recordSuccess(slot.keyHash, slot.model, tokens, remTok, resetTok)
			return text, true, nil
		}

		// Classify HTTP error to update TPM vs TPD cooldown cleanly
		switch status {
		case http.StatusTooManyRequests:
			isTPD, cd := classifyGroq429(headers, body)
			if isTPD {
				groqPool.markTPDExhausted(slot.keyHash, slot.model, cd)
				log.Printf("🚫 [Groq TPD Exhausted] Key #%d | model=%s (cooldown %v) -> switching to next model/key", slot.keyIndex+1, slot.model, cd.Round(time.Minute))
			} else {
				groqPool.markTPMCooldown(slot.keyHash, slot.model, cd)
				log.Printf("⚡ [Groq TPM Limit] Key #%d | model=%s (cooldown %v) -> switching model/key immediately", slot.keyIndex+1, slot.model, cd.Round(time.Second))
			}
		case http.StatusUnauthorized, http.StatusForbidden:
			groqPool.markAuthDisabledForKey(slot.keyHash, 30*time.Minute)
			log.Printf("🔑 [Groq Auth Error %d] Key #%d disabled 30m -> switching to next key", status, slot.keyIndex+1)
		default:
			// Transient 5xx / timeout / 404: brief 45s cooldown so we immediately switch model/key
			groqPool.markTPMCooldown(slot.keyHash, slot.model, 45*time.Second)
			log.Printf("⚠️ [Groq Error] Key #%d | model=%s (%v) -> switching model/key", slot.keyIndex+1, slot.model, err)
		}
		return "", false, err
	}

	for _, slot := range ready {
		if text, ok, err := trySlot(slot); ok {
			return text, nil
		} else {
			lastErr = err
		}
	}

	// If every active slot was temporarily in short TPM cooldown (not TPD-exhausted),
	// try the soonest-expiring slot (waiting briefly up to 3s if cooldown is almost over).
	if len(tpmDelayed) > 0 {
		best := tpmDelayed[0]
		wait := time.Until(best.tpmUntil)
		if wait > 0 && wait <= 3*time.Second {
			time.Sleep(wait)
		}
		if text, ok, err := trySlot(best); ok {
			return text, nil
		} else {
			lastErr = err
		}
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("all Groq keys and models (qwen/qwen3.8-27b, openai/gpt-oss-20b, openai/gpt-oss-120b) are currently rate-limited or exhausted for today")
}

func callGroqSingleModel(apiKey, model string, temperature float64, messages []ChatMessage, jsonMode bool) (string, int64, int, http.Header, []byte, error) {
	const endpoint = "https://api.groq.com/openai/v1/chat/completions"

	maxTokens := 600
	if !jsonMode {
		maxTokens = 320
	}

	doRequest := func(minimal bool) (string, int64, int, http.Header, []byte, error) {
		payload := map[string]interface{}{
			"model":                 model,
			"messages":              messages,
			"temperature":           temperature,
			"max_completion_tokens": maxTokens,
		}
		if !minimal {
			if jsonMode {
				payload["response_format"] = map[string]string{"type": "json_object"}
			}
			// Keep reasoning overhead minimal on gpt-oss models so TPD lasts 3x-5x longer!
			if strings.HasPrefix(model, "openai/gpt-oss") {
				payload["reasoning_effort"] = "low"
			}
		}

		bodyBytes, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(bodyBytes))
		if err != nil {
			return "", 0, 0, nil, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("User-Agent", "VendorWhatsAppBotNode/2.3")

		client := &http.Client{Timeout: 18 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return "", 0, 0, nil, nil, err
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
				Usage struct {
					TotalTokens int64 `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(respBytes, &result); err == nil && len(result.Choices) > 0 {
				content := stripThinkBlocks(result.Choices[0].Message.Content)
				if content != "" {
					return content, result.Usage.TotalTokens, resp.StatusCode, resp.Header, respBytes, nil
				}
			}
			return "", result.Usage.TotalTokens, resp.StatusCode, resp.Header, respBytes, fmt.Errorf("empty content from groq model %s", model)
		}
		return "", 0, resp.StatusCode, resp.Header, respBytes, fmt.Errorf("groq model %s returned status %d", model, resp.StatusCode)
	}

	text, tokens, status, headers, body, err := doRequest(false)
	// If a specific Groq model rejects json_object or reasoning_effort with HTTP 400, retry once with clean payload
	if err != nil && status == http.StatusBadRequest {
		return doRequest(true)
	}
	return text, tokens, status, headers, body, err
}

func callNonGroqModelWithHistory(provider, model string, temperature float64, apiKey, sysPrompt string, history []ChatMessage, jsonMode bool) (string, error) {
	prov := strings.ToLower(strings.TrimSpace(provider))
	if prov == "chatgpt" {
		prov = "openai"
	}
	if strings.HasPrefix(apiKey, "sk-or-") {
		prov = "openrouter"
	} else if strings.HasPrefix(apiKey, "sk-") && prov != "deepseek" {
		prov = "openai"
	} else if strings.HasPrefix(apiKey, "AIza") {
		prov = "gemini"
	}

	cleanModel := strings.TrimSpace(model)
	if isNumericOrPhone(cleanModel) {
		cleanModel = ""
	}

	if prov == "gemini" || prov == "google" || prov == "gemma" {
		return callGeminiPoolWithHistory([]string{apiKey}, temperature, sysPrompt, history, jsonMode)
	}
	return callOpenAIFormatWithHistory(prov, cleanModel, temperature, apiKey, sysPrompt, history, jsonMode)
}

func callOpenAIFormatWithHistory(provider, model string, temperature float64, apiKey, sysPrompt string, history []ChatMessage, jsonMode bool) (string, error) {
	endpoint := "https://api.openai.com/v1/chat/completions"
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
		endpoint = "https://api.openai.com/v1/chat/completions"
		candidateModels = []string{"gpt-4o-mini"}
	}

	if model != "" && !isNumericOrPhone(model) {
		candidateModels = append([]string{model}, candidateModels...)
	}

	messages := buildOpenAIMessages(sysPrompt, history)

	var lastErr error
	for _, m := range candidateModels {
		payload := map[string]interface{}{
			"model":       m,
			"messages":    messages,
			"temperature": temperature,
			"max_tokens":  600,
		}
		if jsonMode && (provider == "openai" || provider == "deepseek") {
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
		req.Header.Set("User-Agent", "VendorWhatsAppBotNode/2.3")

		client := &http.Client{Timeout: 15 * time.Second}
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
				return stripThinkBlocks(result.Choices[0].Message.Content), nil
			}
		}
		lastErr = fmt.Errorf("model %s returned status %d", m, resp.StatusCode)
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("empty AI reply received")
}

// callGeminiAPIWithHistory delegates strictly to the 4-model Gemini/Gemma pool
// (gemma-4-31b-it, gemma-4-26b-a4b-it, gemini-3.1-flash-lite, gemini-3.5-flash-lite).
func callGeminiAPIWithHistory(apiKey, _ string, temperature float64, sysPrompt string, history []ChatMessage, jsonMode bool) (string, error) {
	return callGeminiPoolWithHistory([]string{apiKey}, temperature, sysPrompt, history, jsonMode)
}
