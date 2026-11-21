package whatsapp

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wa-server/central"
	"wa-server/ramstore"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

const ephemeralSessionPath = "/tmp/vendor_wa_session.db"

var (
	sessionFileMu       sync.Mutex
	ephemeralSQLDB      *sql.DB
	sessionLoggedOut    atomic.Bool
	initialPairUploaded atomic.Bool
)

// InitEphemeralSessionStore restores any saved whatsmeow session blob from Central Brain Node
// into an ephemeral local container file (/tmp/vendor_wa_session.db) with zero external database dependency.
func InitEphemeralSessionStore(centralSession *central.BootstrapWhatsAppSession) error {
	sessionFileMu.Lock()
	defer sessionFileMu.Unlock()

	if ephemeralSQLDB != nil {
		_ = ephemeralSQLDB.Close()
		ephemeralSQLDB = nil
	}

	_ = os.MkdirAll(filepath.Dir(ephemeralSessionPath), 0o755)
	sessionLoggedOut.Store(false)
	initialPairUploaded.Store(false)

	restored := false
	if centralSession != nil && centralSession.HasSession && strings.TrimSpace(centralSession.SessionBlobBase64) != "" {
		if err := writeSessionBlobToDisk(centralSession.SessionBlobBase64); err != nil {
			log.Printf("⚠️ [WhatsApp Session Restore] Failed decoding Central Node session blob: %v (starting fresh)", err)
			_ = os.Remove(ephemeralSessionPath)
		} else {
			restored = true
			log.Printf("✅ [WhatsApp Session Restore] Hydrated persistent whatsmeow session from Central Brain Node (%s).", centralSession.Phone)
		}
	} else {
		_ = os.Remove(ephemeralSessionPath)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)", ephemeralSessionPath)
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed opening ephemeral whatsmeow session container: %w", err)
	}
	rawDB.SetMaxOpenConns(1)
	rawDB.SetMaxIdleConns(1)

	dbLog := waLog.Stdout("WhatsAppSession", "ERROR", false)
	container := sqlstore.NewWithDB(rawDB, "sqlite3", dbLog)
	if err := container.Upgrade(context.Background()); err != nil {
		_ = rawDB.Close()
		return fmt.Errorf("failed upgrading whatsmeow session schema: %w", err)
	}

	ephemeralSQLDB = rawDB
	StoreContainer = container

	if restored {
		go LoadLIDMapCache()
	}

	return nil
}

func writeSessionBlobToDisk(blobBase64 string) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(blobBase64))
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return fmt.Errorf("empty session blob")
	}

	var sqliteBytes []byte
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		defer gz.Close()
		sqliteBytes, err = io.ReadAll(io.LimitReader(gz, 64*1024*1024))
		if err != nil {
			return err
		}
	} else {
		sqliteBytes = raw
	}

	// Verify SQLite magic header ("SQLite format 3\x00") before accepting
	if len(sqliteBytes) < 16 || string(sqliteBytes[:15]) != "SQLite format 3" {
		return fmt.Errorf("invalid sqlite header in session blob")
	}

	tmpPath := ephemeralSessionPath + ".restore"
	if err := os.WriteFile(tmpPath, sqliteBytes, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, ephemeralSessionPath)
}

// ExportSessionBlobBase64 snapshots the local ephemeral whatsmeow session file as gzip+base64
// after verifying it is a valid, non-empty SQLite file.
func ExportSessionBlobBase64() (string, error) {
	sessionFileMu.Lock()
	defer sessionFileMu.Unlock()

	data, err := os.ReadFile(ephemeralSessionPath)
	if err != nil {
		return "", err
	}
	if len(data) < 4096 || string(data[:15]) != "SQLite format 3" {
		return "", fmt.Errorf("ephemeral session file is empty or incomplete")
	}

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := gz.Write(data); err != nil {
		_ = gz.Close()
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// SyncSessionToCentralNow validates that the session is paired and non-empty before uploading to Central Brain Node.
// Never overwrites a valid Central session if the local session is empty or logged out.
func SyncSessionToCentralNow(deviceJID, phone, pushName, status string) {
	if sessionLoggedOut.Load() {
		log.Println("🛡️ [Session Sync Guard] Skipping upload because device is explicitly logged out.")
		return
	}
	if strings.TrimSpace(deviceJID) == "" {
		return
	}
	cleanPhone := FormatPhone(phone)
	blob, err := ExportSessionBlobBase64()
	if err != nil || strings.TrimSpace(blob) == "" {
		log.Printf("⚠️ [Session Sync Guard] Skipping upload — session blob invalid/empty: %v", err)
		return
	}

	ramstore.Global.UpdateWhatsAppSession(func(ws *ramstore.WhatsAppSessionState) {
		ws.HasSession = true
		ws.NeedsLogin = false
		ws.Status = status
		ws.DeviceJID = deviceJID
		ws.Phone = cleanPhone
		if pushName != "" {
			ws.PushName = pushName
		}
		ws.QRCode = ""
		ws.SessionBlobBase64 = blob
	})

	if err := central.Client.SaveWhatsAppSession(deviceJID, cleanPhone, pushName, status, blob); err != nil {
		log.Printf("⚠️ [Session Sync] Central Brain Node upload failed: %v", err)
	}
}

// SyncSessionOnFirstLogin uploads the WhatsMeow session snapshot ONCE immediately after first-time QR or 8-digit pairing completes.
func SyncSessionOnFirstLogin(deviceJID, phone, pushName, status string) {
	if !initialPairUploaded.CompareAndSwap(false, true) {
		return
	}
	SyncSessionToCentralNow(deviceJID, phone, pushName, status)
}

// GracefulShutdownWhatsApp performs zero-downtime redeploy shutdown on Render / Railway / Koyeb:
// 1. Cleanly disconnects WhatsApp WebSocket FIRST so the new container won't hit a multi-device session conflict.
// 2. Only if the device was legitimately paired (Store.ID != nil and not logged out), snapshots & uploads the final valid SQLite state.
func GracefulShutdownWhatsApp() {
	client := GetVendorClient()
	if client == nil {
		return
	}

	var deviceJID, phone, pushName string
	hasValidPairedSession := !sessionLoggedOut.Load() && client.Store != nil && client.Store.ID != nil
	if hasValidPairedSession {
		deviceJID = client.Store.ID.String()
		phone = client.Store.ID.User
		pushName = client.Store.PushName
	}

	// Step A: Cleanly disconnect WhatsApp WebSocket BEFORE saving snapshot so no further ratchet writes race the file read
	// and so the incoming container during Render/Koyeb rolling deploy doesn't experience dual-connection logout conflicts.
	if client.IsConnected() {
		log.Println("🔌 [Graceful Shutdown] Cleanly disconnecting WhatsApp WebSocket before final snapshot...")
		client.Disconnect()
		time.Sleep(350 * time.Millisecond)
	}

	// Step B: Save valid snapshot ONLY if paired (never overwrite a valid Central session with an empty/logged-out DB)
	if hasValidPairedSession && deviceJID != "" {
		log.Printf("💾 [Graceful Shutdown] Uploading final valid whatsmeow session snapshot for +%s to Central Brain Node...", FormatPhone(phone))
		SyncSessionToCentralNow(deviceJID, phone, pushName, "Connected")
	} else {
		log.Println("🛡️ [Graceful Shutdown] No active paired session — skipping Central session upload to protect existing state.")
	}
}

// FlushSessionOnShutdown kept as alias for backwards compatibility.
func FlushSessionOnShutdown() {
	GracefulShutdownWhatsApp()
}

// ResetEphemeralSessionFile wipes the ephemeral session container on explicit WhatsApp logout/unlink.
func ResetEphemeralSessionFile() {
	sessionLoggedOut.Store(true)
	initialPairUploaded.Store(false)

	sessionFileMu.Lock()
	if ephemeralSQLDB != nil {
		_ = ephemeralSQLDB.Close()
		ephemeralSQLDB = nil
	}
	_ = os.Remove(ephemeralSessionPath)
	_ = os.Remove(ephemeralSessionPath + "-wal")
	_ = os.Remove(ephemeralSessionPath + "-shm")
	StoreContainer = nil
	sessionFileMu.Unlock()

	_ = InitEphemeralSessionStore(nil)
}
