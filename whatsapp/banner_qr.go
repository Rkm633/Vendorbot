package whatsapp

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log"
	"net/url"
	"strings"

	"wa-server/ramstore"

	qrcode "github.com/skip2/go-qrcode"
)

// =============================================================================
// LOCAL IN-MEMORY DYNAMIC BRANDED BANNER QR COMPOSITOR (Pure Go, Zero CGo)
// - Zero external QR Worker or Cloudinary dependency
// - Decodes compressed blank banner bytes temporarily onto an RGBA canvas,
//   draws a white quiet-zone, stamps the crisp QR at Central Admin coordinates,
//   encodes to JPEG (quality 85), and immediately releases all buffers for GC.
// - Falls back cleanly to a standalone 500x500 crisp PNG QR when no blank
//   banner template is cached in RAM.
// =============================================================================

// BuildUPIQRString constructs the UPI payment URI STRICTLY for QR code encoding.
// NEVER send the returned string as text or clickable link in WhatsApp messages.
func BuildUPIQRString(upiQRData, upiID, payeeName, orderID string, amount float64) string {
	if raw := strings.TrimSpace(upiQRData); raw != "" {
		return raw
	}
	vpa := strings.TrimSpace(upiID)
	if vpa == "" {
		return ""
	}
	name := strings.TrimSpace(payeeName)
	if name == "" {
		name = "Store"
	}
	ord := strings.TrimSpace(orderID)
	return fmt.Sprintf(
		"upi://pay?pa=%s&pn=%s&tr=%s&am=%.2f&cu=INR",
		vpa,
		url.QueryEscape(name),
		url.QueryEscape(ord),
		amount,
	)
}

// GenerateLocalBannerQRBytes renders the UPI QR code locally in memory and composites
// it onto the cached compressed blank banner (if enabled and cached in RAM).
// Returns (imageBytes, mimeType, error).
func GenerateLocalBannerQRBytes(upiStr string, tpl ramstore.BannerTemplate) ([]byte, string, error) {
	cleanURI := strings.TrimSpace(upiStr)
	if cleanURI == "" {
		return nil, "", fmt.Errorf("empty UPI QR data")
	}

	// Fetch compressed blank banner bytes from ultra-low-RAM cache (if enabled)
	bannerBytes, cacheErr := ramstore.GlobalBannerCache.GetOrFetchBytes(tpl)
	if cacheErr != nil {
		log.Printf("⚠️ [Local Banner QR] Blank banner fetch warning (%v) — using standalone 500x500 QR", cacheErr)
	}

	if len(bannerBytes) > 0 {
		composited, compErr := compositeQROntoBlankBanner(cleanURI, bannerBytes, tpl)
		if compErr == nil && len(composited) > 0 {
			return composited, "image/jpeg", nil
		}
		log.Printf("⚠️ [Local Banner QR] Banner composite fallback (%v) — using standalone 500x500 QR", compErr)
	}

	// Fallback: standalone crisp 500x500 PNG UPI QR
	pngBytes, err := qrcode.Encode(cleanURI, qrcode.Medium, 500)
	if err != nil {
		return nil, "", fmt.Errorf("failed generating standalone QR PNG: %w", err)
	}
	return pngBytes, "image/png", nil
}

// compositeQROntoBlankBanner decodes compressed bannerBytes temporarily, stamps the
// white quiet-zone + QR code at exact Central Admin coordinates, and encodes JPEG q=85.
func compositeQROntoBlankBanner(upiStr string, bannerBytes []byte, tpl ramstore.BannerTemplate) ([]byte, error) {
	srcImg, _, err := image.Decode(bytes.NewReader(bannerBytes))
	if err != nil {
		return nil, fmt.Errorf("decode blank banner: %w", err)
	}

	srcBounds := srcImg.Bounds()
	bannerW := srcBounds.Dx()
	bannerH := srcBounds.Dy()
	if bannerW <= 0 || bannerH <= 0 {
		return nil, fmt.Errorf("invalid blank banner dimensions %dx%d", bannerW, bannerH)
	}

	// Temporary mutable RGBA canvas (freed immediately on function return)
	canvas := image.NewRGBA(image.Rect(0, 0, bannerW, bannerH))
	draw.Draw(canvas, canvas.Bounds(), srcImg, srcBounds.Min, draw.Src)

	// Exact placement coordinates matching Central Admin Studio (and legacy g_center, y_-130, w_480, h_480)
	qrSize := tpl.QRSize
	if qrSize <= 0 {
		qrSize = ramstore.DefaultBannerQRSize
	}
	minDim := bannerW
	if bannerH < minDim {
		minDim = bannerH
	}
	if minDim > 80 && qrSize > minDim-20 {
		qrSize = minDim - 20
	}

	offsetX := tpl.QROffsetX
	offsetY := tpl.QROffsetY
	pad := tpl.QRPadding
	if pad < 0 {
		pad = 0
	}

	qrX := ((bannerW - qrSize) / 2) + offsetX
	qrY := ((bannerH - qrSize) / 2) + offsetY

	// Generate crisp QR with zero built-in border so our exact quiet-zone padding matches pixel-for-pixel
	qrObj, err := qrcode.New(upiStr, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("generate QR matrix: %w", err)
	}
	qrObj.DisableBorder = true
	qrImg := qrObj.Image(qrSize)

	// Draw solid white quiet-zone rectangle at (qrX - pad, qrY - pad, qrSize + pad*2, qrSize + pad*2)
	quietRect := image.Rect(qrX-pad, qrY-pad, qrX+qrSize+pad, qrY+qrSize+pad)
	draw.Draw(canvas, quietRect, image.NewUniform(color.White), image.Point{}, draw.Src)

	// Stamp crisp qrSize x qrSize QR code cleanly at (qrX, qrY)
	qrBounds := qrImg.Bounds()
	stampX := qrX + (qrSize-qrBounds.Dx())/2
	stampY := qrY + (qrSize-qrBounds.Dy())/2
	qrRect := image.Rect(stampX, stampY, stampX+qrBounds.Dx(), stampY+qrBounds.Dy())
	draw.Draw(canvas, qrRect, qrImg, qrBounds.Min, draw.Src)

	// Encode final composite to JPEG (quality 85) directly in memory
	var out bytes.Buffer
	if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: 85}); err != nil {
		return nil, fmt.Errorf("encode composite JPEG: %w", err)
	}
	return out.Bytes(), nil
}
