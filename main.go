package main

import (
	"context"
	"embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"wa-server/api"
	"wa-server/central"
	"wa-server/ramstore"
	"wa-server/whatsapp"
)

//go:embed public/index.html
var embeddedFS embed.FS

func serveDashboard(w http.ResponseWriter, r *http.Request) {
	diskPath := path.Join("public", "index.html")
	if fi, err := os.Stat(diskPath); err == nil && !fi.IsDir() {
		http.ServeFile(w, r, diskPath)
		return
	}
	data, err := embeddedFS.ReadFile("public/index.html")
	if err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	http.NotFound(w, r)
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Auto-detect public URL from reverse-proxy Host / X-Forwarded-Host if platform env var wasn't injected
		central.Client.DetectPublicURLFromRequest(r)

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Authorization, X-Vendor-API-Key")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func main() {
	log.Println("🚀 Starting Stateless Vendor WhatsApp Auto-Responder Bot Node v2.3 (Zero-DB, Zero-Polling, Zero-Chat-Sync, RAM-First)...")

	// Memory & GC tuning for Render / Railway / Koyeb 512MB free-tier containers
	debug.SetGCPercent(20)
	debug.SetMemoryLimit(400 * 1024 * 1024)

	// 1. Initialize Central Brain Node Sync Client (Vendor only needs CENTRAL_NODE_URL + VENDOR_API_KEY; public URL auto-detected)
	central.InitFromEnv()

	// 2. One-Time Startup Bootstrap: Fetch vendor business details, AI settings (with backup keys), products (variants+stock+image URLs),
	// customers, active carts, recent orders & saved WhatsMeow session into RAM (zero chat sync, zero periodic polling).
	centralSession, err := central.Client.Bootstrap()
	if err != nil {
		log.Printf("⚠️ [Startup Bootstrap Notice] %v", err)
	}

	// 3. Initialize WhatsApp Engine:
	// - If Central Brain returned a saved session, restore & auto-reconnect without prompting login
	// - If no session exists, enter Needs_Login mode so vendor can pair exclusively from this bot's dashboard
	if err := whatsapp.BootstrapWhatsAppEngine(centralSession); err != nil {
		log.Printf("⚠️ [WhatsApp Engine Boot Error] %v", err)
	}

	// 3b. If Central Node was cold-starting/unreachable at boot, keep retrying startup bootstrap ONLY until first hydration succeeds,
	// then automatically restore WhatsApp session (if not already paired locally) and stop permanently (zero polling).
	if err != nil && central.Client.IsConfigured() {
		central.Client.StartStartupBootstrapRetry(func(waSession *central.BootstrapWhatsAppSession) {
			if waSession != nil && waSession.HasSession {
				client := whatsapp.GetVendorClient()
				if client == nil || client.Store == nil || client.Store.ID == nil {
					if bootErr := whatsapp.BootstrapWhatsAppEngine(waSession); bootErr != nil {
						log.Printf("⚠️ [Delayed WhatsApp Restore Error] %v", bootErr)
					}
				}
			}
			whatsapp.PushToLiveChat(ramstore.Global.GetVendor().Username, "", "bootstrap", "ram_synced")
		})
	}

	// 4. Start local WhatsApp WebSocket keep-alive monitor & lightweight daily admin renewal reminder scheduler
	whatsapp.StartKeepAliveMonitor()
	whatsapp.StartAdminRenewalReminderScheduler()

	// 5. Register HTTP Routes
	mux := http.NewServeMux()

	// Dashboard UI
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		central.Client.DetectPublicURLFromRequest(r)
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/webhook/") {
			http.NotFound(w, r)
			return
		}
		serveDashboard(w, r)
	})

	// Health, Governance & Bot Overview / RAM Inspector
	mux.HandleFunc("/api/health", corsMiddleware(api.HealthHandler))
	mux.HandleFunc("/api/bot/overview", corsMiddleware(api.BotOverviewHandler))
	mux.HandleFunc("/api/bot/governance", corsMiddleware(api.BotGovernanceHandler))
	mux.HandleFunc("/api/bot/resync", corsMiddleware(api.BotResyncHandler))
	mux.HandleFunc("/api/bot/connect-central", corsMiddleware(api.BotConnectCentralHandler))
	mux.HandleFunc("/api/ram/products", corsMiddleware(api.RAMProductsHandler))
	mux.HandleFunc("/api/ram/customers", corsMiddleware(api.RAMCustomersHandler))
	mux.HandleFunc("/api/ram/carts", corsMiddleware(api.RAMCartsHandler))
	mux.HandleFunc("/api/ram/orders", corsMiddleware(api.RAMOrdersHandler))
	mux.HandleFunc("/api/ram/leads", corsMiddleware(api.RAMLeadsHandler))

	// Central Brain Node -> Vendor Bot Node Real-Time Sync Webhooks (Authenticated via VENDOR_API_KEY)
	mux.HandleFunc("/api/sync/full", corsMiddleware(api.RequireCentralAuth(api.SyncFullHandler)))
	mux.HandleFunc("/api/sync/vendor", corsMiddleware(api.RequireCentralAuth(api.SyncVendorHandler)))
	mux.HandleFunc("/api/sync/products", corsMiddleware(api.RequireCentralAuth(api.SyncProductsHandler)))
	mux.HandleFunc("/api/sync/customer", corsMiddleware(api.RequireCentralAuth(api.SyncCustomerHandler)))
	mux.HandleFunc("/api/sync/cart", corsMiddleware(api.RequireCentralAuth(api.SyncCartHandler)))
	mux.HandleFunc("/api/sync/order", corsMiddleware(api.RequireCentralAuth(api.SyncOrderHandler)))
	mux.HandleFunc("/api/sync/send-message", corsMiddleware(api.RequireCentralAuth(api.SyncSendMessageHandler)))

	// Vendor Bot Dashboard-Only WhatsApp Login (QR Code, 8-Digit Pairing & Unlink)
	mux.HandleFunc("/api/whatsapp/status", corsMiddleware(api.StatusHandler))
	mux.HandleFunc("/api/whatsapp/qr", corsMiddleware(api.StatusHandler))
	mux.HandleFunc("/api/status", corsMiddleware(api.StatusHandler))
	mux.HandleFunc("/api/whatsapp/pair", corsMiddleware(api.PairPhoneHandler))
	mux.HandleFunc("/api/pair", corsMiddleware(api.PairPhoneHandler))
	mux.HandleFunc("/api/whatsapp/logout", corsMiddleware(api.LogoutWAHandler))
	mux.HandleFunc("/api/whatsapp/unlink", corsMiddleware(api.LogoutWAHandler))

	// AI Auto-Responder Settings & Interactive Playground
	mux.HandleFunc("/api/ai/settings", corsMiddleware(api.AISettingsHandler))
	mux.HandleFunc("/api/ai/test", corsMiddleware(api.AITestHandler))
	mux.HandleFunc("/api/ai/test-lead-alert", corsMiddleware(api.TestLeadAlertHandler))

	// Live Chat & Real-Time SSE
	mux.HandleFunc("/api/chats", corsMiddleware(api.ChatsHandler))
	mux.HandleFunc("/api/chats/messages", corsMiddleware(api.ChatMessagesHandler))
	mux.HandleFunc("/api/chats/send", corsMiddleware(api.ChatSendHandler))
	mux.HandleFunc("/api/chat/stream", api.LiveChatSSEHandler)

	// Outbound Messaging & Broadcasts
	mux.HandleFunc("/api/send", corsMiddleware(api.SendHandler))
	mux.HandleFunc("/api/bulk", corsMiddleware(api.BulkSendHandler))
	mux.HandleFunc("/api/bulk/send", corsMiddleware(api.BulkSendHandler))
	mux.HandleFunc("/api/bulk/status", corsMiddleware(api.BulkStatusHandler))
	mux.HandleFunc("/api/bulk/upload-temp", corsMiddleware(api.BulkUploadTempHandler))

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	// Zero-Downtime Redeploy Safe Shutdown Sequence:
	// 1. Stop accepting new HTTP requests
	// 2. Stop background sync loops
	// 3. Flush all pending outbound events + retry buffer to Central Brain Node
	// 4. Cleanly disconnect WhatsApp WebSocket (preventing stream-replaced logout on Render/Koyeb redeploy)
	// 5. Upload final validated SQLite session snapshot ONLY if paired
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	doneChan := make(chan struct{})
	go func() {
		defer close(doneChan)
		sig := <-sigChan
		log.Printf("🛑 Received %v — executing zero-downtime graceful shutdown...", sig)

		shutCtx, shutCancel := context.WithTimeout(context.Background(), 4*time.Second)
		_ = srv.Shutdown(shutCtx)
		shutCancel()

		central.Client.FlushPendingEvents(6 * time.Second)
		whatsapp.GracefulShutdownWhatsApp()
		log.Println("👋 Zero-downtime graceful shutdown complete.")
	}()

	log.Printf("✅ Stateless Bot Node listening on port %s (Mode: %s | Public URL: %s)", port, ramstore.Global.GetBotMode(), ramstore.Global.GetSyncStatus().BotPublicURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	<-doneChan
}
