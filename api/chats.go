package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"wa-server/ramstore"
	"wa-server/whatsapp"
)

// ChatsHandler returns active conversations strictly from RAM (0ms).
func ChatsHandler(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, 200, map[string]interface{}{
		"conversations": ramstore.Global.ListConversations(),
	})
}

// ChatMessagesHandler returns the sliding-window messages for a customer phone strictly from RAM.
func ChatMessagesHandler(w http.ResponseWriter, r *http.Request) {
	phone := whatsapp.FormatPhone(r.URL.Query().Get("phone"))
	if phone == "" {
		respondJSON(w, 200, map[string]interface{}{"messages": []interface{}{}})
		return
	}
	respondJSON(w, 200, map[string]interface{}{
		"messages": ramstore.Global.GetChatMessages(phone),
	})
}

// ChatSendHandler sends a manual WhatsApp reply, updates RAM sliding window (max 10), and pushes to SSE (zero Central chat sync).
func ChatSendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Phone   string `json:"phone"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	cleanPhone := whatsapp.FormatPhone(req.Phone)
	cleanMsg := strings.TrimSpace(req.Message)
	if cleanPhone == "" || cleanMsg == "" {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Phone number and message text required"})
		return
	}

	if client := whatsapp.GetAnyLoggedInClient(); client != nil {
		_ = whatsapp.SendMessage(client, cleanPhone, cleanMsg)
	}

	vendorUser := ramstore.Global.GetVendor().Username
	whatsapp.SaveCustomerChatAndPrune(vendorUser, cleanPhone, "outgoing_manual", cleanMsg)
	whatsapp.PushToLiveChat(vendorUser, cleanPhone, cleanMsg, "outgoing_manual")

	respondJSON(w, 200, map[string]interface{}{
		"status":  "success",
		"message": "Reply dispatched via WhatsApp and stored in local RAM (last 10 messages per customer, zero chat sync).",
	})
}

// LiveChatSSEHandler streams live incoming/outgoing messages to the dashboard browser.
func LiveChatSSEHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	streamID := fmt.Sprintf("sse_%d", time.Now().UnixNano())

	messageChan := make(chan map[string]string, 16)
	whatsapp.StreamLock.Lock()
	whatsapp.LiveChatStreams[streamID] = messageChan
	whatsapp.StreamLock.Unlock()

	defer func() {
		whatsapp.StreamLock.Lock()
		delete(whatsapp.LiveChatStreams, streamID)
		whatsapp.StreamLock.Unlock()
	}()

	fmt.Fprintf(w, "data: {\"type\": \"connected\"}\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-messageChan:
			if !ok {
				return
			}
			dataBytes, _ := json.Marshal(msg)
			fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
			flusher.Flush()
		}
	}
}
