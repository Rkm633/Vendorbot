package whatsapp

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type cachedMediaEntry struct {
	ImageBytes    []byte
	JPEGThumbnail []byte
	MimeType      string
	ExpiresAt     time.Time
	LastUsedAt    time.Time
}

const (
	mediaCacheMaxEntries = 40
	mediaCacheTTL        = 30 * time.Minute
)

var (
	mediaCacheMu sync.Mutex
	mediaCache   = make(map[string]*cachedMediaEntry)
)

// FetchImageWithThumbCache downloads a product image (or returns a RAM-cached copy with generated JPEG thumbnail)
// so repeated product photo requests reply instantaneously without re-downloading.
func FetchImageWithThumbCache(imageURL string) ([]byte, []byte, string, error) {
	key := strings.TrimSpace(imageURL)
	if key == "" {
		return nil, nil, "", fmt.Errorf("empty image url")
	}

	now := time.Now()
	mediaCacheMu.Lock()
	if entry, ok := mediaCache[key]; ok && entry != nil && now.Before(entry.ExpiresAt) {
		entry.LastUsedAt = now
		imgCopy := entry.ImageBytes
		thumbCopy := entry.JPEGThumbnail
		mimeCopy := entry.MimeType
		mediaCacheMu.Unlock()
		return imgCopy, thumbCopy, mimeCopy, nil
	}
	mediaCacheMu.Unlock()

	var imgBytes []byte
	var err error
	if strings.HasPrefix(key, "data:") {
		parts := strings.SplitN(key, ",", 2)
		if len(parts) != 2 {
			return nil, nil, "", fmt.Errorf("invalid data url")
		}
		imgBytes, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, nil, "", err
		}
	} else {
		httpClient := &http.Client{Timeout: 12 * time.Second}
		resp, reqErr := httpClient.Get(key)
		if reqErr != nil {
			return nil, nil, "", reqErr
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, nil, "", fmt.Errorf("image HTTP status %d", resp.StatusCode)
		}
		imgBytes, err = io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
		if err != nil {
			return nil, nil, "", err
		}
	}

	if len(imgBytes) < 32 {
		return nil, nil, "", fmt.Errorf("downloaded image is empty")
	}
	if !bytes.HasPrefix(imgBytes, []byte("\x89PNG")) &&
		!bytes.HasPrefix(imgBytes, []byte("\xff\xd8")) &&
		!bytes.HasPrefix(imgBytes, []byte("RIFF")) &&
		!bytes.HasPrefix(imgBytes, []byte("GIF8")) {
		return nil, nil, "", fmt.Errorf("unsupported image format")
	}

	mimeType := http.DetectContentType(imgBytes)
	if !strings.HasPrefix(mimeType, "image/") {
		mimeType = "image/jpeg"
	}

	thumbBytes := generateWhatsAppJPEGThumbnail(imgBytes)
	if len(thumbBytes) == 0 {
		thumbBytes = UniversalThumbnail
	}

	// Only cache reasonable-sized product images (<= 2.5MB) to protect 512MB container RAM
	if len(imgBytes) <= 2560*1024 {
		mediaCacheMu.Lock()
		// Evict expired entries first
		for k, v := range mediaCache {
			if v == nil || now.After(v.ExpiresAt) {
				delete(mediaCache, k)
			}
		}
		// LRU eviction if at capacity
		if len(mediaCache) >= mediaCacheMaxEntries {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range mediaCache {
				if oldestKey == "" || v.LastUsedAt.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.LastUsedAt
				}
			}
			if oldestKey != "" {
				delete(mediaCache, oldestKey)
			}
		}
		mediaCache[key] = &cachedMediaEntry{
			ImageBytes:    imgBytes,
			JPEGThumbnail: thumbBytes,
			MimeType:      mimeType,
			ExpiresAt:     now.Add(mediaCacheTTL),
			LastUsedAt:    now,
		}
		mediaCacheMu.Unlock()
	}

	return imgBytes, thumbBytes, mimeType, nil
}

// generateWhatsAppJPEGThumbnail decodes JPEG/PNG images and generates a compact 96px JPEG thumbnail using Go stdlib.
func generateWhatsAppJPEGThumbnail(raw []byte) []byte {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil || src == nil {
		return nil
	}
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil
	}

	const maxDim = 96
	dstW, dstH := srcW, srcH
	if srcW >= srcH && srcW > maxDim {
		dstW = maxDim
		dstH = (srcH * maxDim) / srcW
	} else if srcH > maxDim {
		dstH = maxDim
		dstW = (srcW * maxDim) / srcH
	}
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		sy := b.Min.Y + (y*srcH)/dstH
		for x := 0; x < dstW; x++ {
			sx := b.Min.X + (x*srcW)/dstW
			dst.Set(x, y, src.At(sx, sy))
		}
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 60}); err != nil {
		return nil
	}
	return out.Bytes()
}

// GetMediaCacheSize returns the current count of cached product images & thumbnails in RAM.
func GetMediaCacheSize() int {
	mediaCacheMu.Lock()
	defer mediaCacheMu.Unlock()
	return len(mediaCache)
}
