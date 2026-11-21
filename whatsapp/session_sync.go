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
	sessionFileMu        sync.Mutex
	ephemeralSQLDB       *sql.DB
	sessionLoggedOut     atomic.Bool
	initialPairUploaded  atomic.Bool
	initialPairUploading atomic.Bool
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
	_ = os.Remove(ephemeralSessionPath + "-wal")
	_ = os.Remove(ephemeralSessionPath + "-shm")
	sessionLoggedOut.Store(false)
	initialPairUploading.Store(false)

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

	// If session was already restored from Central Brain Node, no need to re-upload immediately on boot
	// (GracefulShutdownWhatsApp will still upload the latest ratchet snapshot on Render redeploy/restart).
	initialPairUploaded.Store(restored)

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

func decodeBase64Flexible(rawInput string) ([]byte, error) {
	s := strings.TrimSpace(rawInput)
	if idx := strings.Index(s, ";base64,"); idx != -1 {
		s = s[idx+len(";base64,"):]
	}
	// Strip whitespace/newlines that proxies or JSON serializers may wrap
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if s == "" {
		return nil, fmt.Errorf("empty base64 string")
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func writeSessionBlobToDisk(blobBase64 string) error {
	raw, err := decodeBase64Flexible(blobBase64)
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

	_ = os.Remove(ephemeralSessionPath + "-wal")
	_ = os.Remove(ephemeralSessionPath + "-shm")

	tmpPath := ephemeralSessionPath + ".restore"
	if err := os.WriteFile(tmpPath, sqliteBytes, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, ephemeralSessionPath)
}

// ExportSessionBlobBase64 takes a consistent SQLite snapshot (via VACUUM INTO) of the local ephemeral
// whatsmeow session file and encodes it as gzip+base64 after verifying it is a valid, non-empty SQLite file.
func ExportSessionBlobBase64() (string, error) {
	sessionFileMu.Lock()
	defer sessionFileMu.Unlock()

	var data []byte
	var err error

	if ephemeralSQLDB != nil {
		snapPath := ephemeralSessionPath + ".snap"
		_ = os.Remove(snapPath)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, vacErr := ephemeralSQLDB.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s'", snapPath))
		cancel()
		if vacErr == nil {
			data, err = os.ReadFile(snapPath)
			_ = os.Remove(snapPath)
		} else {
			_ = os.Remove(snapPath)
			_, _ = ephemeralSQLDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
			data, err = os.ReadFile(ephemeralSessionPath)
		}
	} else {
		data, err = os.ReadFile(ephemeralSessionPath)
	}

	if err != nil {
		return "", err
	}
	if len(data) < 4096 || string(data[:15]) != "SQLite format 3" {
		return "", fmt.Errorf("ephemeral session file is empty or incomplete (%d bytes)", len(data))
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

func syncSessionToCentralWithResult(deviceJID, phone, pushName, status string) error {
	if sessionLoggedOut.Load() {
		log.Println("🛡️ [Session Sync Guard] Skipping upload because device is explicitly logged out.")
		return fmt.Errorf("session logged out")
	}
	if strings.TrimSpace(deviceJID) == "" {
		return fmt.Errorf("missing device_jid")
	}
	cleanPhone := FormatPhone(phone)
	blob, err := ExportSessionBlobBase64()
	if err != nil || strings.TrimSpace(blob) == "" {
		log.Printf("⚠️ [Session Sync Guard] Skipping upload — session blob invalid/empty: %v", err)
		return fmt.Errorf("invalid session blob: %w", err)
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

	if !central.Client.IsConfigured() {
		return fmt.Errorf("central brain node not configured yet")
	}
	if err := central.Client.SaveWhatsAppSession(deviceJID, cleanPhone, pushName, status, blob); err != nil {
		log.Printf("⚠️ [Session Sync] Central Brain Node upload failed: %v", err)
		return err
	}
	return nil
}

// SyncSessionToCentralNow validates that the session is paired and non-empty before uploading to Central Brain Node.
// Never overwrites a valid Central session if the local session is empty or logged out.
func SyncSessionToCentralNow(deviceJID, phone, pushName, status string) {
	_ = syncSessionToCentralWithResult(deviceJID, phone, pushName, status)
}

// SyncSessionOnFirstLogin uploads the WhatsMeow session snapshot ONCE after first-time QR or 8-digit pairing completes.
// Waits briefly for whatsmeow to flush Signal keys to SQLite, retries transient failures, and only marks uploaded on success.
func SyncSessionOnFirstLogin(deviceJID, phone, pushName, status string) {
	if initialPairUploaded.Load() || sessionLoggedOut.Load() {
		return
	}
	if !initialPairUploading.CompareAndSwap(false, true) {
		return
	}
	defer initialPairUploading.Store(false)

	// Allow whatsmeow to finish flushing Signal identity, prekeys, and device store rows into SQLite
	time.Sleep(1800 * time.Millisecond)

	backoffs := []time.Duration{0, 3 * time.Second, 6 * time.Second, 12 * time.Second}
	for idx, wait := range backoffs {
		if sessionLoggedOut.Load() || initialPairUploaded.Load() {
			return
		}
		if wait > 0 {
			time.Sleep(wait)
		}
		if err := syncSessionToCentralWithResult(deviceJID, phone, pushName, status); err == nil {
			initialPairUploaded.Store(true)
			log.Printf("✅ [First-Login Session Backup] Saved WhatsApp session (+%s) to Central Brain Node on attempt %d.", FormatPhone(phone), idx+1)
			return
		}
		log.Printf("⏳ [First-Login Session Backup] Attempt %d/%d deferred; retrying shortly...", idx+1, len(backoffs))
	}
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
