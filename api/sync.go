package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wa-server/central"
	"wa-server/ramstore"
	"wa-server/whatsapp"
)

// HealthHandler reports liveness for Render / Railway / Koyeb health checks.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	syncSt := ramstore.Global.GetSyncStatus()
	waSt := ramstore.Global.GetWhatsAppSession()
	vendor := ramstore.Global.GetVendor()
	canReply, pauseReason := ramstore.Global.CanAutoReply()
	storeOpen, closedReason := vendor.IsStoreOpenNow()

	respondJSON(w, 200, map[string]interface{}{
		"status":                  "ok",
		"mode":                    "stateless-ram-bot",
		"database":                "none",
		"sync_mode":               "event-driven-zero-polling",
		"chat_sync":               "ram-only-max-10",
		"bot_public_url":          syncSt.BotPublicURL,
		"central_sync":            syncSt.Connected,
		"whatsapp":                waSt.Status,
		"can_auto_reply":          canReply,
		"auto_reply_pause_reason": pauseReason,
		"store_open":              storeOpen,
		"store_closed_reason":     closedReason,
		"media_cache_entries":     whatsapp.GetMediaCacheSize(),
		"queue_depth":             syncSt.QueueDepth,
		"retry_buffer_depth":      syncSt.RetryBufferDepth,
		"stats":                   ramstore.Global.Stats(),
		"timestamp":               time.Now().UTC().Format(time.RFC3339),
	})
}

// BotOverviewHandler returns complete RAM telemetry, vendor metadata, Central Node status, governance gates, and WhatsApp session state.
func BotOverviewHandler(w http.ResponseWriter, r *http.Request) {
	vendor := ramstore.Global.GetVendor()
	ai := ramstore.Global.GetAI()
	pay := ramstore.Global.GetPayment()
	syncSt := ramstore.Global.GetSyncStatus()
	waSt := ramstore.Global.GetWhatsAppSession()

	// Refresh live whatsmeow connection state
	if client := whatsapp.GetVendorClient(); client != nil && client.Store != nil && client.Store.ID != nil {
		waSt.HasSession = true
		waSt.NeedsLogin = false
		waSt.DeviceJID = client.Store.ID.String()
		waSt.Phone = whatsapp.FormatPhone(client.Store.ID.User)
		if client.IsConnected() {
			waSt.Status = "Connected"
		} else {
			waSt.Status = "Connecting"
		}
	} else if qr := whatsapp.GetCurrentQRCode(); qr != "" {
		waSt.QRCode = qr
		waSt.Status = "QR_Ready"
		waSt.NeedsLogin = true
	}

	allKeys := ai.AllAPIKeys()
	primaryKey := ""
	if len(allKeys) > 0 {
		primaryKey = allKeys[0]
	}
	maskedKey := ""
	if len(primaryKey) > 8 {
		maskedKey = primaryKey[:4] + "••••" + primaryKey[len(primaryKey)-4:]
	} else if primaryKey != "" {
		maskedKey = "••••"
	}

	canReply, pauseReason := ramstore.Global.CanAutoReply()
	storeOpen, closedReason := vendor.IsStoreOpenNow()
	subValid, subReason := vendor.IsSubscriptionValid()

	bannerCached, bannerBytesSize, bannerCachedURL, bannerCachedUpdatedAt := ramstore.GlobalBannerCache.Stats()
	geminiPriority, groqText, groqAudio := whatsapp.GetModelPoolConfig()

	respondJSON(w, 200, map[string]interface{}{
		"status": "success",
		"sync":   syncSt,
		"vendor": vendor,
		"governance": map[string]interface{}{
			"can_auto_reply":           canReply,
			"auto_reply_pause_reason":  pauseReason,
			"store_open":               storeOpen,
			"store_closed_reason":      closedReason,
			"holiday_mode":             vendor.HolidayMode,
			"operating_hours":          vendor.OperatingHours,
			"next_open_time":           vendor.NextOpenTime,
			"min_order_amount":         vendor.MinOrderAmount,
			"estimated_delivery_time":  vendor.EstimatedDeliveryTime,
			"subscription_valid":       subValid,
			"subscription_reason":      subReason,
			"subscription_valid_until": vendor.SubscriptionValidUntil,
			"admin_ai_allowed":         vendor.AdminAIAutoReplyAllowed == nil || *vendor.AdminAIAutoReplyAllowed,
			"admin_pause_reason":       vendor.AdminPauseReason,
		},
		"ai": map[string]interface{}{
			"provider":               ai.Provider,
			"model":                  ai.Model,
			"temperature":            ai.Temperature,
			"auto_reply":             ai.AutoReply,
			"import_catalog":         ai.ImportCatalog,
			"has_api_key":            len(allKeys) > 0,
			"api_key_count":          len(allKeys),
			"backup_api_key_count":   len(ai.BackupAPIKeys),
			"api_key_masked":         maskedKey,
			"system_prompt":          ai.SystemPrompt,
			"gemini_priority_models": geminiPriority,
			"groq_text_models":       groqText,
			"groq_audio_models":      groqAudio,
		},
		"payment": map[string]interface{}{
			"enabled":                  pay.Enabled || pay.PaytmMID != "" || pay.MerchantVPA != "" || pay.UPIID != "" || pay.Gateway != "",
			"gateway":                  pay.Gateway,
			"upi_id":                   pay.UPIID,
			"merchant_vpa":             pay.MerchantVPA,
			"paytm_mid":                pay.PaytmMID,
			"payee_name":               pay.PayeeName,
			"merchant_phone":           pay.MerchantPhone,
			"local_qr_generation":      pay.LocalQRGeneration,
			"local_paytm_verify":       pay.LocalPaytmVerify,
			"paytm_status_url":         pay.PaytmStatusURL,
			"payment_confirm_endpoint": pay.PaymentConfirmEndpoint,
			"qr_only_mode":             pay.QROnlyMode,
			"send_payment_link":        pay.SendPaymentLink,
			"verify_endpoint":          pay.VerifyEndpoint,
			"verify_poll_interval_ms":  pay.VerifyPollIntervalMS,
			"verify_poll_timeout_ms":   pay.VerifyPollTimeoutMS,
			"banner_template":          pay.BannerTemplate,
			"banner_cache": map[string]interface{}{
				"cached":     bannerCached,
				"size_bytes": bannerBytesSize,
				"url":        bannerCachedURL,
				"updated_at": bannerCachedUpdatedAt,
			},
		},
		"whatsapp":            waSt,
		"media_cache_entries": whatsapp.GetMediaCacheSize(),
		"stats":               ramstore.Global.Stats(),
	})
}

// BotGovernanceHandler updates store open/holiday/admin gates in RAM for quick local testing or Central push.
func BotGovernanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}
	var req struct {
		IsOpen                  *bool    `json:"is_open"`
		HolidayMode             *bool    `json:"holiday_mode"`
		HolidayMessage          *string  `json:"holiday_message"`
		OperatingHours          *string  `json:"operating_hours"`
		NextOpenTime            *string  `json:"next_open_time"`
		MinOrderAmount          *float64 `json:"min_order_amount"`
		EstimatedDeliveryTime   *string  `json:"estimated_delivery_time"`
		SubscriptionActive      *bool    `json:"subscription_active"`
		AdminAIAutoReplyAllowed *bool    `json:"admin_ai_auto_reply_allowed"`
		AdminPauseReason        *string  `json:"admin_pause_reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON body"})
		return
	}

	v := ramstore.Global.GetVendor()
	if req.IsOpen != nil {
		v.IsOpen = req.IsOpen
	}
	if req.HolidayMode != nil {
		v.HolidayMode = *req.HolidayMode
	}
	if req.HolidayMessage != nil {
		v.HolidayMessage = *req.HolidayMessage
	}
	if req.OperatingHours != nil {
		v.OperatingHours = *req.OperatingHours
	}
	if req.NextOpenTime != nil {
		v.NextOpenTime = *req.NextOpenTime
	}
	if req.MinOrderAmount != nil {
		v.MinOrderAmount = *req.MinOrderAmount
	}
	if req.EstimatedDeliveryTime != nil {
		v.EstimatedDeliveryTime = *req.EstimatedDeliveryTime
	}
	if req.SubscriptionActive != nil {
		v.SubscriptionActive = req.SubscriptionActive
	}
	if req.AdminAIAutoReplyAllowed != nil {
		v.AdminAIAutoReplyAllowed = req.AdminAIAutoReplyAllowed
	}
	if req.AdminPauseReason != nil {
		v.AdminPauseReason = *req.AdminPauseReason
	}
	ramstore.Global.UpdateVendor(v)
	notifyDashboardSync("governance")

	canReply, pauseReason := ramstore.Global.CanAutoReply()
	storeOpen, closedReason := v.IsStoreOpenNow()
	respondJSON(w, 200, map[string]interface{}{
		"status":                  "success",
		"can_auto_reply":          canReply,
		"auto_reply_pause_reason": pauseReason,
		"store_open":              storeOpen,
		"store_closed_reason":     closedReason,
	})
}

// BotResyncHandler triggers an immediate full bootstrap pull from Central Brain Node into RAM & replays retry buffer.
func BotResyncHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}

	waSession, err := central.Client.Bootstrap()
	if err != nil {
		respondJSON(w, 502, map[string]interface{}{
			"status":  "error",
			"message": "Central Brain Node sync failed: " + err.Error(),
		})
		return
	}

	// Replay any buffered outbound events missed during transient network issues
	go central.Client.ReplayRetryBuffer()

	// If WhatsApp isn't currently paired locally and Central Node returned a saved session, restore & connect it
	client := whatsapp.GetVendorClient()
	if (client == nil || client.Store == nil || client.Store.ID == nil) && waSession != nil && waSession.HasSession {
		_ = whatsapp.BootstrapWhatsAppEngine(waSession)
	}
	notifyDashboardSync("resync")

	respondJSON(w, 200, map[string]interface{}{
		"status":  "success",
		"message": "Vendor RAM state reconciled from Central Brain Node.",
		"stats":   ramstore.Global.Stats(),
	})
}

// BotConnectCentralHandler configures CENTRAL_NODE_URL + VENDOR_API_KEY at runtime and runs immediate bootstrap.
func BotConnectCentralHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}
	var req struct {
		CentralNodeURL string `json:"central_node_url"`
		VendorAPIKey   string `json:"vendor_api_key"`
		BotPublicURL   string `json:"bot_public_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON payload: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.CentralNodeURL) == "" || strings.TrimSpace(req.VendorAPIKey) == "" {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "central_node_url and vendor_api_key are required"})
		return
	}

	central.Client.Configure(req.CentralNodeURL, req.VendorAPIKey, req.BotPublicURL, 0)
	waSession, err := central.Client.Bootstrap()
	if err != nil {
		respondJSON(w, 502, map[string]interface{}{
			"status":  "error",
			"message": "Connected credentials saved, but bootstrap request failed: " + err.Error(),
		})
		return
	}
	_ = whatsapp.BootstrapWhatsAppEngine(waSession)
	notifyDashboardSync("connect")

	respondJSON(w, 200, map[string]interface{}{
		"status":  "success",
		"message": "Connected to Central Brain Node and hydrated RAM store.",
		"stats":   ramstore.Global.Stats(),
	})
}

// --- RAM Cache Inspector Endpoints ---

func RAMProductsHandler(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, 200, map[string]interface{}{
		"status":   "success",
		"products": ramstore.Global.ListProducts(false),
	})
}

func RAMCustomersHandler(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, 200, map[string]interface{}{
		"status":    "success",
		"customers": ramstore.Global.ListCustomers(),
	})
}

func RAMCartsHandler(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, 200, map[string]interface{}{
		"status": "success",
		"carts":  ramstore.Global.ListActiveCarts(),
	})
}

func RAMOrdersHandler(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, 200, map[string]interface{}{
		"status": "success",
		"orders": ramstore.Global.ListOrders(),
	})
}

func RAMLeadsHandler(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, 200, map[string]interface{}{
		"status": "success",
		"leads":  ramstore.Global.ListLeads(),
	})
}

// --- Inbound Webhooks: Central Brain Node -> Vendor Bot Node (Website -> RAM -> WhatsApp) ---

func notifyDashboardSync(section string) {
	whatsapp.PushToLiveChat(ramstore.Global.GetVendor().Username, "", section, "ram_synced")
}

func markInboundReceived(section string) {
	ramstore.Global.UpdateSyncStatus(func(st *ramstore.SyncStatus) {
		st.InboundReceived++
		st.LastSyncAt = time.Now()
		st.Connected = true
	})
	notifyDashboardSync(section)
}

// SyncFullHandler accepts a full vendor state push from Central Brain Node into RAM.
// Parses every top-level section independently so a malformed item in one section never blocks hydrating the rest.
func SyncFullHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	if err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Failed to read request body: " + err.Error()})
		return
	}
	decoded, warnings, err := ramstore.DecodeBootstrapPayload(bodyBytes)
	if err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON body: " + err.Error()})
		return
	}

	ramstore.Global.HydrateFromBootstrap(decoded)
	if decoded.WhatsAppSession != nil && decoded.WhatsAppSession.HasSession {
		client := whatsapp.GetVendorClient()
		if client == nil || client.Store == nil || client.Store.ID == nil {
			_ = whatsapp.BootstrapWhatsAppEngine(&central.BootstrapWhatsAppSession{
				HasSession:        decoded.WhatsAppSession.HasSession,
				Status:            decoded.WhatsAppSession.Status,
				DeviceJID:         decoded.WhatsAppSession.DeviceJID,
				Phone:             decoded.WhatsAppSession.Phone,
				PushName:          decoded.WhatsAppSession.PushName,
				SessionBlobBase64: decoded.WhatsAppSession.SessionBlobBase64,
			})
		}
	}

	markInboundReceived("full")
	resp := map[string]interface{}{
		"status": "success",
		"stats":  ramstore.Global.Stats(),
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	respondJSON(w, 200, resp)
}

// SyncVendorHandler updates vendor profile, governance flags, AI multi-key pool, or payment config in RAM.
func SyncVendorHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Failed to read request body: " + err.Error()})
		return
	}
	decoded, warnings, err := ramstore.DecodeBootstrapPayload(bodyBytes)
	if err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON body: " + err.Error()})
		return
	}
	if decoded.Vendor != nil {
		ramstore.Global.UpdateVendor(*decoded.Vendor)
	}
	if decoded.AI != nil {
		ramstore.Global.SetAI(*decoded.AI)
	}
	if decoded.Payment != nil {
		ramstore.Global.SetPayment(*decoded.Payment)
	}
	markInboundReceived("vendor")
	resp := map[string]interface{}{
		"status":  "success",
		"message": "Vendor configuration synced to RAM",
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	respondJSON(w, 200, resp)
}

// SyncProductsHandler replaces or upserts/deletes products (with variants, image URLs & live stock) in RAM.
func SyncProductsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 12*1024*1024))
	if err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Failed to read request body: " + err.Error()})
		return
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON body: " + err.Error()})
		return
	}

	action := strings.ToLower(ramstore.ParseFlexString(rawMap["action"]))
	productID := ramstore.ParseFlexString(rawMap["product_id"])
	if productID == "" {
		productID = ramstore.ParseFlexString(rawMap["id"])
	}

	var warnings []string
	var products []ramstore.Product
	hasProductsKey := false
	if pRaw, ok := rawMap["products"]; ok && len(pRaw) > 0 && string(pRaw) != "null" {
		hasProductsKey = true
		var wList []string
		products, wList = ramstore.DecodeProducts(pRaw)
		warnings = append(warnings, wList...)
	}
	var singleProd *ramstore.Product
	if pRaw, ok := rawMap["product"]; ok && len(pRaw) > 0 && string(pRaw) != "null" {
		p, err := ramstore.DecodeProduct(pRaw)
		if err != nil {
			respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid product JSON: " + err.Error()})
			return
		}
		singleProd = &p
	}

	switch action {
	case "delete":
		if productID != "" {
			ramstore.Global.DeleteProduct(productID)
		}
	case "upsert":
		if singleProd != nil {
			ramstore.Global.UpsertProduct(*singleProd)
		}
		for _, p := range products {
			ramstore.Global.UpsertProduct(p)
		}
	default:
		if hasProductsKey {
			ramstore.Global.ReplaceProducts(products)
		} else if singleProd != nil {
			ramstore.Global.UpsertProduct(*singleProd)
		}
	}

	markInboundReceived("products")
	resp := map[string]interface{}{
		"status":        "success",
		"product_count": len(ramstore.Global.ListProducts(false)),
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	respondJSON(w, 200, resp)
}

// SyncCustomerHandler stores new/updated website customer details & addresses directly into RAM.
func SyncCustomerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Failed to read request body: " + err.Error()})
		return
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON body: " + err.Error()})
		return
	}
	targetRaw := json.RawMessage(bodyBytes)
	if cRaw, ok := rawMap["customer"]; ok && len(cRaw) > 0 && string(cRaw) != "null" {
		targetRaw = cRaw
	}
	target, err := ramstore.DecodeCustomer(targetRaw)
	if err != nil || target == nil {
		msg := "Invalid customer JSON"
		if err != nil {
			msg = "Invalid customer JSON: " + err.Error()
		}
		respondJSON(w, 400, map[string]string{"status": "error", "message": msg})
		return
	}
	if target.Source == "" {
		target.Source = "website"
	}
	saved := ramstore.Global.UpsertCustomer(target)
	markInboundReceived("customer")
	respondJSON(w, 200, map[string]interface{}{
		"status":   "success",
		"customer": saved,
	})
}

// SyncCartHandler syncs website customer cart changes (including numeric/string product IDs & variant details) into RAM.
func SyncCartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Failed to read request body: " + err.Error()})
		return
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON body: " + err.Error()})
		return
	}

	phone := ramstore.ParseFlexString(rawMap["customer_phone"])
	if phone == "" {
		phone = ramstore.ParseFlexString(rawMap["phone"])
	}
	if phone == "" {
		phone = ramstore.ParseFlexString(rawMap["wa_number"])
	}
	if phone == "" {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "customer_phone is required"})
		return
	}

	replaceAll := ramstore.ParseFlexBool(rawMap["replace"], true)
	items, warnings := ramstore.DecodeCartItems(rawMap["items"])
	for i := range items {
		if items[i].Source == "" {
			items[i].Source = "website"
		}
	}

	if len(items) == 0 && replaceAll {
		ramstore.Global.ClearCart(phone)
		markInboundReceived("cart")
		respondJSON(w, 200, map[string]interface{}{"status": "success", "items": []interface{}{}, "total_amount": 0})
		return
	}

	updated, total := ramstore.Global.UpsertCartItems(phone, items, replaceAll)
	markInboundReceived("cart")
	resp := map[string]interface{}{
		"status":       "success",
		"items":        updated,
		"total_amount": total,
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	respondJSON(w, 200, resp)
}

// SyncOrderHandler receives website order creations or authoritative delivery partner fulfillment status updates from Central Brain Node.
// Updates RAM authoritatively (preventing stale bot state from overwriting Central status) and pushes live WhatsApp alerts.
func SyncOrderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Failed to read request body: " + err.Error()})
		return
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON body: " + err.Error()})
		return
	}

	eventName := ramstore.ParseFlexString(rawMap["event"])
	notifyPtr := ramstore.ParseFlexBoolPtr(rawMap["notify_whatsapp"])

	orderRaw := json.RawMessage(bodyBytes)
	if oRaw, ok := rawMap["order"]; ok && len(oRaw) > 0 && string(oRaw) != "null" {
		orderRaw = oRaw
	}
	ord, err := ramstore.DecodeOrder(orderRaw)
	if err != nil || ord == nil {
		msg := "Invalid order JSON"
		if err != nil {
			msg = "Invalid order JSON: " + err.Error()
		}
		respondJSON(w, 400, map[string]string{"status": "error", "message": msg})
		return
	}
	if strings.TrimSpace(ord.OrderID) == "" {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "order_id is required"})
		return
	}
	if ord.Source == "" {
		ord.Source = "website"
	}

	// Central Node is strictly authoritative for order fulfillment status & delivery partner fields
	saved, statusChanged := ramstore.Global.UpsertOrderFromCentral(ord)
	if saved != nil && saved.CustomerPhone != "" {
		ramstore.Global.UpsertCustomer(&ramstore.Customer{
			WANumber:         saved.CustomerPhone,
			JID:              saved.JID,
			FullName:         saved.CustomerName,
			DeliveryAddress:  saved.DeliveryAddress,
			ContactPhone:     saved.DeliveryPhone,
			LastOrderDetails: fmt.Sprintf("Order #%s (₹%.2f • %s)", saved.OrderID, saved.TotalAmount, saved.OrderStatus),
			Source:           saved.Source,
		})
		if strings.EqualFold(eventName, "order.created") || strings.EqualFold(saved.OrderStatus, "CONFIRMED") {
			ramstore.Global.ClearCart(saved.CustomerPhone)
		}
	}

	markInboundReceived("order")

	// Notify customer on WhatsApp when Central Brain confirms an order / verifies payment OR updates delivery status
	ev := strings.ToLower(strings.TrimSpace(eventName))
	isPaidOrConfirmedPush := ev == "order.paid" || ev == "order.confirmed" ||
		(saved != nil && (strings.EqualFold(saved.PaymentStatus, "PAID") || strings.EqualFold(saved.OrderStatus, "CONFIRMED")))
	isCancelledPush := ev == "order.cancelled" ||
		(saved != nil && (strings.EqualFold(saved.OrderStatus, "CANCELLED") || strings.EqualFold(saved.PaymentStatus, "CANCELLED")))

	// Stop any active 3-second payment polling loop when order.paid / order.confirmed / cancelled arrives
	if saved != nil && (isPaidOrConfirmedPush || isCancelledPush) {
		whatsapp.StopPaymentVerificationPoller(saved.OrderID)
	}

	isFinalConfirmation := ev == "order.created" || isPaidOrConfirmedPush

	// Extract optional txn_id from webhook body or nested order object
	txnID := ramstore.ParseFlexString(rawMap["txn_id"])
	if txnID == "" {
		txnID = ramstore.ParseFlexString(rawMap["transaction_id"])
	}
	if txnID == "" {
		if oRaw, ok := rawMap["order"]; ok && len(oRaw) > 0 {
			var ordMap map[string]json.RawMessage
			if json.Unmarshal(oRaw, &ordMap) == nil {
				txnID = ramstore.ParseFlexString(ordMap["txn_id"])
				if txnID == "" {
					txnID = ramstore.ParseFlexString(ordMap["transaction_id"])
				}
			}
		}
	}

	if saved != nil && saved.CustomerPhone != "" {
		vendorUser := ramstore.Global.GetVendor().Username
		if ev == "order.paid" || ev == "order.confirmed" {
			_ = whatsapp.SetCustomerState(vendorUser, saved.CustomerPhone, "ORDER_CONFIRMED")
			if strings.EqualFold(saved.PaymentStatus, "PAID") || ev == "order.paid" {
				_ = whatsapp.SetCustomerPaymentStatus(vendorUser, saved.CustomerPhone, "PAID")
			}
			_ = whatsapp.SetCustomerCurrentOrderID(vendorUser, saved.CustomerPhone, "")
			ramstore.Global.ClearCart(saved.CustomerPhone)

			// Strict deduplication: only send WhatsApp confirmation if notify_whatsapp === true
			// AND order_id is NOT already in notifiedPaidOrders
			if notifyPtr != nil && *notifyPtr && whatsapp.MarkOrderPaidNotifiedOnce(saved.OrderID) {
				go func(o *ramstore.Order, tx string) {
					whatsapp.SendPaidOrderConfirmationWhatsApp(o.OrderID, o.CustomerPhone, o.CustomerName, o.DeliveryAddress, tx, o.TotalAmount)
				}(saved, txnID)
			}
		} else {
			shouldNotify := statusChanged || isFinalConfirmation
			if notifyPtr != nil {
				shouldNotify = *notifyPtr
			}
			if shouldNotify {
				if isFinalConfirmation {
					_ = whatsapp.SetCustomerState(vendorUser, saved.CustomerPhone, "ORDER_CONFIRMED")
					if strings.EqualFold(saved.PaymentStatus, "PAID") {
						_ = whatsapp.SetCustomerPaymentStatus(vendorUser, saved.CustomerPhone, "PAID")
					}
					_ = whatsapp.SetCustomerCurrentOrderID(vendorUser, saved.CustomerPhone, "")
					ramstore.Global.ClearCart(saved.CustomerPhone)
					if whatsapp.MarkOrderPaidNotifiedOnce(saved.OrderID) {
						go func(o *ramstore.Order) {
							_ = whatsapp.TriggerWebOrderConfirmationPush(vendorUser, o.CustomerPhone, o.OrderID, o.TotalAmount)
						}(saved)
					}
				} else if saved.OrderStatus != "" {
					go func(o *ramstore.Order) {
						_ = whatsapp.SendOrderStatusUpdateWhatsApp(vendorUser, o.JID, o.CustomerPhone, o.OrderID, o.OrderStatus)
					}(saved)
				}
			}
		}
	}

	respondJSON(w, 200, map[string]interface{}{
		"status":         "success",
		"status_changed": statusChanged,
		"order":          saved,
	})
}

// SyncSendMessageHandler lets Central Brain Node send an automated WhatsApp message/image via this vendor bot.
func SyncSendMessageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		respondJSON(w, 405, map[string]string{"status": "error", "message": "Method not allowed"})
		return
	}
	var req struct {
		Phone    string `json:"phone"`
		Message  string `json:"message"`
		ImageURL string `json:"image_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, 400, map[string]string{"status": "error", "message": "Invalid JSON body: " + err.Error()})
		return
	}
	client := whatsapp.GetAnyLoggedInClient()
	if client == nil {
		respondJSON(w, 503, map[string]string{"status": "error", "message": "Vendor WhatsApp is not connected"})
		return
	}

	var err error
	if req.ImageURL != "" {
		err = whatsapp.SendImageFromURL(client, req.Phone, req.ImageURL, req.Message)
	} else {
		err = whatsapp.SendMessage(client, req.Phone, req.Message)
	}
	if err != nil {
		respondJSON(w, 500, map[string]string{"status": "error", "message": err.Error()})
		return
	}

	vendorUser := ramstore.Global.GetVendor().Username
	whatsapp.PushToLiveChat(vendorUser, req.Phone, req.Message, "outgoing_ai")
	go whatsapp.SaveCustomerChatAndPrune(vendorUser, req.Phone, "assistant", req.Message)
	markInboundReceived("send_message")

	respondJSON(w, 200, map[string]string{"status": "success", "message": "Delivered via vendor WhatsApp"})
}
