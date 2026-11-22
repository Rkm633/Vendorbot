package whatsapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"wa-server/ramstore"
)

// =============================================================================
// OCTOPUS HAND: 3-MODE VOICE NOTE LISTENER & MULTI-PROVIDER ROTATION POOL
//
// 1. Gemini-Only Mode (Gemini keys configured, 0 Groq keys):
//    Rotates across:
//      - gemini-3.1-flash-live-preview              (Gemini 3 Flash Live)
//      - gemini-2.5-flash-native-audio-preview-12-2025 (Gemini 2.5 Flash Native Audio Dialog)
// 2. Groq-Only Mode (Groq keys configured, 0 Gemini keys):
//    Rotates across:
//      - whisper-large-v3-turbo
//      - whisper-large-v3
// 3. Dual Mode (BOTH Groq & Gemini keys configured):
//    Alternates turn-by-turn ("bari-bari") between Groq Whisper pool and Gemini
//    Live Audio pool across all active keys × models with instant cross-provider failover.
// =============================================================================

var (
	GroqVoiceModels = []string{
		"whisper-large-v3-turbo",
		"whisper-large-v3",
	}
	GeminiVoiceLiveModels = []string{
		"gemini-3.1-flash-live-preview",
		"gemini-2.5-flash-native-audio-preview-12-2025",
	}
)

type VoiceNoteHandResult struct {
	Hand        string
	Transcript  string
	CleanQuery  string
	RoutingMode string
	Provider    string
	Model       string
	KeyIndex    int
	DurationSec uint32
	Err         error
}

type voiceSlotState struct {
	shortCooldownUntil time.Time
	dailyCooldownUntil time.Time
	authDisabledUntil  time.Time
	requestsToday      int64
	dayUTC             string
}

var (
	voicePoolMu           sync.Mutex
	groqVoiceSlotStates   = make(map[string]*voiceSlotState)
	geminiVoiceSlotStates = make(map[string]*voiceSlotState)
	groqVoiceCursor       uint64
	geminiVoiceCursor     uint64
	dualVoiceTurnCursor   uint64
)

func getVoiceSlotLocked(m map[string]*voiceSlotState, kHash, model string, now time.Time) *voiceSlotState {
	day := now.UTC().Format("2006-01-02")
	id := kHash + "|" + model
	st, ok := m[id]
	if !ok || st == nil {
		st = &voiceSlotState{dayUTC: day}
		m[id] = st
		return st
	}
	if st.dayUTC != day {
		st.dayUTC = day
		st.requestsToday = 0
		st.dailyCooldownUntil = time.Time{}
	}
	return st
}

// GetActiveVoiceRoutingInfo reports active voice models & mode for telemetry.
func GetActiveVoiceRoutingInfo(groqCount, geminiCount int) (mode string, models []string) {
	if groqCount > 0 && geminiCount > 0 {
		out := make([]string, 0, len(GroqVoiceModels)+len(GeminiVoiceLiveModels))
		out = append(out, GroqVoiceModels...)
		out = append(out, GeminiVoiceLiveModels...)
		return "dual_voice_rotation", out
	}
	if geminiCount > 0 {
		return "gemini_live_rotation", append([]string(nil), GeminiVoiceLiveModels...)
	}
	if groqCount > 0 {
		return "groq_whisper_rotation", append([]string(nil), GroqVoiceModels...)
	}
	return "disabled", []string{}
}

// wavToPCM16kMono converts a standard uncompressed RIFF/WAVE PCM payload in pure Go
// to 16kHz 16-bit little-endian mono PCM required by Gemini Live API.
func wavToPCM16kMono(wavBytes []byte) ([]byte, bool) {
	if len(wavBytes) < 44 || string(wavBytes[:4]) != "RIFF" || string(wavBytes[8:12]) != "WAVE" {
		return nil, false
	}
	var audioFormat, numChannels, bitsPerSample uint16
	var sampleRate uint32
	var pcmData []byte

	pos := 12
	for pos+8 <= len(wavBytes) {
		chunkID := string(wavBytes[pos : pos+4])
		chunkSize := int(binary.LittleEndian.Uint32(wavBytes[pos+4 : pos+8]))
		pos += 8
		if chunkSize < 0 || pos+chunkSize > len(wavBytes) {
			break
		}
		switch chunkID {
		case "fmt ":
			if chunkSize >= 16 {
				audioFormat = binary.LittleEndian.Uint16(wavBytes[pos : pos+2])
				numChannels = binary.LittleEndian.Uint16(wavBytes[pos+2 : pos+4])
				sampleRate = binary.LittleEndian.Uint32(wavBytes[pos+4 : pos+8])
				bitsPerSample = binary.LittleEndian.Uint16(wavBytes[pos+14 : pos+16])
			}
		case "data":
			pcmData = wavBytes[pos : pos+chunkSize]
		}
		pos += chunkSize
		if chunkSize%2 == 1 {
			pos++
		}
	}

	if audioFormat != 1 || bitsPerSample != 16 || sampleRate == 0 || numChannels == 0 || len(pcmData) < 2 {
		return nil, false
	}

	frameBytes := int(numChannels) * 2
	totalFrames := len(pcmData) / frameBytes
	if totalFrames == 0 {
		return nil, false
	}
	mono := make([]int16, totalFrames)
	for i := 0; i < totalFrames; i++ {
		var sum int32
		for ch := 0; ch < int(numChannels); ch++ {
			off := i*frameBytes + ch*2
			sum += int32(int16(binary.LittleEndian.Uint16(pcmData[off : off+2])))
		}
		mono[i] = int16(sum / int32(numChannels))
	}

	if sampleRate == 16000 {
		out := make([]byte, len(mono)*2)
		for i, s := range mono {
			binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(s))
		}
		return out, true
	}

	outLen := int(float64(len(mono)) * 16000.0 / float64(sampleRate))
	if outLen <= 0 {
		return nil, false
	}
	out := make([]byte, outLen*2)
	ratio := float64(len(mono)) / float64(outLen)
	for i := 0; i < outLen; i++ {
		srcPos := float64(i) * ratio
		idx := int(srcPos)
		frac := srcPos - float64(idx)
		if idx >= len(mono)-1 {
			idx = len(mono) - 1
			frac = 0
		}
		s0 := float64(mono[idx])
		s1 := s0
		if idx+1 < len(mono) {
			s1 = float64(mono[idx+1])
		}
		val := int16(s0 + (s1-s0)*frac)
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(val))
	}
	return out, true
}

// prepareAudioForGeminiLive converts incoming WhatsApp audio (.ogg/opus, .mp3, .wav, .m4a)
// into raw 16kHz 16-bit LE mono PCM when WAV or ffmpeg is available.
func prepareAudioForGeminiLive(audioBytes []byte, mimeType string) (pcmBytes []byte, cleanMime string) {
	cleanMime = strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	if cleanMime == "" {
		cleanMime = "audio/ogg"
	}
	if cleanMime == "audio/pcm" || cleanMime == "audio/l16" {
		return audioBytes, "audio/pcm;rate=16000"
	}
	if pcm, ok := wavToPCM16kMono(audioBytes); ok {
		return pcm, cleanMime
	}

	if ffPath, err := exec.LookPath("ffmpeg"); err == nil && ffPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, ffPath,
			"-hide_banner", "-loglevel", "error",
			"-i", "pipe:0",
			"-f", "s16le",
			"-acodec", "pcm_s16le",
			"-ac", "1",
			"-ar", "16000",
			"pipe:1",
		)
		cmd.Stdin = bytes.NewReader(audioBytes)
		var outBuf bytes.Buffer
		cmd.Stdout = &outBuf
		if err := cmd.Run(); err == nil && outBuf.Len() > 0 {
			return outBuf.Bytes(), cleanMime
		}
	}
	return nil, cleanMime
}

func inferAudioFilename(mimeType string) string {
	clean := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch clean {
	case "audio/mpeg", "audio/mp3":
		return "voice.mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "voice.wav"
	case "audio/mp4", "audio/m4a", "audio/aac":
		return "voice.m4a"
	case "audio/webm":
		return "voice.webm"
	default:
		return "voice" + filepath.Ext(".ogg")
	}
}

// callGroqWhisperSlot transcribes an audio buffer via Groq Whisper (whisper-large-v3-turbo / whisper-large-v3).
func callGroqWhisperSlot(apiKey, model, bizContext string, audioBytes []byte, mimeType string) (string, int, http.Header, []byte, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", inferAudioFilename(mimeType))
	if err != nil {
		return "", 0, nil, nil, err
	}
	if _, err := part.Write(audioBytes); err != nil {
		return "", 0, nil, nil, err
	}
	_ = writer.WriteField("model", model)
	// Prompt primes Hindi/Hinglish/English retail product spelling for Indian kirana/retail voice notes
	promptHint := fmt.Sprintf("WhatsApp shopping voice note for %s. Transcribe Hindi, Hinglish, or English words accurately (e.g. atta, chini, sarso tel, dal, paneer, Maggi, packet, kg, COD, UPI, address).", bizContext)
	_ = writer.WriteField("prompt", promptHint)
	_ = writer.Close()

	req, err := http.NewRequest("POST", "https://api.groq.com/openai/v1/audio/transcriptions", &body)
	if err != nil {
		return "", 0, nil, nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)

	httpClient := &http.Client{Timeout: 16 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, nil, nil, err
	}
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var parsed struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(respBytes, &parsed); err == nil && strings.TrimSpace(parsed.Text) != "" {
			return strings.TrimSpace(parsed.Text), resp.StatusCode, resp.Header, respBytes, nil
		}
		return "", resp.StatusCode, resp.Header, respBytes, fmt.Errorf("empty groq whisper transcript")
	}
	return "", resp.StatusCode, resp.Header, respBytes, fmt.Errorf("groq whisper %s HTTP %d", model, resp.StatusCode)
}

// callGeminiLiveVoiceModel connects to Gemini Live API (BidiGenerateContent WebSocket)
// using gemini-3.1-flash-live-preview or gemini-2.5-flash-native-audio-preview-12-2025
// with AUDIO responseModality + inputAudioTranscription enabled to extract an exact transcript.
func callGeminiLiveVoiceModel(apiKey, model, bizContext string, audioBytes, pcm16k []byte, cleanMime string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	wsURL := fmt.Sprintf(
		"wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent?key=%s",
		apiKey,
	)
	conn, httpResp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"User-Agent": []string{"aistudio-build"},
		},
	})
	if err != nil {
		status := 0
		if httpResp != nil {
			status = httpResp.StatusCode
		}
		return "", status, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 * 1024 * 1024)

	setupPayload := map[string]interface{}{
		"setup": map[string]interface{}{
			"model": "models/" + model,
			"generationConfig": map[string]interface{}{
				"responseModalities": []string{"AUDIO"},
			},
			"inputAudioTranscription":  map[string]interface{}{},
			"outputAudioTranscription": map[string]interface{}{},
			"systemInstruction": map[string]interface{}{
				"parts": []map[string]string{{
					"text": fmt.Sprintf("You are listening to a customer WhatsApp voice note for shop \"%s\". Acknowledge briefly so your input transcription captures every spoken Hindi/Hinglish/English word accurately.", bizContext),
				}},
			},
		},
	}
	if err := wsjson.Write(ctx, conn, setupPayload); err != nil {
		return "", 0, err
	}

	// Wait for setupComplete
	setupDone := false
	for !setupDone {
		var msg map[string]interface{}
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return "", 0, fmt.Errorf("gemini live setup failed (%s): %w", model, err)
		}
		if _, ok := msg["setupComplete"]; ok {
			setupDone = true
		}
	}

	if len(pcm16k) > 0 {
		const chunkSize = 16000 // ~0.5s of 16kHz 16-bit mono PCM
		for off := 0; off < len(pcm16k); off += chunkSize {
			end := off + chunkSize
			if end > len(pcm16k) {
				end = len(pcm16k)
			}
			chunkB64 := base64.StdEncoding.EncodeToString(pcm16k[off:end])
			frame := map[string]interface{}{
				"realtimeInput": map[string]interface{}{
					"audio": map[string]interface{}{
						"mimeType": "audio/pcm;rate=16000",
						"data":     chunkB64,
					},
				},
			}
			if err := wsjson.Write(ctx, conn, frame); err != nil {
				return "", 0, err
			}
		}
		if err := wsjson.Write(ctx, conn, map[string]interface{}{
			"realtimeInput": map[string]interface{}{
				"audioStreamEnd": true,
			},
		}); err != nil {
			return "", 0, err
		}
	} else {
		// Fallback when container is compressed OGG/MP3 without ffmpeg: send discrete clientContent turn
		rawB64 := base64.StdEncoding.EncodeToString(audioBytes)
		frame := map[string]interface{}{
			"clientContent": map[string]interface{}{
				"turns": []map[string]interface{}{{
					"role": "user",
					"parts": []map[string]interface{}{
						{
							"inlineData": map[string]string{
								"mimeType": cleanMime,
								"data":     rawB64,
							},
						},
						{
							"text": "Transcribe the customer's voice note above verbatim.",
						},
					},
				}},
				"turnComplete": true,
			},
		}
		if err := wsjson.Write(ctx, conn, frame); err != nil {
			return "", 0, err
		}
	}

	var inputTranscript strings.Builder
	var outputTranscript strings.Builder
	var textParts strings.Builder

	for {
		var msg map[string]interface{}
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			// If we already captured transcription before socket closed, return it
			if strings.TrimSpace(inputTranscript.String()) != "" {
				break
			}
			return "", 0, err
		}
		sc, ok := msg["serverContent"].(map[string]interface{})
		if !ok || sc == nil {
			continue
		}
		if inTr, ok := sc["inputTranscription"].(map[string]interface{}); ok && inTr != nil {
			if txt, ok := inTr["text"].(string); ok {
				inputTranscript.WriteString(txt)
			}
		}
		if outTr, ok := sc["outputTranscription"].(map[string]interface{}); ok && outTr != nil {
			if txt, ok := outTr["text"].(string); ok {
				outputTranscript.WriteString(txt)
			}
		}
		if mt, ok := sc["modelTurn"].(map[string]interface{}); ok && mt != nil {
			if parts, ok := mt["parts"].([]interface{}); ok {
				for _, pRaw := range parts {
					if pm, ok := pRaw.(map[string]interface{}); ok {
						if txt, ok := pm["text"].(string); ok {
							textParts.WriteString(txt)
						}
					}
				}
			}
		}
		if tc, _ := sc["turnComplete"].(bool); tc {
			break
		}
		if gc, _ := sc["generationComplete"].(bool); gc && strings.TrimSpace(inputTranscript.String()) != "" {
			break
		}
	}

	_ = conn.Close(websocket.StatusNormalClosure, "done")

	if res := strings.TrimSpace(inputTranscript.String()); res != "" {
		return res, http.StatusOK, nil
	}
	if res := strings.TrimSpace(textParts.String()); res != "" {
		return res, http.StatusOK, nil
	}
	if res := strings.TrimSpace(outputTranscript.String()); res != "" {
		return res, http.StatusOK, nil
	}
	return "", http.StatusOK, fmt.Errorf("empty transcript from gemini live model %s", model)
}

// callGeminiAudioRESTSafetyFallback is ONLY used if ALL Live WebSocket models cannot decode compressed OGG without ffmpeg.
func callGeminiAudioRESTSafetyFallback(apiKey, bizContext string, audioBytes []byte, cleanMime string) (string, int) {
	endpoint := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-3.1-flash-lite:generateContent?key=%s", apiKey)
	payload := map[string]interface{}{
		"contents": []map[string]interface{}{{
			"role": "user",
			"parts": []map[string]interface{}{
				{
					"inlineData": map[string]string{
						"mimeType": cleanMime,
						"data":     base64.StdEncoding.EncodeToString(audioBytes),
					},
				},
				{
					"text": fmt.Sprintf("Transcribe this WhatsApp customer voice note for shop %q verbatim in the original Hindi/Hinglish/English words. Output ONLY the spoken transcript, no extra commentary.", bizContext),
				},
			},
		}},
		"generationConfig": map[string]interface{}{
			"temperature":     0.1,
			"maxOutputTokens": 250,
		},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(b))
	if err != nil {
		return "", 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "aistudio-build")
	resp, err := (&http.Client{Timeout: 16 * time.Second}).Do(req)
	if err != nil {
		return "", 0
	}
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(respBytes, &out); err == nil && len(out.Candidates) > 0 && len(out.Candidates[0].Content.Parts) > 0 {
		return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), resp.StatusCode
	}
	return "", resp.StatusCode
}

type voiceCandidateSlot struct {
	provider string
	keyIndex int
	apiKey   string
	keyHash  string
	model    string
}

func buildGroqVoiceSlots(groqKeys []string) []voiceCandidateSlot {
	if len(groqKeys) == 0 {
		return nil
	}
	now := time.Now()
	voicePoolMu.Lock()
	defer voicePoolMu.Unlock()

	total := len(groqKeys) * len(GroqVoiceModels)
	matrix := make([]voiceCandidateSlot, 0, total)
	for _, mdl := range GroqVoiceModels {
		for kIdx, key := range groqKeys {
			matrix = append(matrix, voiceCandidateSlot{
				provider: "groq",
				keyIndex: kIdx,
				apiKey:   key,
				keyHash:  shortKeyHash(key),
				model:    mdl,
			})
		}
	}
	start := int(groqVoiceCursor % uint64(total))
	groqVoiceCursor++

	rotated := make([]voiceCandidateSlot, 0, total)
	rotated = append(rotated, matrix[start:]...)
	rotated = append(rotated, matrix[:start]...)

	ready := make([]voiceCandidateSlot, 0, total)
	for _, slot := range rotated {
		st := getVoiceSlotLocked(groqVoiceSlotStates, slot.keyHash, slot.model, now)
		if now.Before(st.authDisabledUntil) || now.Before(st.dailyCooldownUntil) || now.Before(st.shortCooldownUntil) {
			continue
		}
		ready = append(ready, slot)
	}
	return ready
}

func buildGeminiVoiceSlots(geminiKeys []string) []voiceCandidateSlot {
	if len(geminiKeys) == 0 {
		return nil
	}
	now := time.Now()
	voicePoolMu.Lock()
	defer voicePoolMu.Unlock()

	total := len(geminiKeys) * len(GeminiVoiceLiveModels)
	matrix := make([]voiceCandidateSlot, 0, total)
	for _, mdl := range GeminiVoiceLiveModels {
		for kIdx, key := range geminiKeys {
			matrix = append(matrix, voiceCandidateSlot{
				provider: "gemini",
				keyIndex: kIdx,
				apiKey:   key,
				keyHash:  shortKeyHash(key),
				model:    mdl,
			})
		}
	}
	start := int(geminiVoiceCursor % uint64(total))
	geminiVoiceCursor++

	rotated := make([]voiceCandidateSlot, 0, total)
	rotated = append(rotated, matrix[start:]...)
	rotated = append(rotated, matrix[:start]...)

	ready := make([]voiceCandidateSlot, 0, total)
	for _, slot := range rotated {
		st := getVoiceSlotLocked(geminiVoiceSlotStates, slot.keyHash, slot.model, now)
		if now.Before(st.authDisabledUntil) || now.Before(st.dailyCooldownUntil) || now.Before(st.shortCooldownUntil) {
			continue
		}
		ready = append(ready, slot)
	}
	return ready
}

// ExecuteVoiceNoteHand is the Octopus Hand that transcribes customer WhatsApp voice notes using:
//   - Gemini-Only: rotates gemini-3.1-flash-live-preview & gemini-2.5-flash-native-audio-preview-12-2025
//   - Groq-Only:   rotates whisper-large-v3-turbo & whisper-large-v3
//   - Dual Mode:   alternates turn-by-turn ("bari-bari") between Groq Whisper & Gemini Live Audio pools
func ExecuteVoiceNoteHand(username string, cfg *UserConfigRAM, audioBytes []byte, mimeType string, durationSec uint32) VoiceNoteHandResult {
	res := VoiceNoteHandResult{
		Hand:        "voice_note_transcribe",
		DurationSec: durationSec,
	}
	if len(audioBytes) == 0 {
		res.Err = fmt.Errorf("empty voice note audio bytes")
		return res
	}
	if cfg == nil {
		cfg = GetUserConfigRAM(username)
	}

	groqKeys, geminiKeys, _ := ClassifyConfiguredAIKeys(username, cfg)
	mode, _ := GetActiveVoiceRoutingInfo(len(groqKeys), len(geminiKeys))
	res.RoutingMode = mode
	if len(groqKeys) == 0 && len(geminiKeys) == 0 {
		res.Err = fmt.Errorf("no Groq or Gemini keys available for voice note hand")
		return res
	}

	bizContext := strings.TrimSpace(cfg.BusinessName)
	if bizContext == "" {
		bizContext = ramstore.Global.GetVendor().BusinessName
	}
	if bizContext == "" {
		bizContext = "Indian Retail Store"
	}

	var queue []voiceCandidateSlot
	groqSlots := buildGroqVoiceSlots(groqKeys)
	geminiSlots := buildGeminiVoiceSlots(geminiKeys)

	if len(groqKeys) > 0 && len(geminiKeys) > 0 {
		voicePoolMu.Lock()
		geminiFirst := (dualVoiceTurnCursor % 2) == 1
		dualVoiceTurnCursor++
		voicePoolMu.Unlock()

		gi, mi := 0, 0
		turnGem := geminiFirst
		for gi < len(groqSlots) || mi < len(geminiSlots) {
			if turnGem {
				if mi < len(geminiSlots) {
					queue = append(queue, geminiSlots[mi])
					mi++
				} else if gi < len(groqSlots) {
					queue = append(queue, groqSlots[gi])
					gi++
				}
			} else {
				if gi < len(groqSlots) {
					queue = append(queue, groqSlots[gi])
					gi++
				} else if mi < len(geminiSlots) {
					queue = append(queue, geminiSlots[mi])
					mi++
				}
			}
			turnGem = !turnGem
		}
	} else if len(groqKeys) > 0 {
		queue = groqSlots
	} else {
		queue = geminiSlots
	}

	var pcm16k []byte
	var cleanMime string
	pcmPrepared := false

	var lastErr error
	for _, slot := range queue {
		if slot.provider == "groq" {
			transcript, status, headers, body, err := callGroqWhisperSlot(slot.apiKey, slot.model, bizContext, audioBytes, mimeType)
			if err == nil && transcript != "" {
				voicePoolMu.Lock()
				st := getVoiceSlotLocked(groqVoiceSlotStates, slot.keyHash, slot.model, time.Now())
				st.requestsToday++
				st.shortCooldownUntil = time.Time{}
				voicePoolMu.Unlock()

				res.Transcript = transcript
				res.CleanQuery = transcript
				res.Provider = "groq"
				res.Model = slot.model
				res.KeyIndex = slot.keyIndex
				log.Printf("🎙️ [Octopus Voice Hand | %s] Transcribed via Groq (%s, key #%d, %ds): %q",
					mode, slot.model, slot.keyIndex+1, durationSec, compactStrForLog(transcript, 90))
				return res
			}
			lastErr = err
			voicePoolMu.Lock()
			st := getVoiceSlotLocked(groqVoiceSlotStates, slot.keyHash, slot.model, time.Now())
			switch status {
			case http.StatusTooManyRequests:
				isTPD, cd := classifyGroq429(headers, body)
				if isTPD {
					st.dailyCooldownUntil = time.Now().Add(cd)
				} else {
					st.shortCooldownUntil = time.Now().Add(cd)
				}
			case http.StatusUnauthorized, http.StatusForbidden:
				for _, m := range GroqVoiceModels {
					getVoiceSlotLocked(groqVoiceSlotStates, slot.keyHash, m, time.Now()).authDisabledUntil = time.Now().Add(30 * time.Minute)
				}
			default:
				st.shortCooldownUntil = time.Now().Add(45 * time.Second)
			}
			voicePoolMu.Unlock()
			log.Printf("⚡ [Octopus Voice Hand] Groq %s failed (%v) -> rotating next voice slot", slot.model, err)
		} else {
			if !pcmPrepared {
				pcm16k, cleanMime = prepareAudioForGeminiLive(audioBytes, mimeType)
				pcmPrepared = true
			}
			transcript, status, err := callGeminiLiveVoiceModel(slot.apiKey, slot.model, bizContext, audioBytes, pcm16k, cleanMime)
			if err == nil && transcript != "" {
				voicePoolMu.Lock()
				st := getVoiceSlotLocked(geminiVoiceSlotStates, slot.keyHash, slot.model, time.Now())
				st.requestsToday++
				st.shortCooldownUntil = time.Time{}
				voicePoolMu.Unlock()

				res.Transcript = transcript
				res.CleanQuery = transcript
				res.Provider = "gemini"
				res.Model = slot.model
				res.KeyIndex = slot.keyIndex
				log.Printf("🎙️ [Octopus Voice Hand | %s] Transcribed via Gemini Live (%s, key #%d, %ds): %q",
					mode, slot.model, slot.keyIndex+1, durationSec, compactStrForLog(transcript, 90))
				return res
			}
			lastErr = err
			voicePoolMu.Lock()
			st := getVoiceSlotLocked(geminiVoiceSlotStates, slot.keyHash, slot.model, time.Now())
			if status == http.StatusTooManyRequests {
				st.shortCooldownUntil = time.Now().Add(65 * time.Second)
			} else if status == http.StatusUnauthorized || status == http.StatusForbidden {
				for _, m := range GeminiVoiceLiveModels {
					getVoiceSlotLocked(geminiVoiceSlotStates, slot.keyHash, m, time.Now()).authDisabledUntil = time.Now().Add(30 * time.Minute)
				}
			} else {
				st.shortCooldownUntil = time.Now().Add(45 * time.Second)
			}
			voicePoolMu.Unlock()
			log.Printf("⚡ [Octopus Voice Hand] Gemini Live %s failed (%v) -> rotating next voice slot", slot.model, err)
		}
	}

	// Safety fallback if ALL Gemini Live slots could not process compressed OGG (e.g. Render bare container without ffmpeg)
	if len(geminiKeys) > 0 && len(pcm16k) == 0 {
		if cleanMime == "" {
			cleanMime = "audio/ogg"
		}
		for kIdx, key := range geminiKeys {
			if tr, stCode := callGeminiAudioRESTSafetyFallback(key, bizContext, audioBytes, cleanMime); stCode == http.StatusOK && tr != "" {
				res.Transcript = tr
				res.CleanQuery = tr
				res.Provider = "gemini"
				res.Model = "gemini-3.1-flash-lite"
				res.KeyIndex = kIdx
				log.Printf("🎙️ [Octopus Voice Hand | REST OGG Fallback] Transcribed via gemini-3.1-flash-lite (key #%d): %q",
					kIdx+1, compactStrForLog(tr, 90))
				return res
			}
		}
	}

	res.Err = lastErr
	return res
}

func compactStrForLog(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func formatSecondsForVoiceFallback(sec uint32) string {
	if sec == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(sec), 10) + "s "
}
