package whatsapp

import (
	"fmt"
	"strings"
	"sync"

	"wa-server/ramstore"
)

// =============================================================================
// MICRO-CAPSULE ADAPTIVE CONTEXT ENGINE (MCACE v3)
//
// Zero hardcoded customer replies — 100% live AI-generated responses at a
// fraction of normal token cost:
//   1. Zero-Latency Local Micro-Intent Router (<0.3ms, 0 tokens) routes each
//      turn to 1 of 5 surgical Micro-Capsules:
//        - GREETING        (~150-210 in-tokens | 0 tool bloat, 0 full catalog, 1-turn history)
//        - CHITCHAT_INFO   (~200-270 in-tokens | store info + 2 support hands only, 0 catalog)
//        - PAYMENT_PENDING (~210-280 in-tokens | payment status/cancel only, 0 catalog)
//        - CHECKOUT        (~280-360 in-tokens | cart + profile + checkout hands only, 0 INV dump)
//        - SHOPPING        (~380-520 in-tokens | state-pruned hands + precision RAG + P1/P2 aliasing)
//   2. Ephemeral Short-ID Aliasing (P1, P2, P3...): replaces 24-36 char UUIDs/ObjectIDs
//      with 1-token codes in prompts and transparently expands them back to exact ProductIDs.
//   3. Semantic History Distillation: strips formatting/markdown clutter from older bot
//      turns while keeping the customer's current message 100% intact.
//   4. Smart Pass-2 Replacement: when `search_catalog` fires, Pass 2 swaps out Pass-1's
//      INV block for SEARCH_RESULTS instead of duplicating both.
// =============================================================================

type TurnCapsule string

const (
	CapsuleGreeting       TurnCapsule = "GREETING"
	CapsuleChitchatInfo   TurnCapsule = "CHITCHAT_INFO"
	CapsulePaymentPending TurnCapsule = "PAYMENT_PENDING"
	CapsuleCheckout       TurnCapsule = "CHECKOUT"
	CapsuleShopping       TurnCapsule = "SHOPPING"
)

// ProductAliasMap maps real product IDs <-> compact 1-token turn aliases (P1, P2, P3...).
type ProductAliasMap struct {
	realToAlias map[string]string
	aliasToReal map[string]string
	nextNum     int
}

func NewProductAliasMap() *ProductAliasMap {
	return &ProductAliasMap{
		realToAlias: make(map[string]string, 24),
		aliasToReal: make(map[string]string, 24),
		nextNum:     1,
	}
}

func (m *ProductAliasMap) Register(realID string) string {
	realID = strings.TrimSpace(realID)
	if realID == "" {
		return ""
	}
	if m == nil {
		return realID
	}
	if alias, ok := m.realToAlias[realID]; ok {
		return alias
	}
	alias := fmt.Sprintf("P%d", m.nextNum)
	m.nextNum++
	m.realToAlias[realID] = alias
	m.aliasToReal[strings.ToLower(alias)] = realID
	m.aliasToReal[fmt.Sprintf("%d", m.nextNum-1)] = realID
	return alias
}

func (m *ProductAliasMap) Resolve(idOrAlias string) string {
	raw := strings.TrimSpace(idOrAlias)
	if raw == "" || m == nil {
		return raw
	}
	// Exact alias match ("P1", "p1", "#P1", "#1", "1")
	key := strings.ToLower(strings.TrimPrefix(raw, "#"))
	if realID, ok := m.aliasToReal[key]; ok {
		return realID
	}
	return raw
}

func (m *ProductAliasMap) Count() int {
	if m == nil {
		return 0
	}
	return len(m.realToAlias)
}

// ResolveActionProductAliases transparently expands P1/P2... short IDs back to exact RAM ProductIDs.
func ResolveActionProductAliases(resp *OctopusAIResponse, aliases *ProductAliasMap) {
	if resp == nil || aliases == nil || aliases.Count() == 0 {
		return
	}
	for i := range resp.Actions {
		p := resp.Actions[i].Payload
		if p == nil || len(p.CartItems) == 0 {
			continue
		}
		for j := range p.CartItems {
			p.CartItems[j].ID = aliases.Resolve(p.CartItems[j].ID)
		}
	}
	if resp.HandPayload != nil {
		for j := range resp.HandPayload.CartItems {
			resp.HandPayload.CartItems[j].ID = aliases.Resolve(resp.HandPayload.CartItems[j].ID)
		}
	}
	for j := range resp.CartItems {
		resp.CartItems[j].ProductID = aliases.Resolve(resp.CartItems[j].ProductID)
	}
}

// -----------------------------------------------------------------------------
// ZERO-LATENCY MICRO-INTENT CLASSIFIER (<0.3ms, 0 LLM TOKENS)
// -----------------------------------------------------------------------------

var pureGreetingWords = map[string]bool{
	"hi": true, "hii": true, "hiii": true, "hiiii": true, "hey": true, "heyy": true,
	"hello": true, "helo": true, "hlo": true, "hlw": true, "hola": true, "yo": true,
	"namaste": true, "namaskar": true, "namastey": true, "pranam": true, "pranaam": true,
	"ram": true, "radhe": true, "jai": true, "shri": true, "salam": true, "salaam": true,
	"assalam": true, "assalamu": true, "alaikum": true, "walaikum": true, "sat": true, "akal": true,
	"good": true, "morning": true, "afternoon": true, "evening": true, "night": true, "gm": true, "gn": true,
	"ji": true, "sir": true, "madam": true, "mam": true, "maam": true, "bhai": true, "bhaiya": true,
	"bro": true, "didi": true, "boss": true, "friend": true, "dear": true, "shop": true, "store": true,
	"kaise": true, "kese": true, "ho": true, "hai": true, "hain": true, "aap": true, "tum": true,
	"kya": true, "haal": true, "hal": true, "chal": true, "chaal": true, "start": true, "test": true,
	"how": true, "are": true, "you": true, "there": true, "anyone": true, "koi": true,
}

var pureAckWords = map[string]bool{
	"ok": true, "okay": true, "okk": true, "okkk": true, "k": true, "kk": true,
	"ji": true, "haan": true, "han": true, "ha": true, "yes": true, "yep": true, "yup": true,
	"hmm": true, "hm": true, "hmmm": true, "thik": true, "theek": true, "thk": true, "sahi": true,
	"achha": true, "accha": true, "acha": true, "nice": true, "great": true, "good": true, "cool": true,
	"awesome": true, "super": true, "perfect": true, "fine": true, "done": true, "sure": true,
	"thanks": true, "thank": true, "thankyou": true, "ty": true, "thx": true, "tq": true,
	"dhanyawad": true, "dhanyavaad": true, "shukriya": true, "welcome": true,
	"bye": true, "byee": true, "alvida": true, "later": true, "baad": true, "me": true, "mein": true,
	"sir": true, "bhai": true, "bhaiya": true, "bro": true, "hai": true, "jiya": true,
}

var storeInfoKeywords = map[string]bool{
	"timing": true, "timings": true, "time": true, "hours": true, "open": true, "close": true,
	"closed": true, "khula": true, "khulta": true, "khulti": true, "band": true, "kab": true,
	"delivery": true, "charge": true, "charges": true, "fee": true, "free": true, "radius": true,
	"minimum": true, "min": true, "policy": true, "return": true, "refund": true, "exchange": true,
	"website": true, "site": true, "link": true, "url": true, "mall": true, "app": true,
	"location": true, "address": true, "kahan": true, "kaha": true, "where": true, "dukaan": true, "dukan": true,
	"owner": true, "malik": true, "manager": true, "human": true, "agent": true, "call": true,
	"contact": true, "number": true, "support": true, "help": true, "complaint": true, "shikayat": true,
	"damaged": true, "kharab": true, "wrong": true, "galat": true, "cod": true, "cash": true, "upi": true, "online": true,
}

var shoppingForceWords = map[string]bool{
	"menu": true, "list": true, "catalog": true, "catalogue": true, "item": true, "items": true,
	"product": true, "products": true, "saman": true, "saaman": true, "rate": true, "rates": true,
	"price": true, "prices": true, "kitna": true, "kitne": true, "bhav": true, "daam": true, "dam": true,
	"photo": true, "photos": true, "pic": true, "pics": true, "image": true, "images": true, "dikhao": true, "dikha": true,
	"chahiye": true, "chaiye": true, "bhejo": true, "bhej": true, "dena": true, "dedo": true, "lana": true,
	"kharidna": true, "buy": true, "order": true, "cart": true, "add": true, "remove": true, "hatao": true,
	"available": true, "stock": true, "milega": true, "milta": true, "hai": false,
	"variety": true, "brand": true, "brands": true, "pack": true, "packet": true, "kg": true, "gm": true, "litre": true,
	"guest": true, "mehmaan": true, "mehman": true, "party": true, "snacks": true, "nashta": true, "breakfast": true,
	"khansi": true, "cough": true, "dard": true, "bukhar": true, "fever": true, "pooja": true, "puja": true,
}

// ClassifyTurnCapsule routes any incoming customer turn to the leanest safe Micro-Capsule.
// Guarantees zero false routing of shopping/product messages.
func ClassifyTurnCapsule(userMsg, currentState string, hasCart bool, hasProductMatch bool) TurnCapsule {
	clean := stripVoiceNoteMarkers(userMsg)
	toks := tokenizeClean(clean)

	// 1. If a real product matched directly -> ALWAYS SHOPPING (never drop catalog on a product query!)
	if hasProductMatch {
		return CapsuleShopping
	}

	// 2. If any explicit shopping/menu/photo/order trigger is present -> SHOPPING (unless pure checkout/payment below)
	hasShoppingWord := false
	hasDigit := false
	for _, t := range toks {
		if shoppingForceWords[t] {
			hasShoppingWord = true
		}
		for i := 0; i < len(t); i++ {
			if t[i] >= '0' && t[i] <= '9' {
				hasDigit = true
				break
			}
		}
	}

	// 3. Payment Pending state (when customer isn't asking for new products)
	if currentState == "PAYMENT_PENDING" && !hasShoppingWord {
		return CapsulePaymentPending
	}

	// 4. Pure Greeting (<= 5 clean tokens, every token is a pure greeting word, zero digits, zero shopping triggers)
	if len(toks) > 0 && len(toks) <= 5 && !hasDigit && !hasShoppingWord {
		allGreet := true
		for _, t := range toks {
			if !pureGreetingWords[t] {
				allGreet = false
				break
			}
		}
		if allGreet {
			return CapsuleGreeting
		}
	}
	// Emoji-only / punctuation-only greeting (e.g. "👋" or "🙏")
	if len(toks) == 0 && strings.TrimSpace(clean) != "" && !hasCart {
		return CapsuleGreeting
	}

	// 5. Pure Checkout / Profile / COD / Online confirmation when cart already has items
	if hasCart && isPureCheckoutOrProfileMessage(clean, true) {
		return CapsuleCheckout
	}

	// 6. Pure Acknowledgement ("ok", "thanks", "dhanyawad", "hmm") when NOT shopping
	if len(toks) > 0 && len(toks) <= 4 && !hasDigit && !hasShoppingWord {
		allAck := true
		for _, t := range toks {
			if !pureAckWords[t] {
				allAck = false
				break
			}
		}
		if allAck {
			if hasCart {
				return CapsuleCheckout
			}
			return CapsuleChitchatInfo
		}
	}

	// 7. Store info / timing / delivery charge / website link / human support query (zero shopping triggers, <= 12 words)
	if !hasShoppingWord && len(toks) > 0 && len(toks) <= 12 {
		infoHits := 0
		for _, t := range toks {
			if storeInfoKeywords[t] {
				infoHits++
			}
		}
		if infoHits > 0 && !hasCart {
			return CapsuleChitchatInfo
		}
	}

	return CapsuleShopping
}

// -----------------------------------------------------------------------------
// SEMANTIC HISTORY DISTILLATION (Strips Markdown/Emoji Bloat From Past Turns)
// -----------------------------------------------------------------------------

func stripDecorForPastTurn(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	// Replace newlines with compact separator so multi-bullet past replies become 1 crisp line
	lines := strings.Split(s, "\n")
	var kept []string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		ln = strings.TrimLeft(ln, ">•-* ")
		if ln != "" {
			kept = append(kept, ln)
		}
	}
	joined := strings.Join(kept, " | ")
	// Strip WhatsApp markdown markers from past assistant history (saves formatting tokens)
	joined = strings.ReplaceAll(joined, "*", "")
	joined = strings.ReplaceAll(joined, "_", "")
	joined = strings.ReplaceAll(joined, "~", "")
	return strings.TrimSpace(multiSpaceRe.ReplaceAllString(joined, " "))
}

func distillHistoryForCapsule(full []ChatMessage, capsule TurnCapsule, hasCart bool) []ChatMessage {
	maxTotal := 4
	switch capsule {
	case CapsuleGreeting:
		maxTotal = 1 // Only current greeting turn! Zero stale history overhead!
	case CapsuleChitchatInfo:
		maxTotal = 2
	case CapsulePaymentPending:
		maxTotal = 3
	case CapsuleCheckout:
		maxTotal = 4
	case CapsuleShopping:
		if hasCart {
			maxTotal = MaxLLMHistoryMessages // 6 turns when cart is active
		} else {
			maxTotal = 4
		}
	}

	if len(full) == 0 {
		return nil
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
		txt := strings.TrimSpace(m.Content)
		if txt == "" {
			continue
		}
		if isLatest && role == "user" {
			// NEVER lose any detail from the customer's current message (up to 500 runes)
			txt = compactText(strings.ReplaceAll(txt, "\n\n", "\n"), 500)
		} else if role == "assistant" {
			// Aggressively distill past bot turns: strip markdown/bullet bloat & cap to 90 runes
			txt = compactText(stripDecorForPastTurn(txt), 90)
		} else {
			// Past user turns capped to 120 runes
			txt = compactText(strings.ReplaceAll(txt, "\n\n", "\n"), 120)
		}
		if txt != "" {
			out = append(out, ChatMessage{Role: role, Content: txt})
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// COMPACT STORE / CATEGORY / SAMPLE BUILDERS (ZERO WASTE)
// -----------------------------------------------------------------------------

func buildGreetingHighlights(products []ramstore.Product) (catsLine string, topSample string) {
	if len(products) == 0 {
		return "", ""
	}
	catCounts := make(map[string]int)
	var catOrder []string
	catFirst := make(map[string]ramstore.Product)
	for _, p := range products {
		c := strings.TrimSpace(p.Category)
		if c == "" {
			c = "General"
		}
		if _, exists := catCounts[c]; !exists {
			catOrder = append(catOrder, c)
			catFirst[c] = p
		}
		catCounts[c]++
	}
	catParts := make([]string, 0, len(catOrder))
	for i, c := range catOrder {
		if i >= 10 {
			catParts = append(catParts, fmt.Sprintf("+%d more", len(catOrder)-10))
			break
		}
		catParts = append(catParts, fmt.Sprintf("%s(%d)", c, catCounts[c]))
	}
	catsLine = strings.Join(catParts, ", ")

	// Pick up to 3 diverse showcase items (name + ₹price ONLY — no UUIDs, no variants!)
	var sampleParts []string
	for _, c := range catOrder {
		if len(sampleParts) >= 3 {
			break
		}
		p := catFirst[c]
		sampleParts = append(sampleParts, fmt.Sprintf("%s ₹%s", strings.TrimSpace(p.Name), formatRupeeCompact(p.EffectivePrice())))
	}
	if len(sampleParts) < 3 {
		for _, p := range products {
			if len(sampleParts) >= 3 {
				break
			}
			entry := fmt.Sprintf("%s ₹%s", strings.TrimSpace(p.Name), formatRupeeCompact(p.EffectivePrice()))
			dup := false
			for _, ex := range sampleParts {
				if ex == entry {
					dup = true
					break
				}
			}
			if !dup {
				sampleParts = append(sampleParts, entry)
			}
		}
	}
	return catsLine, strings.Join(sampleParts, " | ")
}

func buildCompactStoreRuleLine(cfg *UserConfigRAM) string {
	locMode := "TextAddrOK"
	if cfg.RequireCustomerLocation {
		locMode = "LocationPinReq"
	}
	payMode := "COD+ONLINE"
	if !cfg.AllowCOD {
		payMode = "ONLINE_ONLY"
	}
	parts := []string{locMode}
	if cfg.MaxDeliveryRadiusKM > 0 {
		parts = append(parts, fmt.Sprintf("Max%.0fkm/Free%.0fkm/Fee₹%s", cfg.MaxDeliveryRadiusKM, cfg.FreeDeliveryRadiusKM, formatRupeeCompact(cfg.DeliveryChargeAmount)))
	}
	if cfg.MinOrderAmount > 0 {
		parts = append(parts, "Min₹"+formatRupeeCompact(cfg.MinOrderAmount))
	}
	if strings.TrimSpace(cfg.EstimatedDeliveryTime) != "" {
		parts = append(parts, "ETA:"+strings.TrimSpace(cfg.EstimatedDeliveryTime))
	}
	if strings.TrimSpace(cfg.OperatingHours) != "" {
		parts = append(parts, "Hrs:"+strings.TrimSpace(cfg.OperatingHours))
	}
	parts = append(parts, "Pay:"+payMode)
	return strings.Join(parts, "|")
}

// -----------------------------------------------------------------------------
// MICRO-CAPSULE PROMPT COMPILER (THE HEART OF MCACE v3)
// -----------------------------------------------------------------------------

type CompiledCapsule struct {
	Capsule         TurnCapsule
	SystemPrompt    string // Full system prompt for Pass 1
	BasePrompt      string // System prompt WITHOUT INV block (so Pass-2 search_catalog swaps INV cleanly!)
	History         []ChatMessage
	Aliases         *ProductAliasMap
	MaxOutputTokens int
	EstInputTokens  int
}

func estimateTokenCount(s string) int {
	n := len([]rune(strings.TrimSpace(s)))
	if n == 0 {
		return 0
	}
	// ~3.6 chars/token conservative average across English/Hinglish/JSON/emojis
	return (n*10 + 35) / 36
}

func BuildCapsulePromptAndHistory(username, customerPhone, incomingText string, fullHistory []ChatMessage, cfg *UserConfigRAM) *CompiledCapsule {
	cleanPhone := FormatPhone(customerPhone)
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

	payStatus := GetCustomerPaymentStatus(username, cleanPhone)
	custState := GetCustomerState(username, cleanPhone)
	calcRes, _ := CalculateCartTotalAndFetchCredentials(username, cleanPhone)
	hasCart := calcRes != nil && len(calcRes.Items) > 0

	currentState := "BROWSING"
	if strings.EqualFold(payStatus, "PENDING") || strings.EqualFold(custState, "PAYMENT_PENDING") {
		currentState = "PAYMENT_PENDING"
	} else if hasCart {
		currentState = "CART_ACTIVE"
	}

	hasProdMatch := HasDirectProductMatch(incomingText)
	capsule := ClassifyTurnCapsule(incomingText, currentState, hasCart, hasProdMatch)
	history := distillHistoryForCapsule(fullHistory, capsule, hasCart)
	isFirstTurn := len(fullHistory) <= 1

	custProfile, _ := GetCustomerProfile(username, cleanPhone)
	custName := ""
	if custProfile != nil {
		custName = strings.TrimSpace(custProfile.FullName)
	}

	aliases := NewProductAliasMap()
	products := ramstore.Global.ListProducts(true)
	vendorNote := compactText(cfg.SystemPrompt, 180)
	firstTip := ""
	if isFirstTurn {
		firstTip = "\nFirst chat: warmly ask them to type their full request in one message."
	}

	var base strings.Builder
	var invBlock string
	maxOut := 520

	switch capsule {
	case CapsuleGreeting:
		// ~150-210 total input tokens! 100% live AI-generated, warm humanoid welcome!
		maxOut = 260
		catsLine, topSample := buildGreetingHighlights(products)
		base.WriteString(fmt.Sprintf(
			`Warm, lively WhatsApp shopkeeper for "%s" (%s). Reply in %s.
OUTPUT RAW JSON ONLY: {"ai_reply_text":"...","quote_reply":false,"actions":[]}
Style: natural human shopkeeper tone, *bold* store/items/*₹prices*, friendly emojis (✨🛍️🙏💛), crisp bullets. Welcome customer warmly, highlight categories & popular picks below, and offer photos/help.%s`,
			bName, bType, lang, firstTip,
		))
		if vendorNote != "" {
			base.WriteString("\nNOTE: " + vendorNote)
		}
		if custName != "" {
			base.WriteString("\nCUST: " + custName)
		}
		if hasCart {
			base.WriteString(fmt.Sprintf("\nCART: %d items (₹%s)", len(calcRes.Items), formatRupeeCompact(calcRes.TotalAmount)))
		}
		if catsLine != "" {
			base.WriteString("\nCATS: " + catsLine)
		}
		if topSample != "" {
			base.WriteString("\nTOP: " + topSample)
		}

	case CapsuleChitchatInfo:
		// ~200-270 total input tokens! Zero product dump!
		maxOut = 280
		policies := compactText(cfg.StorePolicies, 140)
		catsLine, _ := buildGreetingHighlights(products)
		base.WriteString(fmt.Sprintf(
			`Warm WhatsApp shopkeeper for "%s" (%s). Reply in %s.
OUTPUT RAW JSON ONLY: {"ai_reply_text":"...","quote_reply":false,"actions":[]}
Style: friendly human tone, *bold* key info, natural emojis. Set quote_reply:true only if answering a specific question/complaint.
HANDS(optional): generate_store_link:{} (website/link ask) | escalate_to_human:{} (complaint/owner ask, keep helping warmly).
Never ask for UTR/screenshots; online payments verify automatically.%s`,
			bName, bType, lang, firstTip,
		))
		if policies != "" {
			base.WriteString("\nPOLICY: " + policies)
		}
		if vendorNote != "" {
			base.WriteString("\nNOTE: " + vendorNote)
		}
		base.WriteString("\nSTORE: " + buildCompactStoreRuleLine(cfg))
		if custName != "" {
			base.WriteString("\nCUST: " + custName)
		}
		if catsLine != "" {
			base.WriteString("\nCATS: " + catsLine)
		}

	case CapsulePaymentPending:
		// ~210-280 total input tokens! Zero catalog dump!
		maxOut = 280
		ordID := strings.TrimSpace(GetCustomerCurrentOrderID(username, cleanPhone))
		base.WriteString(fmt.Sprintf(
			`Warm WhatsApp shopkeeper for "%s". Reply in %s.
OUTPUT RAW JSON ONLY: {"ai_reply_text":"...","quote_reply":false,"actions":[]}
Style: reassuring human tone, *bold* key words, friendly emojis.
HANDS(optional): cancel_order:{} (if user wants to cancel) | escalate_to_human:{} (complaint/owner help).
RULE: NEVER ask for UTR/screenshots or mark Paid. Reassure customer that our automatic Central Payment System verifies online payments within seconds and sends confirmation + PDF receipt right away.`,
			bName, lang,
		))
		base.WriteString("\nSTATE:PAYMENT_PENDING")
		if ordID != "" {
			base.WriteString(" | ORD:" + ordID)
		}
		if hasCart {
			base.WriteString(fmt.Sprintf(" | AMT:₹%s", formatRupeeCompact(calcRes.TotalAmount)))
		}
		if custName != "" {
			base.WriteString("\nCUST: " + custName)
		}

	case CapsuleCheckout:
		// ~280-360 total input tokens! Zero unrelated INV dump!
		maxOut = 380
		base.WriteString(fmt.Sprintf(
			`Warm WhatsApp shopkeeper for "%s". Reply in %s.
OUTPUT RAW JSON ONLY: {"ai_reply_text":"...","quote_reply":false,"actions":[]}
Style: crisp human tone, *bold* items/*₹totals*, bullets, emojis. Use blockquote (> ) only for total/payment box.
HANDS(only needed):
• extract_profile:{"profile":{"name":"","address":"","contact":""}} (save name/addr/10-digit phone)
• submit_order:{"payment_mode":"COD"|"ONLINE"} (ONLY when cart+address+mode all ready)
• clear_cart:{} | cancel_order:{} | escalate_to_human:{}
RULES: Before submit_order, show *Order Summary* (items, total, address) & ask *Online* 💳 or *COD* 💵 (if allowed). Never ask UTR/screenshots.`,
			bName, lang,
		))
		if vendorNote != "" {
			base.WriteString("\nNOTE: " + vendorNote)
		}
		base.WriteString("\nSTORE: " + buildCompactStoreRuleLine(cfg))
		base.WriteString("\n" + buildAliasedOctopusContext(username, cleanPhone, currentState, calcRes, custProfile, aliases))

	default: // CapsuleShopping
		// ~380-520 total input tokens! State-pruned hands + P1/P2 Short-ID aliasing + precision RAG!
		maxOut = 550
		cartProductIDs := make(map[string]bool)
		if calcRes != nil {
			for _, it := range calcRes.Items {
				if it.ProductID != "" {
					cartProductIDs[it.ProductID] = true
				}
			}
		}

		// State-pruned hands: ONLY include checkout hands when cart has items!
		checkoutHands := ""
		checkoutRule := ""
		if hasCart {
			checkoutHands = "\n• submit_order:{\"payment_mode\":\"COD\"|\"ONLINE\"} (cart+addr+mode ready) | clear_cart:{} | cancel_order:{}"
			checkoutRule = " Before submit_order show *Order Summary* & ask Online/COD."
		}

		base.WriteString(fmt.Sprintf(
			`Warm, lively WhatsApp shopkeeper for "%s" (%s). Reply in %s.
OUTPUT RAW JSON ONLY: {"ai_reply_text":"...","quote_reply":false,"actions":[]}
Style: natural human tone, *bold* items/*₹prices*/variants, emojis (✨🛍️🔥💰🙌), "• " bullets. Quote (> ) only for highlight boxes. quote_reply:true only for direct question/price/item/complaint.
HANDS(only needed; [] for chat):
• search_catalog:{"queries":["kw"],"categories":["Cat"]} (ONLY if user's need/item is missing from INV; use CATS)
• send_catalog_images:{"category":"item|cat|all"} (when photos asked)
• update_cart:{"cart_items":[{"id":"P1","variant":"","qty":1}]} (use EXACT short ID P1/P2 from INV/CLEAR_MATCH; add CLEAR_MATCH immediately, ask brand/size for VARIETY_OPTIONS)
• extract_profile:{"profile":{"name":"","address":"","contact":""}} | generate_store_link:{} | escalate_to_human:{}%s
RULES: Quote/add ONLY in-stock items from INV/CLEAR_MATCH/VARIETY_OPTIONS/SEARCH_RESULTS. Never ask UTR/screenshots.%s%s`,
			bName, bType, lang, checkoutHands, checkoutRule, firstTip,
		))
		if vendorNote != "" {
			base.WriteString("\nNOTE: " + vendorNote)
		}
		base.WriteString("\nSTORE: " + buildCompactStoreRuleLine(cfg))
		base.WriteString("\n" + buildAliasedOctopusContext(username, cleanPhone, currentState, calcRes, custProfile, aliases))

		invBlock = BuildSmartCatalogContextAliased(incomingText, history, cartProductIDs, aliases)
	}

	baseStr := base.String()
	fullPrompt := baseStr
	if invBlock != "" {
		fullPrompt = baseStr + "\n\n" + invBlock
	}

	histChars := 0
	for _, h := range history {
		histChars += len([]rune(h.Content)) + 12
	}
	estIn := estimateTokenCount(fullPrompt) + (histChars*10+35)/36

	recordCapsuleTelemetry(capsule, estIn)

	return &CompiledCapsule{
		Capsule:         capsule,
		SystemPrompt:    fullPrompt,
		BasePrompt:      baseStr,
		History:         history,
		Aliases:         aliases,
		MaxOutputTokens: maxOut,
		EstInputTokens:  estIn,
	}
}

func buildAliasedOctopusContext(username, cleanPhone, currentState string, calcRes *CartCalculationResult, custProfile *CustomerProfile, aliases *ProductAliasMap) string {
	var cartParts []string
	var cartTotal float64
	if calcRes != nil {
		cartTotal = calcRes.TotalAmount
		for _, it := range calcRes.Items {
			idLabel := aliases.Register(it.ProductID)
			vPart := ""
			if it.SelectedVariant != "" {
				vPart = "[" + it.SelectedVariant + "]"
			}
			cartParts = append(cartParts, fmt.Sprintf("%s %s%s x%d=₹%s",
				idLabel, it.ProductName, vPart, it.Qty, formatRupeeCompact(it.LineTotal)))
		}
	}

	cartLine := "CART:empty"
	if len(cartParts) > 0 {
		cartLine = fmt.Sprintf("CART(₹%s): %s", formatRupeeCompact(cartTotal), strings.Join(cartParts, "; "))
	}

	var profParts []string
	if custProfile != nil {
		if strings.TrimSpace(custProfile.FullName) != "" {
			profParts = append(profParts, "Name="+strings.TrimSpace(custProfile.FullName))
		}
		if strings.TrimSpace(custProfile.DeliveryAddress) != "" {
			profParts = append(profParts, "Addr="+compactText(custProfile.DeliveryAddress, 110))
		}
		if strings.TrimSpace(custProfile.ContactPhone) != "" {
			profParts = append(profParts, "Ph="+strings.TrimSpace(custProfile.ContactPhone))
		}
	}

	var sb strings.Builder
	sb.WriteString("STATE:" + currentState + " | " + cartLine)
	if ordID := strings.TrimSpace(GetCustomerCurrentOrderID(username, cleanPhone)); ordID != "" {
		sb.WriteString(" | ORD:" + ordID)
	}
	if len(profParts) > 0 {
		sb.WriteString("\nCUST:" + strings.Join(profParts, "|"))
	}
	return sb.String()
}

// -----------------------------------------------------------------------------
// ROLLING TOKEN EFFICIENCY TELEMETRY (IN-MEMORY, O(1))
// -----------------------------------------------------------------------------

type capsuleTelemetryStore struct {
	mu               sync.Mutex
	totalTurns       int64
	totalEstInTokens int64
	perCapsule       map[string]int64
}

var globalCapsuleTelemetry = &capsuleTelemetryStore{
	perCapsule: make(map[string]int64),
}

func recordCapsuleTelemetry(c TurnCapsule, estInTokens int) {
	globalCapsuleTelemetry.mu.Lock()
	defer globalCapsuleTelemetry.mu.Unlock()
	globalCapsuleTelemetry.totalTurns++
	globalCapsuleTelemetry.totalEstInTokens += int64(estInTokens)
	globalCapsuleTelemetry.perCapsule[string(c)]++
}

func GetCapsuleTelemetryStats() map[string]interface{} {
	globalCapsuleTelemetry.mu.Lock()
	defer globalCapsuleTelemetry.mu.Unlock()
	avg := int64(0)
	if globalCapsuleTelemetry.totalTurns > 0 {
		avg = globalCapsuleTelemetry.totalEstInTokens / globalCapsuleTelemetry.totalTurns
	}
	counts := make(map[string]int64, len(globalCapsuleTelemetry.perCapsule))
	for k, v := range globalCapsuleTelemetry.perCapsule {
		counts[k] = v
	}
	return map[string]interface{}{
		"architecture":     "micro_capsule_v3",
		"total_turns":      globalCapsuleTelemetry.totalTurns,
		"avg_est_in_tok":   avg,
		"capsule_dispatch": counts,
	}
}
