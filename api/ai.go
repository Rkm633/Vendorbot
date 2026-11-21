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

// AISettingsHandler reads or updates the vendor's AI/store config in RAM.
func AISettingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v := ramstore.Global.GetVendor()
		a := ramstore.Global.GetAI()
		p := ramstore.Global.GetPayment()
		respondJSON(w, 200, map[string]interface{}{
			"status":                    "success",
			"business_name":             v.BusinessName,
			"business_type":             v.BusinessType,
			"response_language":         v.Language,
			"ai_provider":               a.Provider,
			"ai_model":                  a.Model,
			"ai_temperature":            a.Temperature,
			"system_prompt":             a.SystemPrompt,
			"auto_reply":                a.AutoReply,
			"import_catalog_details":    a.ImportCatalog,
			"notification_phone":        v.NotificationPhone,
			"paytm_mid":                 p.PaytmMID,
			"paytm_vpa":                 p.MerchantVPA,
			"store_policies":            v.StorePolicies,
			"max_delivery_radius_km":    v.MaxDeliveryRadiusKM,
			"free_delivery_radius_km":   v.FreeDeliveryRadiusKM,
			"delivery_charge_amount":    v.DeliveryChargeAmount,
			"require_customer_location": v.RequireCustomerLocation,
			"allow_cod":                 v.AllowCOD,
		})
		return
	}

	if r.Method == "POST" {
		var req struct {
			BusinessName      string  `json:"business_name"`
			BusinessType      string  `json:"business_type"`
			Language          string  `json:"language"`
			AIProvider        string   `json:"ai_provider"`
			APIKey            string   `json:"api_key"`
			APIKeys           []string `json:"api_keys"`
			BackupAPIKeys     []string `json:"backup_api_keys"`
			AIModel           string   `json:"ai_model"`
			AITemperature     float64 `json:"ai_temperature"`
			SystemPrompt      string  `json:"system_prompt"`
			AutoReply         *bool   `json:"auto_reply"`
			ImportCatalog     *bool   `json:"import_catalog"`
			NotificationPhone string  `json:"notification_phone"`
			StorePolicies     string  `json:"store_policies"`
			AllowCOD          *bool   `json:"allow_cod"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON body: " + err.Error()})
			return
		}

		v := ramstore.Global.GetVendor()
		if req.BusinessName != "" {
			v.BusinessName = strings.TrimSpace(req.BusinessName)
		}
		if req.BusinessType != "" {
			v.BusinessType = strings.TrimSpace(req.BusinessType)
		}
		if req.Language != "" {
			v.Language = strings.TrimSpace(req.Language)
		}
		if req.NotificationPhone != "" {
			v.NotificationPhone = strings.TrimSpace(req.NotificationPhone)
		}
		if req.StorePolicies != "" {
			v.StorePolicies = strings.TrimSpace(req.StorePolicies)
		}
		if req.AllowCOD != nil {
			v.AllowCOD = *req.AllowCOD
		}
		ramstore.Global.UpdateVendor(v)

		a := ramstore.Global.GetAI()
		if req.AIProvider != "" {
			a.Provider = strings.TrimSpace(req.AIProvider)
		}
		if req.APIKey != "" {
			a.APIKey = strings.TrimSpace(req.APIKey)
		}
		if len(req.APIKeys) > 0 {
			a.APIKeys = req.APIKeys
		}
		if len(req.BackupAPIKeys) > 0 {
			a.BackupAPIKeys = req.BackupAPIKeys
		}
		if req.AIModel != "" {
			a.Model = strings.TrimSpace(req.AIModel)
		}
		if req.AITemperature > 0 {
			a.Temperature = req.AITemperature
		}
		if req.SystemPrompt != "" {
			a.SystemPrompt = strings.TrimSpace(req.SystemPrompt)
		}
		if req.AutoReply != nil {
			a.AutoReply = *req.AutoReply
		}
		if req.ImportCatalog != nil {
			a.ImportCatalog = *req.ImportCatalog
		}
		ramstore.Global.SetAI(a)
		whatsapp.PushToLiveChat(v.Username, "", "ai_settings", "ram_synced")

		respondJSON(w, 200, map[string]interface{}{
			"status":        "success",
			"api_key_count": len(ramstore.Global.GetAI().AllAPIKeys()),
			"message":       "AI settings updated in RAM.",
		})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// AITestHandler runs the Octopus AI Auto-Responder directly against live RAM catalog & carts.
func AITestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Query         string `json:"query"`
		CustomerPhone string `json:"customer_phone"`
		Provider      string `json:"provider"`
		APIKey        string `json:"api_key"`
		SystemPrompt  string `json:"system_prompt"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if strings.TrimSpace(req.Query) == "" {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Customer query text is required"})
		return
	}
	phone := whatsapp.FormatPhone(req.CustomerPhone)
	if phone == "" {
		phone = "919999999999"
	}

	vendorUser := ramstore.Global.GetVendor().Username
	octResp, err := whatsapp.GenerateAIReply(vendorUser, phone, req.Query, req.Provider, req.APIKey, req.SystemPrompt)
	if err != nil || octResp == nil {
		msg := "Configure an AI API key via Central Brain Node (or GROQ_API_KEY / GEMINI_API_KEY env var) to test live LLM replies."
		if err != nil {
			msg = err.Error()
		}
		respondJSON(w, 200, map[string]interface{}{
			"status":  "fallback",
			"reply":   fmt.Sprintf("AI Key Notice: %s", msg),
			"actions": []interface{}{},
		})
		return
	}

	// Apply cart/profile actions to RAM during interactive playground test so state updates live
	for _, act := range octResp.Actions {
		if act.Hand == "update_cart" && act.Payload != nil && len(act.Payload.CartItems) > 0 {
			var items []whatsapp.AICartItem
			for _, ci := range act.Payload.CartItems {
				items = append(items, whatsapp.AICartItem{ProductID: ci.ID, Qty: int(ci.Qty), Variant: ci.Variant})
			}
			_, _ = whatsapp.AddItemsToActiveCartWithStockCheck(vendorUser, phone, items)
		} else if act.Hand == "extract_profile" && act.Payload != nil && act.Payload.Profile != nil {
			_ = whatsapp.UpdateCustomerProfileFromData(vendorUser, phone, &whatsapp.AIProfileData{
				Name:          act.Payload.Profile.Name,
				Address:       act.Payload.Profile.Address,
				ContactNumber: act.Payload.Profile.Contact,
			})
		} else if act.Hand == "clear_cart" {
			_ = whatsapp.ClearActiveCart(vendorUser, phone)
		}
	}

	whatsapp.SaveCustomerChatAndPrune(vendorUser, phone, "customer", req.Query)
	whatsapp.SaveCustomerChatAndPrune(vendorUser, phone, "assistant", octResp.AIReplyText)

	respondJSON(w, 200, map[string]interface{}{
		"status":  "success",
		"reply":   octResp.AIReplyText,
		"actions": octResp.Actions,
		"cart":    ramstore.Global.GetCart(phone),
	})
}

// TestLeadAlertHandler dispatches a test owner alert over WhatsApp and records lead in RAM + Central Node.
func TestLeadAlertHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		NotificationPhone string `json:"notification_phone"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	targetPhone := strings.TrimSpace(req.NotificationPhone)
	if targetPhone == "" {
		targetPhone = ramstore.Global.GetVendor().NotificationPhone
	}
	cleanPhone := whatsapp.FormatPhone(targetPhone)
	if cleanPhone == "" {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Notification phone number is required"})
		return
	}

	vendor := ramstore.Global.GetVendor()
	alertText := fmt.Sprintf("🚨 *TEST OWNER ALERT* 🚨\n\n• Store: %s\n• Target: +%s\n• Time: %s\n\n✅ Your stateless Vendor WhatsApp Bot Node is connected and operational!",
		vendor.BusinessName, cleanPhone, time.Now().Format("02 Jan 15:04:05"))

	whatsapp.PushToLiveChat(vendor.Username, cleanPhone, alertText, "lead_alert")
	if client := whatsapp.GetAnyLoggedInClient(); client != nil {
		_ = whatsapp.SendMessage(client, cleanPhone, alertText)
	}

	_ = ramstore.Global.AddLead(cleanPhone, "Test Owner Alert Dispatched", "Test Alert")

	respondJSON(w, 200, map[string]interface{}{
		"status":             "success",
		"message":            fmt.Sprintf("Test alert dispatched to +%s", cleanPhone),
		"notification_phone": cleanPhone,
	})
}
