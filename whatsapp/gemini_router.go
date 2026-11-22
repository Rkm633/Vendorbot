package whatsapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"wa-server/ramstore"
)

// =============================================================================
// GEMINI 4-MODEL POOL + DUAL GROQ<->GEMINI ROTATION + GEMINI DECISION 1 ROUTER
//
// 1. Gemini-Only Mode: Strictly uses ONLY these 4 models across all Gemini keys:
//    - gemma-4-31b-it          (Gemma 4 31B)
//    - gemma-4-26b-a4b-it      (Gemma 4 26B)
//    - gemini-3.1-flash-lite   (Gemini 3.1 Flash Lite)
//    - gemini-3.5-flash-lite   (Gemini 3.5 Flash Lite)
//    + gemini-decision-1 strictly for smart routing (token saving & precision).
// 2. Groq-Only Mode: Keeps existing 3-model Groq pool untouched (zero Gemini calls).
// 3. Dual Mode (Groq + Gemini both configured): Alternates ("bari-bari") between
//    Groq and Gemini across all active keys × allowed models, with instant failover
//    on TPM/RPM/TPD/429 + gemini-decision-1 smart routing enabled.
// =============================================================================

var geminiAllowedModels = []string{
	"gemma-4-31b-it",
	"gemma-4-26b-a4b-it",
	"gemini-3.1-flash-lite",
	"gemini-3.5-flash-lite",
}

const GeminiDecisionModel = "gemini-decision-1"

type geminiSlotState struct {
	rpmCooldownUntil  time.Time
	rpdCooldownUntil  time.Time
	authDisabledUntil time.Time
	tokensUsedToday   int64
	requestsToday     int64
	dayUTC            string
}

type geminiCandidateSlot struct {
	keyIndex  int
	apiKey    string
	keyHash   string
	model     string
	tokensDay int64
	rpmUntil  time.Time
}

type geminiPoolManager struct {
	mu     sync.Mutex
	slots  map[string]*geminiSlotState
	cursor uint64
}

var (
	geminiPool     = &geminiPoolManager{slots: make(map[string]*geminiSlotState)}
	dualTurnMu     sync.Mutex
	dualTurnCursor uint64

	decisionPoolMu    sync.Mutex
	decisionKeyStates = make(map[string]*geminiSlotState)
	decisionKeyCursor uint64
	decisionHTTP      = &http.Client{Timeout: 6 * time.Second}
)

// isGeminiProvider detects whether a provider/key pair targets Google Gemini API.
func isGeminiProvider(provider, apiKey string) bool {
	k := strings.TrimSpace(apiKey)
	if strings.HasPrefix(k, "AIza") {
		return true
	}
	if strings.HasPrefix(k, "gsk_") || strings.HasPrefix(k, "sk-or-") || strings.HasPrefix(k, "sk-") {
		return false
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	return p == "gemini" || p == "google" || p == "gemma"
}

func isGeminiModelAllowed(m string) bool {
	m = strings.ToLower(strings.TrimSpace(m))
	for _, allowed := range geminiAllowedModels {
		if m == allowed {
			return true
		}
	}
	return false
}

// GetActiveAIRoutingMode reports the active AI routing strategy for overview telemetry.
func GetActiveAIRoutingMode(groqCount, geminiCount int) string {
	if groqCount > 0 && geminiCount > 0 {
		return "dual_rotation"
	}
	if geminiCount > 0 {
		return "gemini_only"
	}
	if groqCount > 0 {
		return "groq_only"
	}
	return "other"
}

// ClassifyConfiguredAIKeys partitions active vendor + env keys into Groq, Gemini, and other providers.
func ClassifyConfiguredAIKeys(username string, cfg *UserConfigRAM) (groqKeys []string, geminiKeys []string, otherKeys []string) {
	keys := collectCandidateAPIKeys(username, cfg)
	provider := ""
	if cfg != nil {
		provider = cfg.Provider
	}
	for _, k := range keys {
		if isGroqProvider(provider, k) {
			groqKeys = append(groqKeys, k)
		} else if isGeminiProvider(provider, k) {
			geminiKeys = append(geminiKeys, k)
		} else {
			otherKeys = append(otherKeys, k)
		}
	}
	return groqKeys, geminiKeys, otherKeys
}

func (pm *geminiPoolManager) getSlotLocked(kHash, model string, now time.Time) *geminiSlotState {
	day := now.UTC().Format("2006-01-02")
	id := kHash + "|" + model
	st, ok := pm.slots[id]
	if !ok || st == nil {
		st = &geminiSlotState{dayUTC: day}
		pm.slots[id] = st
		return st
	}
	if st.dayUTC != day {
		st.dayUTC = day
		st.tokensUsedToday = 0
		st.requestsToday = 0
		st.rpdCooldownUntil = time.Time{}
	}
	return st
}

// buildCandidates constructs the (active Gemini keys × 4 allowed models) rotation matrix.
func (pm *geminiPoolManager) buildCandidates(geminiKeys []string) (ready []geminiCandidateSlot, rpmDelayed []geminiCandidateSlot) {
	now := time.Now()
	pm.mu.Lock()
	defer pm.mu.Unlock()

	total := len(geminiKeys) * len(geminiAllowedModels)
	if total == 0 {
		return nil, nil
	}

	matrix := make([]geminiCandidateSlot, 0, total)
	for _, model := range geminiAllowedModels {
		for kIdx, key := range geminiKeys {
			matrix = append(matrix, geminiCandidateSlot{
				keyIndex: kIdx,
				apiKey:   key,
				keyHash:  shortKeyHash(key),
				model:    model,
			})
		}
	}

	start := int(pm.cursor % uint64(total))
	pm.cursor++

	rotated := make([]geminiCandidateSlot, 0, total)
	rotated = append(rotated, matrix[start:]...)
	rotated = append(rotated, matrix[:start]...)

	for _, slot := range rotated {
		st := pm.getSlotLocked(slot.keyHash, slot.model, now)
		if now.Before(st.authDisabledUntil) || now.Before(st.rpdCooldownUntil) {
			continue
		}
		slot.tokensDay = st.tokensUsedToday
		slot.rpmUntil = st.rpmCooldownUntil
		if now.Before(st.rpmCooldownUntil) {
			rpmDelayed = append(rpmDelayed, slot)
		} else {
			ready = append(ready, slot)
		}
	}

	// Balance daily usage across all 4 Gemini/Gemma models × all active keys
	sort.SliceStable(ready, func(i, j int) bool {
		return (ready[i].tokensDay / 5000) < (ready[j].tokensDay / 5000)
	})
	sort.SliceStable(rpmDelayed, func(i, j int) bool {
		return rpmDelayed[i].rpmUntil.Before(rpmDelayed[j].rpmUntil)
	})
	return ready, rpmDelayed
}

func (pm *geminiPoolManager) recordSuccess(kHash, model string, totalTokens int64) {
	now := time.Now()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	st := pm.getSlotLocked(kHash, model, now)
	st.tokensUsedToday += totalTokens
	st.requestsToday++
	st.rpmCooldownUntil = time.Time{}
}

func (pm *geminiPoolManager) markRPMCooldown(kHash, model string, dur time.Duration) {
	if dur <= 0 {
		dur = 65 * time.Second
	}
	now := time.Now()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	st := pm.getSlotLocked(kHash, model, now)
	st.rpmCooldownUntil = now.Add(dur)
}

func (pm *geminiPoolManager) markRPDExhausted(kHash, model string, dur time.Duration) {
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
	st.rpdCooldownUntil = until
}

func (pm *geminiPoolManager) markAuthDisabledForKey(kHash string, dur time.Duration) {
	if dur <= 0 {
		dur = 30 * time.Minute
	}
	now := time.Now()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, mdl := range geminiAllowedModels {
		st := pm.getSlotLocked(kHash, mdl, now)
		st.authDisabledUntil = now.Add(dur)
	}
}

func classifyGemini429(headers http.Header, body []byte) (isDaily bool, cooldown time.Duration) {
	lower := strings.ToLower(string(body))
	retryAfter := parseResetDuration(headers.Get("Retry-After"))
	if strings.Contains(lower, "per day") ||
		strings.Contains(lower, "perday") ||
		strings.Contains(lower, "rpd") ||
		strings.Contains(lower, "tpd") ||
		strings.Contains(lower, "daily") ||
		retryAfter >= 5*time.Minute {
		cd := retryAfter
		if cd < 30*time.Minute {
			cd = 2 * time.Hour
		}
		return true, cd
	}
	cd := retryAfter
	if cd <= 0 {
		cd = 65 * time.Second
	} else {
		cd += 2 * time.Second
	}
	return false, cd
}

// callGeminiSingleModel calls one of the 4 allowed Gemini/Gemma models via v1beta generateContent.
// Automatically retries once without system_instruction/response_mime_type if a Gemma model returns HTTP 400.
func callGeminiSingleModel(apiKey, model string, temperature float64, sysPrompt string, history []ChatMessage, jsonMode bool) (string, int64, int, http.Header, []byte, error) {
	endpoint := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)
	isGemma := strings.HasPrefix(strings.ToLower(model), "gemma-")

	buildContents := func(foldSystem bool) []map[string]interface{} {
		var contents []map[string]interface{}
		firstUserFolded := false
		for _, m := range history {
			role := "user"
			if m.Role == "assistant" || m.Role == "outgoing" || m.Role == "outgoing_ai" {
				role = "model"
			}
			txt := strings.TrimSpace(m.Content)
			if txt == "" {
				continue
			}
			if foldSystem && !firstUserFolded && role == "user" && strings.TrimSpace(sysPrompt) != "" {
				txt = "[INSTRUCTIONS]\n" + sysPrompt + "\n\n[CUSTOMER MESSAGE]\n" + txt
				firstUserFolded = true
			}
			contents = append(contents, map[string]interface{}{
				"role":  role,
				"parts": []map[string]string{{"text": txt}},
			})
		}
		if foldSystem && !firstUserFolded && strings.TrimSpace(sysPrompt) != "" {
			contents = append([]map[string]interface{}{{
				"role":  "user",
				"parts": []map[string]string{{"text": "[INSTRUCTIONS]\n" + sysPrompt}},
			}}, contents...)
		}
		return contents
	}

	doRequest := func(minimal bool) (string, int64, int, http.Header, []byte, error) {
		foldSys := minimal || isGemma
		contents := buildContents(foldSys)
		if len(contents) == 0 {
			return "", 0, 0, nil, nil, fmt.Errorf("empty chat history for Gemini")
		}
		maxOut := 600
		if !jsonMode {
			maxOut = 320
		}
		genCfg := map[string]interface{}{
			"temperature":     temperature,
			"maxOutputTokens": maxOut,
		}
		if jsonMode && !minimal && !isGemma {
			genCfg["response_mime_type"] = "application/json"
		}
		payload := map[string]interface{}{
			"contents":         contents,
			"generationConfig": genCfg,
		}
		if !foldSys && strings.TrimSpace(sysPrompt) != "" {
			payload["system_instruction"] = map[string]interface{}{
				"parts": []map[string]string{{"text": sysPrompt}},
			}
		}

		bodyBytes, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(bodyBytes))
		if err != nil {
			return "", 0, 0, nil, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "VendorWhatsAppBotNode/2.6")

		client := &http.Client{Timeout: 18 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return "", 0, 0, nil, nil, err
		}
		respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
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
				UsageMetadata struct {
					TotalTokenCount int64 `json:"totalTokenCount"`
				} `json:"usageMetadata"`
			}
			if err := json.Unmarshal(respBytes, &result); err == nil && len(result.Candidates) > 0 && len(result.Candidates[0].Content.Parts) > 0 {
				var sb strings.Builder
				for _, pt := range result.Candidates[0].Content.Parts {
					sb.WriteString(pt.Text)
				}
				content := stripThinkBlocks(sb.String())
				if content != "" {
					return content, result.UsageMetadata.TotalTokenCount, resp.StatusCode, resp.Header, respBytes, nil
				}
			}
			return "", result.UsageMetadata.TotalTokenCount, resp.StatusCode, resp.Header, respBytes, fmt.Errorf("empty content from gemini model %s", model)
		}
		return "", 0, resp.StatusCode, resp.Header, respBytes, fmt.Errorf("gemini model %s returned status %d", model, resp.StatusCode)
	}

	text, tokens, status, headers, body, err := doRequest(false)
	if err != nil && status == http.StatusBadRequest {
		return doRequest(true)
	}
	return text, tokens, status, headers, body, err
}

func tryExecuteGeminiSlot(slot geminiCandidateSlot, temperature float64, sysPrompt string, history []ChatMessage, jsonMode bool) (string, bool, error) {
	text, tokens, status, headers, body, err := callGeminiSingleModel(slot.apiKey, slot.model, temperature, sysPrompt, history, jsonMode)
	if err == nil && strings.TrimSpace(text) != "" {
		geminiPool.recordSuccess(slot.keyHash, slot.model, tokens)
		return text, true, nil
	}

	switch status {
	case http.StatusTooManyRequests:
		isDaily, cd := classifyGemini429(headers, body)
		if isDaily {
			geminiPool.markRPDExhausted(slot.keyHash, slot.model, cd)
			log.Printf("🚫 [Gemini RPD/TPD Exhausted] Key #%d | model=%s (cooldown %v) -> switching model/key", slot.keyIndex+1, slot.model, cd.Round(time.Minute))
		} else {
			geminiPool.markRPMCooldown(slot.keyHash, slot.model, cd)
			log.Printf("⚡ [Gemini RPM/TPM Limit] Key #%d | model=%s (cooldown %v) -> switching model/key immediately", slot.keyIndex+1, slot.model, cd.Round(time.Second))
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		geminiPool.markAuthDisabledForKey(slot.keyHash, 30*time.Minute)
		log.Printf("🔑 [Gemini Auth Error %d] Key #%d disabled 30m -> switching to next key", status, slot.keyIndex+1)
	default:
		geminiPool.markRPMCooldown(slot.keyHash, slot.model, 45*time.Second)
		log.Printf("⚠️ [Gemini Error] Key #%d | model=%s (%v) -> switching model/key", slot.keyIndex+1, slot.model, err)
	}
	return "", false, err
}

// callGeminiPoolWithHistory strictly uses ONLY the 4 allowed Gemini/Gemma models across all active Gemini keys.
func callGeminiPoolWithHistory(geminiKeys []string, temperature float64, sysPrompt string, history []ChatMessage, jsonMode bool) (string, error) {
	if len(geminiKeys) == 0 {
		return "", fmt.Errorf("no Gemini API keys available")
	}
	ready, rpmDelayed := geminiPool.buildCandidates(geminiKeys)

	var lastErr error
	for _, slot := range ready {
		if text, ok, err := tryExecuteGeminiSlot(slot, temperature, sysPrompt, history, jsonMode); ok {
			return text, nil
		} else {
			lastErr = err
		}
	}

	if len(rpmDelayed) > 0 {
		best := rpmDelayed[0]
		wait := time.Until(best.rpmUntil)
		if wait > 0 && wait <= 3*time.Second {
			time.Sleep(wait)
		}
		if text, ok, err := tryExecuteGeminiSlot(best, temperature, sysPrompt, history, jsonMode); ok {
			return text, nil
		} else {
			lastErr = err
		}
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("all Gemini keys and models (gemma-4-31b-it, gemma-4-26b-a4b-it, gemini-3.1-flash-lite, gemini-3.5-flash-lite) are currently rate-limited or exhausted")
}

// callDualGroqGeminiPoolWithHistory alternates ("bari-bari") between Groq (3 models) and Gemini (4 models)
// across all active keys on every turn, with instant cross-provider & cross-model failover on TPM/RPM/TPD/429.
func callDualGroqGeminiPoolWithHistory(groqKeys, geminiKeys []string, temperature float64, sysPrompt string, history []ChatMessage, jsonMode bool) (string, error) {
	groqReady, groqDelayed := groqPool.buildCandidates(groqKeys)
	geminiReady, geminiDelayed := geminiPool.buildCandidates(geminiKeys)

	dualTurnMu.Lock()
	startWithGemini := (dualTurnCursor % 2) == 1
	dualTurnCursor++
	dualTurnMu.Unlock()

	type dualStep struct {
		isGemini   bool
		groqSlot   groqCandidateSlot
		geminiSlot geminiCandidateSlot
	}

	interleave := func(gList []groqCandidateSlot, mList []geminiCandidateSlot, geminiFirst bool) []dualStep {
		out := make([]dualStep, 0, len(gList)+len(mList))
		gi, mi := 0, 0
		turnGemini := geminiFirst
		for gi < len(gList) || mi < len(mList) {
			if turnGemini {
				if mi < len(mList) {
					out = append(out, dualStep{isGemini: true, geminiSlot: mList[mi]})
					mi++
				} else if gi < len(gList) {
					out = append(out, dualStep{isGemini: false, groqSlot: gList[gi]})
					gi++
				}
			} else {
				if gi < len(gList) {
					out = append(out, dualStep{isGemini: false, groqSlot: gList[gi]})
					gi++
				} else if mi < len(mList) {
					out = append(out, dualStep{isGemini: true, geminiSlot: mList[mi]})
					mi++
				}
			}
			turnGemini = !turnGemini
		}
		return out
	}

	messages := buildOpenAIMessages(sysPrompt, history)
	tryGroq := func(slot groqCandidateSlot) (string, bool, error) {
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
		switch status {
		case http.StatusTooManyRequests:
			isTPD, cd := classifyGroq429(headers, body)
			if isTPD {
				groqPool.markTPDExhausted(slot.keyHash, slot.model, cd)
				log.Printf("🚫 [Dual Rotation | Groq TPD] Key #%d | %s -> rotating to next provider/model", slot.keyIndex+1, slot.model)
			} else {
				groqPool.markTPMCooldown(slot.keyHash, slot.model, cd)
				log.Printf("⚡ [Dual Rotation | Groq TPM] Key #%d | %s -> rotating to next provider/model", slot.keyIndex+1, slot.model)
			}
		case http.StatusUnauthorized, http.StatusForbidden:
			groqPool.markAuthDisabledForKey(slot.keyHash, 30*time.Minute)
		default:
			groqPool.markTPMCooldown(slot.keyHash, slot.model, 45*time.Second)
		}
		return "", false, err
	}

	var lastErr error
	readyQueue := interleave(groqReady, geminiReady, startWithGemini)
	for _, step := range readyQueue {
		if step.isGemini {
			if text, ok, err := tryExecuteGeminiSlot(step.geminiSlot, temperature, sysPrompt, history, jsonMode); ok {
				log.Printf("🔀 [Dual Rotation] Served by Gemini Key #%d | %s", step.geminiSlot.keyIndex+1, step.geminiSlot.model)
				return text, nil
			} else {
				lastErr = err
			}
		} else {
			if text, ok, err := tryGroq(step.groqSlot); ok {
				log.Printf("🔀 [Dual Rotation] Served by Groq Key #%d | %s", step.groqSlot.keyIndex+1, step.groqSlot.model)
				return text, nil
			} else {
				lastErr = err
			}
		}
	}

	// If every ready slot was temporarily cooled down, try the soonest-expiring delayed slot across both providers
	delayedQueue := interleave(groqDelayed, geminiDelayed, startWithGemini)
	if len(delayedQueue) > 0 {
		first := delayedQueue[0]
		if first.isGemini {
			wait := time.Until(first.geminiSlot.rpmUntil)
			if wait > 0 && wait <= 3*time.Second {
				time.Sleep(wait)
			}
			if text, ok, err := tryExecuteGeminiSlot(first.geminiSlot, temperature, sysPrompt, history, jsonMode); ok {
				return text, nil
			} else {
				lastErr = err
			}
		} else {
			wait := time.Until(first.groqSlot.tpmUntil)
			if wait > 0 && wait <= 3*time.Second {
				time.Sleep(wait)
			}
			if text, ok, err := tryGroq(first.groqSlot); ok {
				return text, nil
			} else {
				lastErr = err
			}
		}
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("all Groq and Gemini slots are currently rate-limited or exhausted")
}

// =============================================================================
// GEMINI DECISION 1 — ZERO-OUTPUT-TOKEN SMART ROUTER (Quota-Guarded)
// Used ONLY for smart routing (skipping catalog on non-shopping turns, matching
// vague Hinglish needs to exact store categories, precise quote_reply & human
// escalation) — NEVER generates text and NEVER blocks chat if quota-limited.
// =============================================================================

type DecisionRouteResult struct {
	UsedDecisionAPI bool
	Intent          string
	SkipCatalog     bool
	NeedsQuoteReply bool
	NeedsEscalation bool
	MatchedCategory string
}

var trivialAckWords = map[string]bool{
	"ok": true, "okay": true, "k": true, "kk": true, "ji": true, "hmm": true,
	"hm": true, "thk": true, "thik": true, "theek": true, "thanks": true,
	"thank": true, "thankyou": true, "dhanyawad": true, "shukriya": true,
	"haan": true, "han": true, "yes": true, "no": true, "nahi": true,
	"cod": true, "online": true, "upi": true, "confirm": true,
}

// shouldSkipDecisionRouter saves gemini-decision-1 RPD/RPM quota on trivial 1-word messages.
func shouldSkipDecisionRouter(incomingText string) bool {
	s := strings.ToLower(strings.TrimSpace(incomingText))
	if s == "" || len([]rune(s)) <= 2 {
		return true
	}
	words := strings.Fields(s)
	if len(words) == 1 && trivialAckWords[strings.Trim(words[0], ".,!?🙏👍✅😊")] {
		return true
	}
	return false
}

func getDecisionKeyStateLocked(kHash string, now time.Time) *geminiSlotState {
	day := now.UTC().Format("2006-01-02")
	st, ok := decisionKeyStates[kHash]
	if !ok || st == nil {
		st = &geminiSlotState{dayUTC: day}
		decisionKeyStates[kHash] = st
		return st
	}
	if st.dayUTC != day {
		st.dayUTC = day
		st.tokensUsedToday = 0
		st.requestsToday = 0
		st.rpdCooldownUntil = time.Time{}
	}
	return st
}

func collectStoreCategoriesForDecision() []string {
	products := ramstore.Global.ListProducts(true)
	seen := make(map[string]bool)
	var cats []string
	for _, p := range products {
		c := strings.TrimSpace(p.Category)
		if c == "" {
			continue
		}
		key := strings.ToLower(c)
		if !seen[key] {
			seen[key] = true
			cats = append(cats, c)
		}
	}
	sort.Strings(cats)
	return cats
}

// RunGeminiDecisionSmartRoute calls Gemini Decisions API (v1beta/decisions, model: "gemini-decision-1",
// 0 output tokens) strictly for smart routing when Gemini key(s) are configured.
func RunGeminiDecisionSmartRoute(geminiKeys []string, incomingText string, cartActive bool) DecisionRouteResult {
	res := DecisionRouteResult{}
	if len(geminiKeys) == 0 || shouldSkipDecisionRouter(incomingText) {
		return res
	}

	// Pick an available (non-cooled-down) Gemini key in round-robin order
	now := time.Now()
	decisionPoolMu.Lock()
	start := int(decisionKeyCursor % uint64(len(geminiKeys)))
	decisionKeyCursor++
	var candidates []string
	for i := 0; i < len(geminiKeys); i++ {
		k := geminiKeys[(start+i)%len(geminiKeys)]
		st := getDecisionKeyStateLocked(shortKeyHash(k), now)
		if now.Before(st.authDisabledUntil) || now.Before(st.rpdCooldownUntil) || now.Before(st.rpmCooldownUntil) {
			continue
		}
		candidates = append(candidates, k)
	}
	decisionPoolMu.Unlock()

	if len(candidates) == 0 {
		return res
	}

	// Build single batched Decisions API request (1 call = 0 output tokens!)
	questions := map[string]interface{}{
		"route_intent": map[string]interface{}{
			"type":         "choice",
			"instructions": "Classify the WhatsApp store customer's primary intent in this message.",
			"criteria": map[string]string{
				"greeting_menu":        "Simple greeting like hi/hello/namaste or asking what the store sells.",
				"product_shopping":     "Asking about products, prices, stock, photos, recommendations, or adding/editing cart items.",
				"checkout_address":     "Sharing delivery name/address/phone, confirming order, or selecting COD/Online payment.",
				"order_payment_status": "Asking about order status, delivery ETA, or saying payment is done.",
				"complaint_human":      "Angry complaint, refund/damaged item issue, or asking to speak with a human/owner.",
				"chitchat_other":       "Off-topic chitchat or anything outside these categories.",
			},
		},
		"needs_catalog": map[string]interface{}{
			"type":         "noul",
			"instructions": "Does answering this customer message require looking up store products, varieties, or prices?",
			"criteria": map[string]string{
				"true_description":  "Customer wants to browse, check price/availability, add/change products, or needs product suggestions.",
				"false_description": "Customer is only greeting briefly, giving address/phone, paying, checking order status, complaining, or saying thanks.",
			},
		},
		"needs_quote_reply": map[string]interface{}{
			"type":         "noul",
			"instructions": "Is the customer asking a specific question, price check, item availability, address question, or complaint where WhatsApp quote-reply adds clarity?",
			"criteria": map[string]string{
				"true_description":  "Specific question, price/stock inquiry, multi-part request, or complaint.",
				"false_description": "Simple greeting, short acknowledgement, or normal smooth continuation.",
			},
		},
		"needs_escalation": map[string]interface{}{
			"type":         "noul",
			"instructions": "Is the customer angry, reporting a damaged/wrong delivery, demanding a refund, or explicitly asking to talk to the store owner or human agent?",
			"criteria": map[string]string{
				"true_description":  "Clear anger, defect/damage complaint, refund dispute, or explicit human/owner request.",
				"false_description": "Normal polite shopping, price inquiry, cart update, or checkout.",
			},
		},
	}

	// Add category matcher ONLY when store has 2..240 categories (great for 500-product vague Hinglish queries!)
	storeCats := collectStoreCategoriesForDecision()
	catKeyToName := make(map[string]string)
	if len(storeCats) >= 2 && len(storeCats) <= 240 {
		catCriteria := map[string]string{
			"z_other": "No specific store category matches this message.",
		}
		for i, cName := range storeCats {
			cKey := fmt.Sprintf("cat_%03d", i)
			catCriteria[cKey] = cName
			catKeyToName[cKey] = cName
		}
		questions["matched_category"] = map[string]interface{}{
			"type":         "choice",
			"instructions": "Which store product category best matches the customer's shopping need? Pick z_other if the customer is not asking for a product category.",
			"criteria":     catCriteria,
		}
	}

	cartLabel := "empty"
	if cartActive {
		cartLabel = "has_items"
	}
	inputText := fmt.Sprintf("cart=%s\ncustomer_message=%s", cartLabel, compactText(incomingText, 350))

	reqBody := map[string]interface{}{
		"model":     GeminiDecisionModel,
		"input":     inputText,
		"questions": questions,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return res
	}

	for _, apiKey := range candidates {
		kHash := shortKeyHash(apiKey)
		endpoint := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/decisions?key=%s", apiKey)
		req, rErr := http.NewRequest("POST", endpoint, bytes.NewBuffer(bodyBytes))
		if rErr != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "aistudio-build")

		resp, dErr := decisionHTTP.Do(req)
		if dErr != nil {
			decisionPoolMu.Lock()
			getDecisionKeyStateLocked(kHash, time.Now()).rpmCooldownUntil = time.Now().Add(30 * time.Second)
			decisionPoolMu.Unlock()
			continue
		}
		respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			isDaily, cd := classifyGemini429(resp.Header, respBytes)
			decisionPoolMu.Lock()
			st := getDecisionKeyStateLocked(kHash, time.Now())
			if isDaily {
				st.rpdCooldownUntil = time.Now().Add(cd)
			} else {
				st.rpmCooldownUntil = time.Now().Add(cd)
			}
			decisionPoolMu.Unlock()
			log.Printf("⚡ [Gemini Decision 1 Limit] Key cooled down (%v, daily=%v) — falling back gracefully", cd.Round(time.Second), isDaily)
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			decisionPoolMu.Lock()
			getDecisionKeyStateLocked(kHash, time.Now()).authDisabledUntil = time.Now().Add(30 * time.Minute)
			decisionPoolMu.Unlock()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			decisionPoolMu.Lock()
			getDecisionKeyStateLocked(kHash, time.Now()).rpmCooldownUntil = time.Now().Add(60 * time.Second)
			decisionPoolMu.Unlock()
			log.Printf("⚠️ [Gemini Decision 1] HTTP %d — bypassing smart router cleanly", resp.StatusCode)
			continue
		}

		var parsed struct {
			Answers map[string]struct {
				Type       string  `json:"type"`
				Noul       float64 `json:"noul"`
				Choice     string  `json:"choice"`
				Confidence float64 `json:"confidence"`
			} `json:"answers"`
		}
		if err := json.Unmarshal(respBytes, &parsed); err != nil || len(parsed.Answers) == 0 {
			continue
		}

		decisionPoolMu.Lock()
		getDecisionKeyStateLocked(kHash, time.Now()).requestsToday++
		decisionPoolMu.Unlock()

		res.UsedDecisionAPI = true
		if ans, ok := parsed.Answers["route_intent"]; ok && ans.Choice != "" {
			res.Intent = ans.Choice
		}
		if ans, ok := parsed.Answers["needs_catalog"]; ok {
			// Skip catalog ONLY when strongly confident customer is NOT shopping/browsing (saves 50-70% tokens!)
			if ans.Noul < 0.35 && res.Intent != "product_shopping" && res.Intent != "greeting_menu" {
				res.SkipCatalog = true
			}
		}
		if ans, ok := parsed.Answers["needs_quote_reply"]; ok && ans.Noul >= 0.72 {
			res.NeedsQuoteReply = true
		}
		if ans, ok := parsed.Answers["needs_escalation"]; ok && ans.Noul >= 0.85 {
			res.NeedsEscalation = true
		}
		if ans, ok := parsed.Answers["matched_category"]; ok && ans.Choice != "" && ans.Choice != "z_other" && ans.Confidence >= 0.60 {
			if realCat, exists := catKeyToName[ans.Choice]; exists {
				res.MatchedCategory = realCat
			}
		}

		log.Printf("🧭 [Gemini Decision 1 Router] intent=%s | skip_catalog=%v | quote=%v | escalate=%v | cat=%q (0 output tokens)",
			res.Intent, res.SkipCatalog, res.NeedsQuoteReply, res.NeedsEscalation, res.MatchedCategory)
		return res
	}

	return res
}
