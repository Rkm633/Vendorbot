package api

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	"wa-server/ramstore"
	"wa-server/whatsapp"
)

// SendHandler sends a single WhatsApp text or image message.
func SendHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone    string `json:"phone"`
		Message  string `json:"message"`
		ImageURL string `json:"image_url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.Phone == "" {
		req.Phone = r.URL.Query().Get("phone")
	}
	if req.Message == "" {
		req.Message = r.URL.Query().Get("message")
	}
	if req.ImageURL == "" {
		req.ImageURL = r.URL.Query().Get("image_url")
	}

	client := whatsapp.GetAnyLoggedInClient()
	if client == nil {
		respondJSON(w, 503, map[string]string{"status": "error", "message": "WhatsApp device is not connected"})
		return
	}

	var err error
	if req.ImageURL != "" {
		err = whatsapp.SendImageFromURL(client, req.Phone, req.ImageURL, req.Message)
	} else if req.Message != "" {
		err = whatsapp.SendMessage(client, req.Phone, req.Message)
	}
	if err != nil {
		respondJSON(w, 500, map[string]string{"status": "error", "message": err.Error()})
		return
	}

	vendorUser := ramstore.Global.GetVendor().Username
	whatsapp.SaveCustomerChatAndPrune(vendorUser, req.Phone, "assistant", req.Message)
	whatsapp.PushToLiveChat(vendorUser, req.Phone, req.Message, "outgoing_manual")
	respondJSON(w, 200, map[string]string{"status": "success", "message": "Message Sent"})
}

// BulkSendHandler launches an anti-ban broadcast campaign tracked in RAM.
func BulkSendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var rawBody map[string]interface{}
	bodyBytes, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(bodyBytes, &rawBody)

	message, _ := rawBody["message"].(string)
	imageURL, _ := rawBody["image_url"].(string)
	useAntiBan := true
	if ab, ok := rawBody["anti_ban"].(bool); ok {
		useAntiBan = ab
	}

	var phoneList []string
	if phoneStr, ok := rawBody["phones"].(string); ok {
		re := regexp.MustCompile(`[\r\n,;]+`)
		for _, t := range re.Split(phoneStr, -1) {
			clean := whatsapp.FormatPhone(t)
			if len(clean) >= 8 && len(clean) <= 15 {
				phoneList = append(phoneList, clean)
			}
		}
	} else if phoneArr, ok := rawBody["phones"].([]interface{}); ok {
		for _, item := range phoneArr {
			if s, ok := item.(string); ok {
				clean := whatsapp.FormatPhone(s)
				if len(clean) >= 8 && len(clean) <= 15 {
					phoneList = append(phoneList, clean)
				}
			}
		}
	}

	if len(phoneList) == 0 {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "No valid phone numbers provided"})
		return
	}

	client := whatsapp.GetAnyLoggedInClient()
	if client == nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Connect WhatsApp before launching broadcast"})
		return
	}

	campaignID := fmt.Sprintf("camp_%d_%d", time.Now().Unix(), rand.Intn(1000))
	vendorUser := ramstore.Global.GetVendor().Username

	go func() {
		for i, p := range phoneList {
			if useAntiBan && i > 0 {
				time.Sleep(time.Duration(rand.Intn(4)+3) * time.Second)
			}
			_ = whatsapp.EnsureConnected(client)

			var sendErr error
			if imageURL != "" {
				sendErr = whatsapp.SendImageFromURL(client, p, imageURL, message)
			} else {
				sendErr = whatsapp.SendMessage(client, p, message)
			}

			status := "sent"
			note := ""
			if sendErr != nil {
				status = "failed"
				note = sendErr.Error()
			} else {
				whatsapp.SaveCustomerChatAndPrune(vendorUser, p, "assistant", message)
				whatsapp.PushToLiveChat(vendorUser, p, message, "outgoing_bulk")
			}

			ramstore.Global.AddBulkLog(ramstore.BulkLogItem{
				CampaignID: campaignID,
				Phone:      p,
				Status:     status,
				Note:       note,
				Time:       time.Now().Format("02 Jan 15:04"),
			})
		}
	}()

	respondJSON(w, 200, map[string]interface{}{
		"status":      "success",
		"campaign_id": campaignID,
		"total":       len(phoneList),
	})
}

// BulkStatusHandler returns recent broadcast delivery logs from RAM.
func BulkStatusHandler(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, 200, map[string]interface{}{
		"logs": ramstore.Global.ListBulkLogs(),
	})
}

// BulkUploadTempHandler echoes temporary base64 data URLs for broadcast media preview.
func BulkUploadTempHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DataURL  string `json:"data_url"`
		Filename string `json:"filename"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if strings.TrimSpace(req.DataURL) == "" {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "No media data URL provided"})
		return
	}
	fn := req.Filename
	if fn == "" {
		fn = "broadcast.jpg"
	}
	respondJSON(w, 200, map[string]interface{}{
		"status":    "success",
		"file_url":  req.DataURL,
		"file_path": fn,
	})
}
