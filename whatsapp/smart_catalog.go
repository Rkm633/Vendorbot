package whatsapp

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"wa-server/ramstore"
)

// =============================================================================
// 4-LAYER STATIC HYBRID CATALOG SEARCH ENGINE (500+ Products, 20-25 Item Lists)
//   Layer 1: Hinglish-Hindi-English Synonym Expansion + Phonetic + Fuzzy Match
//   Layer 2: Multi-Item Line Splitter (20-25 items/message -> per-line lookup)
//   Layer 3: Confidence Split (CLEAR_MATCH vs OPTIONS vs UNMATCHED) + Checkout Skip
//   Layer 4: On-Demand `search_catalog` Hand for Indirect / Need-Based Queries
// =============================================================================

// commerceSynonymGroups maps equivalent Hinglish, Hindi-transliterated, and English commerce terms.
// Every word in a group expands bidirectionally at O(1) lookup cost with zero external dependencies.
var commerceSynonymGroups = [][]string{
	{"sugar", "chini", "cheeni", "shakkar", "shakar", "misri", "mishri"},
	{"atta", "aata", "ata", "wheat", "gehu", "gehun", "chakki", "flour"},
	{"maida", "refined", "allpurpose"},
	{"besan", "gram", "chickpea"},
	{"suji", "sooji", "rava", "semolina"},
	{"poha", "chura", "chivda", "flattened"},
	{"daliya", "dalia", "oats", "porridge"},
	{"sewai", "sevai", "vermicelli"},
	{"rice", "chawal", "basmati", "sona", "kolam"},
	{"dal", "daal", "lentil", "pulses", "arhar", "toor", "tuvar", "moong", "mung", "masoor", "urad", "chana", "rajma", "chole", "kabuli"},
	{"mustard", "sarso", "sarson", "kachi", "ghani"},
	{"oil", "tel", "tail", "refind", "refined", "sunflower", "soyabean", "groundnut", "olive"},
	{"ghee", "ghi", "clarified"},
	{"butter", "makkhan", "makhan"},
	{"milk", "doodh", "dudh", "dairy"},
	{"curd", "dahi", "yogurt", "yoghurt"},
	{"paneer", "cottage"},
	{"buttermilk", "chaas", "chhaach", "lassi", "mattha"},
	{"cheese", "cheez"},
	{"salt", "namak", "sendha", "iodized"},
	{"turmeric", "haldi"},
	{"chilli", "chili", "mirch", "mirchi", "lalmirch", "kashmiri"},
	{"coriander", "dhaniya", "dhania"},
	{"cumin", "jeera", "jira"},
	{"spice", "spices", "masala", "masale", "garam"},
	{"pepper", "kalimirch", "blackpepper"},
	{"cardamom", "elaichi", "ilaychi"},
	{"clove", "laung", "long"},
	{"cinnamon", "dalchini"},
	{"fenugreek", "methi"},
	{"fennel", "saunf", "sonf"},
	{"carom", "ajwain"},
	{"asafoetida", "hing", "heeng"},
	{"ginger", "adrak", "adarak"},
	{"garlic", "lahsun", "lehsun", "lasun"},
	{"onion", "pyaz", "pyaaz", "kanda"},
	{"potato", "aloo", "alu"},
	{"tomato", "tamatar"},
	{"lemon", "nimbu", "neebu"},
	{"apple", "seb"},
	{"banana", "kela"},
	{"mango", "aam"},
	{"orange", "santra", "narangi"},
	{"grapes", "angoor", "angur"},
	{"pomegranate", "anar", "anaar"},
	{"tea", "chai", "chay", "patti", "chaipatti"},
	{"coffee", "kafi", "nescafe", "bru"},
	{"biscuit", "biscuits", "biskit", "biskut", "cookie", "cookies", "parle", "britannia", "marie", "goodday", "oreo"},
	{"namkeen", "snacks", "snack", "bhujia", "bhujiya", "mixture", "sev", "chips", "kurkure", "lays", "haldiram"},
	{"noodles", "noodle", "maggi", "magi", "yippee", "pasta", "macaroni", "chowmein"},
	{"chocolate", "chocolates", "choco", "candy", "toffee", "dairy", "kitkat", "munch", "perk"},
	{"sweet", "sweets", "mithai", "mithayi", "laddu", "ladoo", "barfi", "gulab", "rasgulla", "soan", "papdi"},
	{"bread", "pav", "bun", "rusk", "toast", "cake", "bakery", "muffin", "cupcake"},
	{"juice", "colddrink", "drink", "soda", "beverage", "coke", "pepsi", "sprite", "frooti", "maaza", "real", "tropicana"},
	{"water", "paani", "pani", "bisleri", "mineral"},
	{"jaggery", "gur", "gud"},
	{"honey", "shahad", "sahad"},
	{"pickle", "achar", "achaar"},
	{"sauce", "ketchup", "chutney", "jam", "mayo", "mayonnaise"},
	{"soap", "sabun", "saabun", "bath", "lifebuoy", "lux", "dettol", "detol", "dove", "pears", "santoor"},
	{"detergent", "surf", "washing", "powder", "rin", "tide", "ariel", "wheel", "ghadi", "kapda"},
	{"shampoo", "hair", "baal", "conditioner", "clinic", "sunsilk", "head", "pantene", "dove"},
	{"toothpaste", "paste", "manjan", "colgate", "colgat", "pepsodent", "closeup", "sensodyne", "brush", "toothbrush"},
	{"facewash", "cream", "lotion", "moisturizer", "powder", "talc", "deodorant", "deo", "perfume"},
	{"dishwash", "bartan", "vim", "pril", "scrubber", "utensil"},
	{"cleaner", "floor", "toilet", "harpic", "lizol", "phenyl", "finayl", "colin", "domex"},
	{"mosquito", "machhar", "machar", "coil", "allout", "goodknight", "mortien", "hit", "spray"},
	{"diaper", "diapers", "pampers", "huggies", "mamy", "baby", "wipes"},
	{"sanitary", "pad", "pads", "whisper", "stayfree", "sofy"},
	{"pooja", "puja", "agarbatti", "incense", "dhoop", "kapoor", "camphor", "diya", "batti", "roli", "chandan", "kumkum"},
	{"cough", "khansi", "khasi", "cold", "sardi", "zukham", "jukham", "throat", "gala", "vicks", "strepsils", "honitus", "inhaler"},
	{"pain", "dard", "headache", "sirdard", "balm", "zandu", "amrutanjan", "moov", "volini", "iodex", "fever", "bukhar"},
	{"dryfruit", "dryfruits", "almond", "badam", "cashew", "kaju", "raisin", "kishmish", "kismis", "walnut", "akhrot", "pistachio", "pista", "makhana", "dates", "khajoor"},
	{"peanut", "peanuts", "moongfali", "mungfali", "singdana"},
	{"coconut", "nariyal", "kopra"},
	{"soya", "soybean", "soyabean", "nutrela", "chunks"},
	{"egg", "eggs", "anda", "ande"},
	{"chicken", "murga", "mutton", "meat", "fish", "machli"},
	{"matchbox", "machis", "candle", "mombatti"},
	{"battery", "cell", "eveready", "duracell"},
	{"notebook", "copy", "register", "pen", "pencil", "eraser", "stationery"},
	{"tiffin", "lunchbox", "kids", "bachha", "bachhe", "school"},
	{"guest", "mehmaan", "mehman", "party"},
}

var (
	synonymLookup     map[string][]string
	phoneticSynLookup map[string][]string

	itemSplitLineRegex   = regexp.MustCompile(`[\n\r,;•|]+`)
	numberedSplitRegex   = regexp.MustCompile(`(?i)(?:^|\s)(?:\d{1,2}[\.\)]\s+)`)
	conjunctionSplitRe   = regexp.MustCompile(`(?i)\s+(?:aur|and|\+|&)\s+`)
	qtyUnitExtractRegex  = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*(kg|kilo|g|gm|gms|gram|grams|l|ltr|litre|liter|ml|pkt|pkts|packet|packets|pack|packs|pcs|pc|piece|pieces|bottle|bottles|btl|box|boxes|dozen|dz|jar|tin|pouch|bag|sachet|strip|strips|x)\b`)
	bareNumberPrefixRe   = regexp.MustCompile(`^\s*(?:[-*•]+|\d{1,2}[\.\)])\s*`)
	nonAlnumSpaceRe      = regexp.MustCompile(`[^a-z0-9\s]+`)
	multiSpaceRe         = regexp.MustCompile(`\s+`)
)

func init() {
	synonymLookup = make(map[string][]string, 600)
	phoneticSynLookup = make(map[string][]string, 600)

	for _, group := range commerceSynonymGroups {
		cleanGroup := make([]string, 0, len(group))
		for _, w := range group {
			w = strings.ToLower(strings.TrimSpace(w))
			if w != "" {
				cleanGroup = append(cleanGroup, w)
			}
		}
		for _, w := range cleanGroup {
			synonymLookup[w] = appendUniqueStrings(synonymLookup[w], cleanGroup...)
			pk := phoneticKey(w)
			if len(pk) >= 3 {
				phoneticSynLookup[pk] = appendUniqueStrings(phoneticSynLookup[pk], cleanGroup...)
			}
		}
	}
}

func appendUniqueStrings(dst []string, items ...string) []string {
	for _, it := range items {
		exists := false
		for _, d := range dst {
			if d == it {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, it)
		}
	}
	return dst
}

// phoneticKey normalizes Hinglish/English spelling variations so typos like
// "ashirwad" <-> "aashirvaad", "cheeni" <-> "chini", "detol" <-> "dettol", "biskut" <-> "biscuit" match at O(1).
func phoneticKey(word string) string {
	w := strings.ToLower(strings.TrimSpace(word))
	if w == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"chh", "ch",
		"ph", "f",
		"sh", "s",
		"kh", "k",
		"gh", "g",
		"bh", "b",
		"dh", "d",
		"th", "t",
		"ee", "i",
		"oo", "u",
		"aa", "a",
		"ck", "k",
		"z", "j",
		"w", "v",
		"q", "k",
		"y", "i",
	)
	w = replacer.Replace(w)
	var b strings.Builder
	b.Grow(len(w))
	var prev byte
	for i := 0; i < len(w); i++ {
		c := w[i]
		if c >= 'a' && c <= 'z' || (c >= '0' && c <= '9') {
			if c != prev {
				b.WriteByte(c)
				prev = c
			}
		}
	}
	return b.String()
}

// levenshteinWithin returns true if the edit distance between a and b is <= maxDist.
func levenshteinWithin(a, b string, maxDist int) bool {
	if a == b {
		return true
	}
	la, lb := len(a), len(b)
	diff := la - lb
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDist {
		return false
	}
	if la == 0 || lb == 0 {
		return diff <= maxDist
	}

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		rowMin := curr[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
			if m < rowMin {
				rowMin = m
			}
		}
		if rowMin > maxDist {
			return false
		}
		prev, curr = curr, prev
	}
	return prev[lb] <= maxDist
}

var fillerOrderWords = map[string]bool{
	"kilo": true, "kg": true, "gm": true, "gms": true, "gram": true, "grams": true,
	"ltr": true, "litre": true, "liter": true, "ml": true, "pkt": true, "pkts": true,
	"packet": true, "packets": true, "pack": true, "packs": true, "pcs": true, "pc": true,
	"piece": true, "pieces": true, "bottle": true, "bottles": true, "btl": true,
	"box": true, "boxes": true, "dozen": true, "pouch": true, "bag": true, "jar": true,
	"mujhe": true, "mujhko": true, "chahiye": true, "chahie": true, "dena": true,
	"dijiye": true, "de": true, "do": true, "bhej": true, "bhejo": true, "bhejna": true,
	"add": true, "kar": true, "karo": true, "kardo": true, "krdo": true, "please": true,
	"pls": true, "plz": true, "bhai": true, "bhaiya": true, "sir": true, "ji": true,
	"ka": true, "ke": true, "ki": true, "ko": true, "se": true, "me": true, "mein": true,
	"ek": true, "do2": true, "aur": true, "and": true, "or": true, "ya": true,
	"wala": true, "wali": true, "wale": true, "want": true, "need": true, "send": true,
	"give": true, "get": true, "order": true, "item": true, "items": true,
	"batao": true, "dikhao": true, "hai": true, "hain": true, "hoga": true, "kya": true,
	"rate": true, "price": true, "kitna": true, "kitne": true, "kaun": true, "sa": true,
	"sabse": true, "accha": true, "acha": true, "best": true, "fresh": true, "new": true,
	"cart": true, "list": true, "ye": true, "yeh": true, "wo": true, "woh": true,
	"mera": true, "meri": true, "mere": true, "apna": true, "bhi": true, "toh": true,
	"thoda": true, "kuch": true, "kuchh": true, "saman": true, "saaman": true,
}

var checkoutOnlyWords = map[string]bool{
	"cod": true, "cash": true, "online": true, "upi": true, "paytm": true, "gpay": true,
	"phonepe": true, "qr": true, "pay": true, "payment": true, "confirm": true, "confirmed": true,
	"proceed": true, "checkout": true, "place": true, "submit": true, "yes": true, "haan": true,
	"han": true, "ha": true, "ok": true, "okay": true, "theek": true, "thik": true, "done": true,
	"address": true, "addr": true, "pincode": true, "pin": true, "sector": true, "road": true,
	"nagar": true, "colony": true, "flat": true, "house": true, "gali": true, "mohalla": true,
	"near": true, "behind": true, "opposite": true, "floor": true, "tower": true, "block": true,
	"phase": true, "village": true, "post": true, "dist": true, "district": true, "state": true,
	"city": true, "mobile": true, "phone": true, "number": true, "name": true, "naam": true,
	"mera": true, "hai": true, "deliver": true, "delivery": true, "kar": true, "do": true,
	"karo": true, "kardo": true, "bhej": true, "bhejo": true, "order": true, "bill": true,
	"total": true, "cancel": true, "status": true, "track": true, "paid": true, "donehai": true,
}

// indexedProduct holds pre-tokenized lowercase & phonetic forms for ultra-fast <2ms scoring across 500+ items.
type indexedProduct struct {
	prod       ramstore.Product
	nameLow    string
	catLow     string
	descLow    string
	tagsLow    string
	variantLow string
	nameTokens []string
	allTokens  []string
	phonTokens []string
}

func tokenizeClean(s string) []string {
	low := strings.ToLower(s)
	low = nonAlnumSpaceRe.ReplaceAllString(low, " ")
	fields := strings.Fields(low)
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		if len(f) >= 2 && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func buildIndexedProducts(products []ramstore.Product) []indexedProduct {
	out := make([]indexedProduct, 0, len(products))
	for _, p := range products {
		nameLow := strings.ToLower(strings.TrimSpace(p.Name))
		catLow := strings.ToLower(strings.TrimSpace(p.Category))
		descLow := strings.ToLower(strings.TrimSpace(p.Description))
		tagsLow := strings.ToLower(strings.Join(p.Tags, " "))

		var vNames []string
		for _, v := range p.Variants {
			if lbl := strings.TrimSpace(v.DisplayLabel()); lbl != "" {
				vNames = append(vNames, strings.ToLower(lbl))
			}
		}
		variantLow := strings.Join(vNames, " ")

		nameToks := tokenizeClean(nameLow)
		allToks := tokenizeClean(nameLow + " " + catLow + " " + tagsLow + " " + variantLow + " " + descLow)
		phonSet := make(map[string]bool, len(allToks))
		phonToks := make([]string, 0, len(allToks))
		for _, t := range allToks {
			pk := phoneticKey(t)
			if len(pk) >= 3 && !phonSet[pk] {
				phonSet[pk] = true
				phonToks = append(phonToks, pk)
			}
		}

		out = append(out, indexedProduct{
			prod:       p,
			nameLow:    nameLow,
			catLow:     catLow,
			descLow:    descLow,
			tagsLow:    tagsLow,
			variantLow: variantLow,
			nameTokens: nameToks,
			allTokens:  allToks,
			phonTokens: phonToks,
		})
	}
	return out
}

// expandQueryToken returns the token itself + Hinglish/English synonyms + phonetic synonyms.
func expandQueryToken(tok string) (exact []string, expanded []string) {
	t := strings.ToLower(strings.TrimSpace(tok))
	if t == "" {
		return nil, nil
	}
	exact = []string{t}
	expSet := make(map[string]bool)
	expSet[t] = true

	if syns, ok := synonymLookup[t]; ok {
		for _, s := range syns {
			if !expSet[s] {
				expSet[s] = true
				expanded = append(expanded, s)
			}
		}
	}
	pk := phoneticKey(t)
	if len(pk) >= 3 {
		if syns, ok := phoneticSynLookup[pk]; ok {
			for _, s := range syns {
				if !expSet[s] {
					expSet[s] = true
					expanded = append(expanded, s)
				}
			}
		}
	}
	return exact, expanded
}

// extractSearchTokensAndHint strips filler words and returns meaningful search keywords + optional unit/size hint (e.g. "1kg", "500g", "2l").
func extractSearchTokensAndHint(rawChunk string) (tokens []string, unitHint string) {
	low := strings.ToLower(strings.TrimSpace(rawChunk))
	if m := qtyUnitExtractRegex.FindStringSubmatch(low); len(m) >= 3 {
		unitNorm := strings.ToLower(m[2])
		switch unitNorm {
		case "kilo":
			unitNorm = "kg"
		case "gm", "gms", "gram", "grams":
			unitNorm = "g"
		case "ltr", "litre", "liter":
			unitNorm = "l"
		}
		if unitNorm == "kg" || unitNorm == "g" || unitNorm == "l" || unitNorm == "ml" {
			unitHint = m[1] + unitNorm
		}
	}
	cleaned := qtyUnitExtractRegex.ReplaceAllString(low, " ")
	cleaned = nonAlnumSpaceRe.ReplaceAllString(cleaned, " ")
	seen := make(map[string]bool)
	for _, f := range strings.Fields(cleaned) {
		if len(f) < 2 || fillerOrderWords[f] || ragStopWords[f] {
			continue
		}
		// Skip pure numbers (quantities like "2", "4")
		allDigits := true
		for i := 0; i < len(f); i++ {
			if f[i] < '0' || f[i] > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			continue
		}
		if !seen[f] {
			seen[f] = true
			tokens = append(tokens, f)
		}
	}
	return tokens, unitHint
}

// scoreIndexedProduct scores how well an indexed product matches the given query tokens + optional unit hint + raw chunk.
func scoreIndexedProduct(ip indexedProduct, rawChunk string, tokens []string, unitHint string) int {
	if len(tokens) == 0 {
		return 0
	}
	score := 0
	chunkLow := strings.ToLower(strings.TrimSpace(rawChunk))

	// Exact full product name substring boost
	if len(ip.nameLow) >= 3 && strings.Contains(chunkLow, ip.nameLow) {
		score += 20
	}

	matchedToks := 0
	for _, tok := range tokens {
		tokScore := 0
		exactList, synList := expandQueryToken(tok)
		tokPhon := phoneticKey(tok)

		// 1. Direct token match in name / tags / category / desc
		for _, et := range exactList {
			if strings.Contains(ip.nameLow, et) {
				tokScore = maxInt(tokScore, 10)
			}
			for _, nt := range ip.nameTokens {
				if nt == et {
					tokScore = maxInt(tokScore, 14)
				} else if len(et) >= 4 && strings.HasPrefix(nt, et) {
					tokScore = maxInt(tokScore, 11)
				}
			}
			if ip.tagsLow != "" && strings.Contains(ip.tagsLow, et) {
				tokScore = maxInt(tokScore, 10)
			}
			if strings.Contains(ip.catLow, et) {
				tokScore = maxInt(tokScore, 6)
			}
			if strings.Contains(ip.descLow, et) {
				tokScore = maxInt(tokScore, 3)
			}
		}

		// 2. Synonym expansion match (e.g., "chini" -> "sugar", "sarso" -> "mustard", "tel" -> "oil")
		if tokScore < 12 {
			for _, syn := range synList {
				if strings.Contains(ip.nameLow, syn) {
					tokScore = maxInt(tokScore, 11)
				} else if ip.tagsLow != "" && strings.Contains(ip.tagsLow, syn) {
					tokScore = maxInt(tokScore, 9)
				} else if strings.Contains(ip.catLow, syn) {
					tokScore = maxInt(tokScore, 6)
				} else if strings.Contains(ip.descLow, syn) {
					tokScore = maxInt(tokScore, 3)
				}
			}
		}

		// 3. Phonetic & Fuzzy Levenshtein fallback (typos like "ashirwad" -> "aashirvaad", "magi" -> "maggi")
		if tokScore < 8 && len(tok) >= 4 {
			if len(tokPhon) >= 3 {
				for _, pt := range ip.phonTokens {
					if pt == tokPhon {
						tokScore = maxInt(tokScore, 9)
						break
					}
				}
			}
			if tokScore < 7 {
				maxDist := 1
				if len(tok) >= 6 {
					maxDist = 2
				}
				for _, nt := range ip.nameTokens {
					if len(nt) >= 4 && levenshteinWithin(tok, nt, maxDist) {
						tokScore = maxInt(tokScore, 8)
						break
					}
				}
			}
		}

		if tokScore > 0 {
			matchedToks++
			score += tokScore
		}
	}

	// Multi-token coverage bonus (e.g., "sarso" + "tel" both matching "Mustard Oil"!)
	if len(tokens) >= 2 && matchedToks == len(tokens) {
		score += 8
	} else if len(tokens) >= 2 && matchedToks == 0 {
		return 0
	}

	// Unit/size variant hint boost (e.g. "10kg" matches "10kg" in name or variant!)
	if score > 0 && unitHint != "" {
		if strings.Contains(strings.ReplaceAll(ip.nameLow, " ", ""), unitHint) ||
			strings.Contains(strings.ReplaceAll(ip.variantLow, " ", ""), unitHint) {
			score += 5
		}
	}

	return score
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// SplitCustomerItemLines splits a customer's WhatsApp order list (up to 25-35 items) into individual item chunks.
func SplitCustomerItemLines(msg string) []string {
	s := strings.TrimSpace(msg)
	if s == "" {
		return nil
	}
	// Turn inline numbered markers ("1. Atta 2. Chini 3. Tel") into newline splits
	s = numberedSplitRegex.ReplaceAllString(s, "\n")

	rawParts := itemSplitLineRegex.Split(s, -1)
	var chunks []string
	for _, part := range rawParts {
		subParts := conjunctionSplitRe.Split(part, -1)
		for _, sub := range subParts {
			clean := bareNumberPrefixRe.ReplaceAllString(strings.TrimSpace(sub), "")
			clean = multiSpaceRe.ReplaceAllString(strings.TrimSpace(clean), " ")
			if len(clean) < 2 {
				continue
			}
			toks, _ := extractSearchTokensAndHint(clean)
			if len(toks) == 0 {
				continue
			}
			chunks = append(chunks, clean)
			if len(chunks) >= 35 {
				return chunks
			}
		}
	}
	return chunks
}

// isPureCheckoutOrProfileMessage returns true when the customer has an active cart and is ONLY
// giving checkout/payment/address/confirmation details (so injecting bulk inventory wastes tokens).
func isPureCheckoutOrProfileMessage(msg string, hasCart bool) bool {
	if !hasCart {
		return false
	}
	toks := tokenizeClean(msg)
	if len(toks) == 0 {
		return true
	}
	nonCheckout := 0
	for _, t := range toks {
		if checkoutOnlyWords[t] || fillerOrderWords[t] || ragStopWords[t] {
			continue
		}
		// Digits (house number, pincode, phone number) count as checkout/address info
		allDigits := true
		for i := 0; i < len(t); i++ {
			if t[i] < '0' || t[i] > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			continue
		}
		nonCheckout++
	}
	return nonCheckout == 0
}

type scoredIndexedProd struct {
	ip    indexedProduct
	score int
}

var voiceMarkerRegex = regexp.MustCompile(`(?i)^\s*(?:🎙️\s*)?\[(?:customer\s+)?voice\s+note\]:?\s*`)

// stripVoiceNoteMarkers ensures voice transcripts ("🎙️ [Voice Note]: atta 5kg") never
// inject noise words ("customer", "voice", "note") into smart product matching or checkout detection.
func stripVoiceNoteMarkers(s string) string {
	return strings.TrimSpace(voiceMarkerRegex.ReplaceAllString(strings.TrimSpace(s), ""))
}

// BuildSmartCatalogContext builds a token-optimal, 500-product-ready catalog context using:
//   - Layer 1: Hinglish/Hindi/English synonyms + phonetic + fuzzy matching
//   - Layer 2: Multi-item line splitter for 20-25 item orders
//   - Layer 3: Confidence grouping (CLEAR_MATCH / OPTIONS / UNMATCHED) + Checkout inventory skip
func BuildSmartCatalogContext(userMsg string, history []ChatMessage, cartProductIDs map[string]bool) string {
	userMsg = stripVoiceNoteMarkers(userMsg)
	cleanHistory := make([]ChatMessage, 0, len(history))
	for _, h := range history {
		cleanHistory = append(cleanHistory, ChatMessage{Role: h.Role, Content: stripVoiceNoteMarkers(h.Content)})
	}
	history = cleanHistory

	products := ramstore.Global.ListProducts(true)
	if len(products) == 0 {
		return "INV: No in-stock items loaded yet."
	}

	// Build 1-line category directory (~25 tokens even for 500 products)
	catCounts := make(map[string]int)
	var catOrder []string
	catBuckets := make(map[string][]ramstore.Product)
	for _, p := range products {
		c := strings.TrimSpace(p.Category)
		if c == "" {
			c = "General"
		}
		if _, exists := catCounts[c]; !exists {
			catOrder = append(catOrder, c)
		}
		catCounts[c]++
		catBuckets[c] = append(catBuckets[c], p)
	}
	catSummary := make([]string, 0, len(catOrder))
	for _, c := range catOrder {
		catSummary = append(catSummary, fmt.Sprintf("%s(%d)", c, catCounts[c]))
	}

	var sb strings.Builder
	sb.WriteString("CATS: " + strings.Join(catSummary, ", ") + "\n")

	// Small catalog (<= 18 items): send all compactly (~250 tokens)
	if len(products) <= 18 {
		sb.WriteString("INV(id|name|cat|price|vars):\n")
		for _, p := range products {
			sb.WriteString(formatCompactProductLine(p) + "\n")
		}
		return strings.TrimSpace(sb.String())
	}

	// Collect active cart products (always retained so AI can update/remove cart items accurately)
	cartProds := make([]ramstore.Product, 0, len(cartProductIDs))
	if len(cartProductIDs) > 0 {
		for _, p := range products {
			if cartProductIDs[p.ID] {
				cartProds = append(cartProds, p)
			}
		}
	}

	// Optimization: On pure checkout/address/COD/Online confirmation turns with active cart,
	// skip bulk inventory injection (saves ~300-500 tokens per checkout turn!).
	if isPureCheckoutOrProfileMessage(userMsg, len(cartProds) > 0) {
		sb.WriteString("INV(checkout mode — cart items only):\n")
		for _, p := range cartProds {
			sb.WriteString(formatCompactProductLine(p) + "\n")
		}
		return strings.TrimSpace(sb.String())
	}

	indexed := buildIndexedProducts(products)

	// Layer 2 & 3: Check if customer sent a multi-item order list (2 to 35 items in one message!)
	itemChunks := SplitCustomerItemLines(userMsg)
	if len(itemChunks) >= 2 {
		var clearLines []string
		var optionLines []string
		var unmatched []string
		includedIDs := make(map[string]bool)
		totalProductLines := 0
		const maxMultiItemProducts = 42

		for _, chunk := range itemChunks {
			toks, unitHint := extractSearchTokensAndHint(chunk)
			if len(toks) == 0 {
				continue
			}
			var scored []scoredIndexedProd
			for _, ip := range indexed {
				sc := scoreIndexedProduct(ip, chunk, toks, unitHint)
				if sc >= 6 {
					scored = append(scored, scoredIndexedProd{ip: ip, score: sc})
				}
			}
			sort.SliceStable(scored, func(i, j int) bool {
				return scored[i].score > scored[j].score
			})

			if len(scored) == 0 {
				unmatched = append(unmatched, chunk)
				continue
			}

			// High-confidence single clear winner vs ambiguous variety choices
			top := scored[0]
			isClearSingle := top.score >= 12 && (len(scored) == 1 || top.score >= scored[1].score+6)
			if isClearSingle {
				clearLines = append(clearLines, fmt.Sprintf("%s => %s", chunk, formatCompactProductLine(top.ip.prod)))
				if !includedIDs[top.ip.prod.ID] {
					includedIDs[top.ip.prod.ID] = true
					totalProductLines++
				}
			} else {
				limit := 2
				if len(scored) >= 3 && scored[2].score >= top.score-2 && totalProductLines < maxMultiItemProducts-2 {
					limit = 3
				}
				if limit > len(scored) {
					limit = len(scored)
				}
				opts := make([]string, 0, limit)
				for k := 0; k < limit; k++ {
					if totalProductLines >= maxMultiItemProducts && !includedIDs[scored[k].ip.prod.ID] {
						break
					}
					opts = append(opts, formatCompactProductLine(scored[k].ip.prod))
					if !includedIDs[scored[k].ip.prod.ID] {
						includedIDs[scored[k].ip.prod.ID] = true
						totalProductLines++
					}
				}
				if len(opts) > 0 {
					optionLines = append(optionLines, fmt.Sprintf("%s => %s", chunk, strings.Join(opts, " OR ")))
				}
			}
		}

		// Only use multi-item formatted view if at least one chunk matched (otherwise fall through to single-query/fallback)
		if len(clearLines) > 0 || len(optionLines) > 0 {
			sb.WriteString(fmt.Sprintf("MULTI_ITEM_MATCH(%d lines matched from %d in-stock):\n", len(clearLines)+len(optionLines), len(products)))
			if len(clearLines) > 0 {
				sb.WriteString("CLEAR_MATCH(ready to add):\n")
				for _, l := range clearLines {
					sb.WriteString("• " + l + "\n")
				}
			}
			if len(optionLines) > 0 {
				sb.WriteString("VARIETY_OPTIONS(ask/pick best):\n")
				for _, l := range optionLines {
					sb.WriteString("• " + l + "\n")
				}
			}
			if len(unmatched) > 0 {
				sb.WriteString("UNMATCHED(use search_catalog if indirect need, else inform): " + strings.Join(unmatched, ", ") + "\n")
			}
			// Append any active cart items not already shown
			var extraCart []string
			for _, cp := range cartProds {
				if !includedIDs[cp.ID] {
					extraCart = append(extraCart, formatCompactProductLine(cp))
				}
			}
			if len(extraCart) > 0 {
				sb.WriteString("IN_CART_REF:\n" + strings.Join(extraCart, "\n") + "\n")
			}
			return strings.TrimSpace(sb.String())
		}
	}

	// Single-Query Mode (current message + last 2 user messages) with Synonym + Phonetic + Fuzzy scoring
	var queryParts []string
	queryParts = append(queryParts, userMsg)
	addedPrev := 0
	for i := len(history) - 2; i >= 0 && addedPrev < 2; i-- {
		if history[i].Role == "user" {
			queryParts = append(queryParts, history[i].Content)
			addedPrev++
		}
	}
	combinedQuery := strings.Join(queryParts, " ")
	tokens, unitHint := extractSearchTokensAndHint(combinedQuery)

	selectedIDs := make(map[string]bool)
	selected := make([]ramstore.Product, 0, 14)

	for _, cp := range cartProds {
		if !selectedIDs[cp.ID] {
			selectedIDs[cp.ID] = true
			selected = append(selected, cp)
		}
	}

	var scored []scoredIndexedProd
	if len(tokens) > 0 {
		for _, ip := range indexed {
			if selectedIDs[ip.prod.ID] {
				continue
			}
			sc := scoreIndexedProduct(ip, combinedQuery, tokens, unitHint)
			if sc >= 5 {
				scored = append(scored, scoredIndexedProd{ip: ip, score: sc})
			}
		}
		sort.SliceStable(scored, func(i, j int) bool {
			return scored[i].score > scored[j].score
		})
	}

	const maxSingleQueryItems = 14
	for _, sp := range scored {
		if len(selected) >= maxSingleQueryItems {
			break
		}
		selectedIDs[sp.ip.prod.ID] = true
		selected = append(selected, sp.ip.prod)
	}

	// If zero/very few matches (e.g. greeting, "menu dikhao", or indirect need like "guest aa rahe hain"),
	// include a compact diverse sample across categories + hint for on-demand `search_catalog` hand!
	if len(selected) < 6 {
		maxShow := 8
		round := 0
		for len(selected) < maxShow {
			addedAny := false
			for _, c := range catOrder {
				bucket := catBuckets[c]
				if round < len(bucket) {
					p := bucket[round]
					if !selectedIDs[p.ID] {
						selectedIDs[p.ID] = true
						selected = append(selected, p)
						addedAny = true
						if len(selected) >= maxShow {
							break
						}
					}
				}
			}
			if !addedAny {
				break
			}
			round++
		}
	}

	sb.WriteString(fmt.Sprintf("INV(showing %d/%d relevant in-stock; id|name|cat|price|vars):\n", len(selected), len(products)))
	for _, p := range selected {
		sb.WriteString(formatCompactProductLine(p) + "\n")
	}
	if len(scored) == 0 && len(tokens) > 0 {
		sb.WriteString("HINT: 0 direct keyword hits. If customer described a need/use-case, call search_catalog hand with product keywords or categories from CATS.")
	}
	return strings.TrimSpace(sb.String())
}

// ExecuteSearchCatalogHand executes Layer 4's on-demand `search_catalog` hand strictly in local RAM (0 tokens, <2ms),
// returning up to 15 compact matching in-stock items for the AI's 2nd-pass final reply.
func ExecuteSearchCatalogHand(queries []string, categories []string) string {
	products := ramstore.Global.ListProducts(true)
	if len(products) == 0 {
		return "SEARCH_RESULTS: No in-stock products available."
	}

	indexed := buildIndexedProducts(products)

	cleanCats := make([]string, 0, len(categories))
	for _, c := range categories {
		c = strings.ToLower(strings.TrimSpace(c))
		if c != "" {
			cleanCats = append(cleanCats, c)
		}
	}

	combinedQ := strings.Join(queries, " ")
	tokens, unitHint := extractSearchTokensAndHint(combinedQ)

	type scoredRes struct {
		prod  ramstore.Product
		score int
	}
	var results []scoredRes
	for _, ip := range indexed {
		sc := 0
		if len(tokens) > 0 {
			sc += scoreIndexedProduct(ip, combinedQ, tokens, unitHint)
		}
		for _, wantCat := range cleanCats {
			if ip.catLow == wantCat || strings.Contains(ip.catLow, wantCat) || strings.Contains(wantCat, ip.catLow) {
				sc += 12
			}
		}
		if sc > 0 {
			results = append(results, scoredRes{prod: ip.prod, score: sc})
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	const maxLookupItems = 15
	if len(results) > maxLookupItems {
		results = results[:maxLookupItems]
	}

	if len(results) == 0 {
		return fmt.Sprintf("SEARCH_RESULTS(0 matches for queries=%v categories=%v): None of those exact items are currently in stock. Politely tell the customer and suggest available categories from CATS.", queries, categories)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("SEARCH_RESULTS(%d in-stock matches; id|name|cat|price|vars):\n", len(results)))
	for _, r := range results {
		sb.WriteString(formatCompactProductLine(r.prod) + "\n")
	}
	return strings.TrimSpace(sb.String())
}
