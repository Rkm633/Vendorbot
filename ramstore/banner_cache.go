package ramstore

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// ULTRA-LOW-RAM BLANK BANNER RAW BYTES CACHE (Render Free Tier 512MB Safe)
// Stores ONLY the raw compressed JPEG/PNG bytes (~150-300 KB) — NEVER a decoded
// RGBA bitmap — and frees memory immediately if banner_template is disabled.
// =============================================================================

const maxBlankBannerBytes = 5 * 1024 * 1024 // 5 MB hard safety cap

type blankBannerCache struct {
	mu              sync.RWMutex
	rawBytes        []byte
	cachedURL       string
	cachedUpdatedAt string
	fetchingKey     string
}

var GlobalBannerCache = &blankBannerCache{}

// Clear frees cached banner bytes immediately so Go GC can reclaim RAM.
func (c *blankBannerCache) Clear() {
	c.mu.Lock()
	had := len(c.rawBytes) > 0
	c.rawBytes = nil
	c.cachedURL = ""
	c.cachedUpdatedAt = ""
	c.fetchingKey = ""
	c.mu.Unlock()
	if had {
		log.Printf("🧹 [Banner Cache] Cleared cached blank banner bytes from RAM")
	}
}

// Stats returns lightweight cache metadata for the dashboard overview.
func (c *blankBannerCache) Stats() (cached bool, sizeBytes int, url string, updatedAt string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.rawBytes) > 0, len(c.rawBytes), c.cachedURL, c.cachedUpdatedAt
}

// SyncAsync refreshes or clears the compressed blank banner bytes in RAM whenever
// bootstrap or /api/sync/vendor or /api/sync/full updates payment.banner_template.
func (c *blankBannerCache) SyncAsync(tpl BannerTemplate) {
	cleanURL := strings.TrimSpace(tpl.BannerURL)
	cleanUpd := strings.TrimSpace(tpl.UpdatedAt)

	if !tpl.Enabled || cleanURL == "" {
		c.Clear()
		return
	}

	key := cleanURL + "|" + cleanUpd
	c.mu.Lock()
	if len(c.rawBytes) > 0 && c.cachedURL == cleanURL && c.cachedUpdatedAt == cleanUpd {
		c.mu.Unlock()
		return
	}
	if c.fetchingKey == key {
		c.mu.Unlock()
		return
	}
	c.fetchingKey = key
	c.mu.Unlock()

	go func(targetURL, targetUpd, targetKey string) {
		raw, err := downloadCompressedBanner(targetURL)
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.fetchingKey == targetKey {
			c.fetchingKey = ""
		}
		if err != nil {
			log.Printf("⚠️ [Banner Cache] Failed downloading blank banner (%s): %v", targetURL, err)
			return
		}
		c.rawBytes = raw
		c.cachedURL = targetURL
		c.cachedUpdatedAt = targetUpd
		log.Printf("🖼️ [Banner Cache] Cached blank banner compressed bytes in RAM (%.1f KB, updated_at=%s)", float64(len(raw))/1024.0, targetUpd)
	}(cleanURL, cleanUpd, key)
}

// GetOrFetchBytes returns the cached compressed blank banner bytes if available,
// or downloads and caches them once on-demand if enabled and not yet fetched.
func (c *blankBannerCache) GetOrFetchBytes(tpl BannerTemplate) ([]byte, error) {
	cleanURL := strings.TrimSpace(tpl.BannerURL)
	cleanUpd := strings.TrimSpace(tpl.UpdatedAt)

	if !tpl.Enabled || cleanURL == "" {
		c.Clear()
		return nil, nil
	}

	c.mu.RLock()
	if len(c.rawBytes) > 0 && c.cachedURL == cleanURL && c.cachedUpdatedAt == cleanUpd {
		cp := make([]byte, len(c.rawBytes))
		copy(cp, c.rawBytes)
		c.mu.RUnlock()
		return cp, nil
	}
	c.mu.RUnlock()

	raw, err := downloadCompressedBanner(cleanURL)
	if err != nil {
		// Fall back to any previously cached banner bytes for same URL if transient network error occurs
		c.mu.RLock()
		if len(c.rawBytes) > 0 && c.cachedURL == cleanURL {
			cp := make([]byte, len(c.rawBytes))
			copy(cp, c.rawBytes)
			c.mu.RUnlock()
			return cp, nil
		}
		c.mu.RUnlock()
		return nil, err
	}

	c.mu.Lock()
	c.rawBytes = raw
	c.cachedURL = cleanURL
	c.cachedUpdatedAt = cleanUpd
	c.fetchingKey = ""
	c.mu.Unlock()

	log.Printf("🖼️ [Banner Cache] On-demand cached blank banner bytes in RAM (%.1f KB)", float64(len(raw))/1024.0)
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return cp, nil
}

func downloadCompressedBanner(bannerURL string) ([]byte, error) {
	req, err := http.NewRequest("GET", bannerURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "VendorWhatsAppBotNode/2.5")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d downloading blank banner", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBlankBannerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty blank banner response")
	}
	if len(raw) > maxBlankBannerBytes {
		return nil, fmt.Errorf("blank banner exceeds 5MB RAM safety limit")
	}
	return raw, nil
}
