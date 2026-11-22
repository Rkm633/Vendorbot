package ramstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseFlexibleTime parses timestamps from RFC3339, RFC3339Nano, SQLite/SQL datetime
// ("YYYY-MM-DD HH:MM:SS" with optional fractional seconds or timezone), date-only strings,
// or Unix epoch numbers (seconds or milliseconds). Empty strings and null return zero time without error.
func ParseFlexibleTime(raw json.RawMessage) (time.Time, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
		return time.Time{}, nil
	}

	// Numeric Unix timestamp (seconds or milliseconds)
	if trimmed[0] != '"' {
		var num float64
		if err := json.Unmarshal(trimmed, &num); err == nil {
			if num == 0 {
				return time.Time{}, nil
			}
			if math.Abs(num) >= 1e12 {
				sec := int64(num) / 1000
				nsec := (int64(num) % 1000) * int64(time.Millisecond)
				return time.Unix(sec, nsec).UTC(), nil
			}
			return time.Unix(int64(num), 0).UTC(), nil
		}
		return time.Time{}, fmt.Errorf("invalid timestamp value: %s", string(trimmed))
	}

	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return time.Time{}, err
	}
	return ParseFlexibleTimeString(s)
}

// ParseFlexibleTimeString parses string timestamps in RFC3339, SQLite/SQL, and common ISO formats.
func ParseFlexibleTimeString(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.EqualFold(s, "null") || s == "0000-00-00 00:00:00" {
		return time.Time{}, nil
	}

	// Numeric string epoch
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && len(s) >= 9 {
		if n >= 1e12 {
			return time.Unix(n/1000, (n%1000)*int64(time.Millisecond)).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp format %q", s)
}

func isNullOrEmpty(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// ParseFlexString converts a JSON string, number, or boolean cleanly into a Go string.
func ParseFlexString(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return ""
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			return strings.TrimSpace(s)
		}
		return ""
	}
	var num json.Number
	dec := json.NewDecoder(bytes.NewReader(t))
	dec.UseNumber()
	if err := dec.Decode(&num); err == nil {
		s := strings.TrimSpace(num.String())
		// Clean trailing ".0" from integer-valued floats
		if f, err2 := strconv.ParseFloat(s, 64); err2 == nil && math.Trunc(f) == f && !strings.ContainsAny(s, "eE") {
			return strconv.FormatInt(int64(f), 10)
		}
		return s
	}
	var b bool
	if err := json.Unmarshal(t, &b); err == nil {
		if b {
			return "true"
		}
		return "false"
	}
	return ""
}

// ParseFlexFloat converts a JSON number or numeric string cleanly into float64.
func ParseFlexFloat(raw json.RawMessage) float64 {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return 0
	}
	var f float64
	if err := json.Unmarshal(t, &f); err == nil {
		return f
	}
	var s string
	if err := json.Unmarshal(t, &s); err == nil {
		s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
		if s == "" {
			return 0
		}
		if parsed, err2 := strconv.ParseFloat(s, 64); err2 == nil {
			return parsed
		}
	}
	return 0
}

// ParseFlexInt converts a JSON number, float, or numeric string cleanly into int.
func ParseFlexInt(raw json.RawMessage) int {
	p := ParseFlexIntPtr(raw)
	if p == nil {
		return 0
	}
	return *p
}

// ParseFlexIntPtr converts a JSON number, float, or numeric string into *int (nil on null/empty).
func ParseFlexIntPtr(raw json.RawMessage) *int {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) || bytes.Equal(t, []byte(`""`)) {
		return nil
	}
	var i int
	if err := json.Unmarshal(t, &i); err == nil {
		return &i
	}
	var f float64
	if err := json.Unmarshal(t, &f); err == nil {
		v := int(math.Round(f))
		return &v
	}
	var s string
	if err := json.Unmarshal(t, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		if parsed, err2 := strconv.ParseFloat(s, 64); err2 == nil {
			v := int(math.Round(parsed))
			return &v
		}
	}
	return nil
}

// ParseFlexBoolPtr converts JSON bool, SQLite 0/1 integer, or string ("true"/"false"/"1"/"0"/"yes"/"no") into *bool.
func ParseFlexBoolPtr(raw json.RawMessage) *bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) || bytes.Equal(t, []byte(`""`)) {
		return nil
	}
	var b bool
	if err := json.Unmarshal(t, &b); err == nil {
		return &b
	}
	var f float64
	if err := json.Unmarshal(t, &f); err == nil {
		v := f != 0
		return &v
	}
	var s string
	if err := json.Unmarshal(t, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "on", "active", "enabled":
			return BoolPtr(true)
		case "false", "0", "no", "off", "inactive", "disabled":
			return BoolPtr(false)
		}
	}
	return nil
}

// ParseFlexBool converts JSON bool/int/string into bool with a fallback default.
func ParseFlexBool(raw json.RawMessage, defaultVal bool) bool {
	if p := ParseFlexBoolPtr(raw); p != nil {
		return *p
	}
	return defaultVal
}

// ParseFlexStringSlice converts a JSON array of strings/numbers or a comma/newline string into []string.
func ParseFlexStringSlice(raw json.RawMessage) []string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil
	}
	if t[0] == '[' {
		var raws []json.RawMessage
		if err := json.Unmarshal(t, &raws); err == nil {
			out := make([]string, 0, len(raws))
			for _, item := range raws {
				if s := ParseFlexString(item); s != "" {
					out = append(out, s)
				}
			}
			return out
		}
	}
	// Handle JSON-encoded array inside a string e.g. "[\"https://...\",\"https://...\"]"
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			s = strings.TrimSpace(s)
			if s == "" {
				return nil
			}
			if strings.HasPrefix(s, "[") {
				var nested []json.RawMessage
				if err2 := json.Unmarshal([]byte(s), &nested); err2 == nil {
					out := make([]string, 0, len(nested))
					for _, item := range nested {
						if val := ParseFlexString(item); val != "" {
							out = append(out, val)
						}
					}
					return out
				}
			}
			parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' })
			out := make([]string, 0, len(parts))
			for _, part := range parts {
				if p := strings.TrimSpace(part); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
	}
	return nil
}

func firstRaw(m map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && !isNullOrEmpty(v) {
			return v, true
		}
	}
	return nil, false
}

// DecodeVendorProfile resiliently decodes a VendorProfile JSON object from Central Node / SQLite / Postgres.
func DecodeVendorProfile(raw json.RawMessage) (*VendorProfile, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("vendor section is not a JSON object: %w", err)
	}
	v := &VendorProfile{
		AllowCOD: true,
	}
	if r, ok := firstRaw(m, "vendor_id", "id"); ok {
		v.VendorID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "username", "user_name", "handle"); ok {
		v.Username = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "business_name", "store_name", "shop_name", "name"); ok {
		v.BusinessName = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "business_type", "category", "store_type"); ok {
		v.BusinessType = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "language", "response_language", "bot_language"); ok {
		v.Language = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "store_policies", "policies", "policy"); ok {
		v.StorePolicies = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "store_slug", "slug"); ok {
		v.StoreSlug = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "store_url", "website_url", "shop_url"); ok {
		v.StoreURL = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "mall_url", "marketplace_url"); ok {
		v.MallURL = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "notification_phone", "owner_phone", "phone", "whatsapp_phone"); ok {
		v.NotificationPhone = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "latitude", "lat"); ok {
		v.Latitude = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "longitude", "lng", "lon"); ok {
		v.Longitude = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "max_delivery_radius_km", "delivery_radius_km"); ok {
		v.MaxDeliveryRadiusKM = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "free_delivery_radius_km"); ok {
		v.FreeDeliveryRadiusKM = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "delivery_charge_amount", "delivery_charge", "delivery_fee"); ok {
		v.DeliveryChargeAmount = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "require_customer_location"); ok {
		v.RequireCustomerLocation = ParseFlexBool(r, false)
	}
	if r, ok := firstRaw(m, "allow_cod", "cod_enabled"); ok {
		v.AllowCOD = ParseFlexBool(r, true)
	}
	if r, ok := firstRaw(m, "is_open", "store_open"); ok {
		v.IsOpen = ParseFlexBoolPtr(r)
	}
	if r, ok := firstRaw(m, "holiday_mode", "is_holiday"); ok {
		v.HolidayMode = ParseFlexBool(r, false)
	}
	if r, ok := firstRaw(m, "holiday_message"); ok {
		v.HolidayMessage = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "operating_hours", "timings", "store_hours"); ok {
		v.OperatingHours = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "next_open_time"); ok {
		v.NextOpenTime = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "min_order_amount", "minimum_order_amount", "min_order"); ok {
		v.MinOrderAmount = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "estimated_delivery_time", "delivery_eta", "eta"); ok {
		v.EstimatedDeliveryTime = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "subscription_active"); ok {
		v.SubscriptionActive = ParseFlexBoolPtr(r)
	}
	if r, ok := firstRaw(m, "subscription_valid_until", "subscription_expires_at"); ok {
		v.SubscriptionValidUntil = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "admin_ai_auto_reply_allowed", "admin_ai_allowed"); ok {
		v.AdminAIAutoReplyAllowed = ParseFlexBoolPtr(r)
	}
	if r, ok := firstRaw(m, "admin_pause_reason"); ok {
		v.AdminPauseReason = ParseFlexString(r)
	}
	return v, nil
}

// DecodeAIConfig resiliently decodes an AIConfig JSON object.
func DecodeAIConfig(raw json.RawMessage) (*AIConfig, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("ai section is not a JSON object: %w", err)
	}
	a := &AIConfig{
		Temperature:   0.7,
		AutoReply:     true,
		ImportCatalog: true,
	}
	if r, ok := firstRaw(m, "provider", "ai_provider"); ok {
		a.Provider = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "api_key", "ai_api_key", "primary_api_key"); ok {
		a.APIKey = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "api_keys"); ok {
		a.APIKeys = ParseFlexStringSlice(r)
	}
	if r, ok := firstRaw(m, "backup_api_keys", "fallback_api_keys"); ok {
		a.BackupAPIKeys = ParseFlexStringSlice(r)
	}
	if r, ok := firstRaw(m, "model", "ai_model"); ok {
		a.Model = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "temperature", "ai_temperature"); ok {
		if temp := ParseFlexFloat(r); temp > 0 {
			a.Temperature = temp
		}
	}
	if r, ok := firstRaw(m, "system_prompt", "prompt", "instructions"); ok {
		a.SystemPrompt = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "auto_reply", "ai_auto_reply", "enabled"); ok {
		a.AutoReply = ParseFlexBool(r, true)
	}
	if r, ok := firstRaw(m, "import_catalog", "import_catalog_details"); ok {
		a.ImportCatalog = ParseFlexBool(r, true)
	}
	return a, nil
}

// DecodePaymentConfig resiliently decodes a PaymentConfig JSON object with WaMart Banner QR & verify defaults.
func DecodePaymentConfig(raw json.RawMessage) (*PaymentConfig, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("payment section is not a JSON object: %w", err)
	}
	def := DefaultPaymentConfig()
	p := &def
	if r, ok := firstRaw(m, "enabled", "online_payment_enabled", "is_enabled", "active"); ok {
		p.Enabled = ParseFlexBool(r, false)
	}
	if r, ok := firstRaw(m, "merchant_phone", "phone"); ok {
		p.MerchantPhone = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "upi_id", "merchant_vpa", "paytm_vpa", "vpa"); ok {
		vpa := ParseFlexString(r)
		p.UPIID = vpa
		p.MerchantVPA = vpa
	}
	if r, ok := firstRaw(m, "merchant_vpa"); ok {
		if v := ParseFlexString(r); v != "" {
			p.MerchantVPA = v
			if p.UPIID == "" {
				p.UPIID = v
			}
		}
	}
	if r, ok := firstRaw(m, "paytm_mid", "mid", "merchant_id"); ok {
		p.PaytmMID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "payee_name", "merchant_name", "upi_name"); ok {
		p.PayeeName = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "qr_worker_base_url"); ok {
		if v := ParseFlexString(r); v != "" {
			p.QRWorkerBaseURL = v
		}
	}
	if r, ok := firstRaw(m, "qr_generator_url"); ok {
		if v := ParseFlexString(r); v != "" {
			p.QRGeneratorURL = v
		}
	} else if rBase, ok := firstRaw(m, "qr_worker_base_url"); ok {
		if base := ParseFlexString(rBase); base != "" {
			p.QRGeneratorURL = strings.TrimRight(base, "/") + "/generate"
		}
	}
	if r, ok := firstRaw(m, "qr_cloud_name", "cloudinary_cloud_name"); ok {
		if v := ParseFlexString(r); v != "" {
			p.QRCloudName = v
			p.CloudinaryCloudName = v
		}
	}
	if r, ok := firstRaw(m, "qr_only_mode"); ok {
		p.QROnlyMode = ParseFlexBool(r, true)
	}
	if r, ok := firstRaw(m, "send_payment_link"); ok {
		p.SendPaymentLink = ParseFlexBool(r, false)
	}
	if r, ok := firstRaw(m, "verify_endpoint"); ok {
		if v := ParseFlexString(r); v != "" {
			p.VerifyEndpoint = v
		}
	}
	if r, ok := firstRaw(m, "verify_poll_interval_ms"); ok {
		if v := ParseFlexInt(r); v > 0 {
			p.VerifyPollIntervalMS = v
		}
	}
	if r, ok := firstRaw(m, "verify_poll_timeout_ms"); ok {
		if v := ParseFlexInt(r); v > 0 {
			p.VerifyPollTimeoutMS = v
		}
	}
	if r, ok := firstRaw(m, "gateway", "provider", "payment_gateway"); ok {
		p.Gateway = ParseFlexString(r)
	}
	p.ApplyDefaults("")
	return p, nil
}

// DecodeProductVariant resiliently decodes a single variant object or string.
func DecodeProductVariant(raw json.RawMessage) (ProductVariant, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return ProductVariant{}, false
	}
	if t[0] == '"' {
		name := ParseFlexString(t)
		if name == "" {
			return ProductVariant{}, false
		}
		return ProductVariant{Name: name, InStock: BoolPtr(true)}, true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(t, &m); err != nil {
		return ProductVariant{}, false
	}
	pv := ProductVariant{}
	if r, ok := firstRaw(m, "name", "label", "title", "variant"); ok {
		pv.Name = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "weight"); ok {
		pv.Weight = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "size"); ok {
		pv.Size = ParseFlexString(r)
	}
	if pv.Name == "" {
		pv.Name = pv.DisplayLabel()
	}
	if r, ok := firstRaw(m, "price"); ok {
		pv.Price = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "sale_price", "offer_price"); ok {
		pv.SalePrice = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "mrp"); ok {
		pv.MRP = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "stock_qty", "stock", "qty"); ok {
		pv.StockQty = ParseFlexIntPtr(r)
	}
	if r, ok := firstRaw(m, "in_stock", "available"); ok {
		pv.InStock = ParseFlexBoolPtr(r)
	}
	if r, ok := firstRaw(m, "sku"); ok {
		pv.SKU = ParseFlexString(r)
	}
	if pv.DisplayLabel() == "" {
		return ProductVariant{}, false
	}
	return pv, true
}

func decodeVariantsRaw(raw json.RawMessage) VariantList {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil
	}
	// Handle JSON-encoded array inside a string column
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			s = strings.TrimSpace(s)
			if strings.HasPrefix(s, "[") {
				t = []byte(s)
			} else if s != "" {
				parts := strings.Split(s, ",")
				var out VariantList
				for _, p := range parts {
					if name := strings.TrimSpace(p); name != "" {
						out = append(out, ProductVariant{Name: name, InStock: BoolPtr(true)})
					}
				}
				return out
			}
		}
	}
	if len(t) == 0 || t[0] != '[' {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(t, &items); err != nil {
		return nil
	}
	out := make(VariantList, 0, len(items))
	for _, it := range items {
		if pv, ok := DecodeProductVariant(it); ok {
			out = append(out, pv)
		}
	}
	return out
}

// DecodeProduct resiliently decodes a single Product (accepting string/number IDs, image URL aliases, SQLite 0/1 booleans).
func DecodeProduct(raw json.RawMessage) (Product, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return Product{}, fmt.Errorf("product item is not a JSON object: %w", err)
	}
	p := Product{
		InStock: true,
	}
	if r, ok := firstRaw(m, "id", "product_id"); ok {
		p.ID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "name", "product_name", "title"); ok {
		p.Name = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "price", "unit_price"); ok {
		p.Price = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "sale_price", "offer_price", "discounted_price"); ok {
		p.SalePrice = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "mrp", "compare_at_price"); ok {
		p.MRP = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "description", "desc", "short_description"); ok {
		p.Description = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "category", "category_name"); ok {
		p.Category = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "unit_type", "unit"); ok {
		p.UnitType = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "variants"); ok {
		p.Variants = decodeVariantsRaw(r)
	}
	if r, ok := firstRaw(m, "stock_qty", "stock", "quantity"); ok {
		p.StockQty = ParseFlexIntPtr(r)
	}
	if r, ok := firstRaw(m, "in_stock", "available", "is_available"); ok {
		p.InStock = ParseFlexBool(r, true)
	} else if p.StockQty != nil && *p.StockQty <= 0 {
		p.InStock = false
	}
	if r, ok := firstRaw(m, "image_url", "image", "photo_url", "img_url", "product_image", "primary_image"); ok {
		p.ImageURL = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "images", "image_urls", "gallery"); ok {
		p.Images = ParseFlexStringSlice(r)
	}
	if r, ok := firstRaw(m, "thumb_url", "thumbnail", "thumbnail_url"); ok {
		p.ThumbURL = ParseFlexString(r)
	}
	if p.ImageURL == "" && len(p.Images) > 0 {
		p.ImageURL = p.Images[0]
	}
	if p.ImageURL == "" && p.ThumbURL != "" {
		p.ImageURL = p.ThumbURL
	}
	if r, ok := firstRaw(m, "whatsapp_catalog_link", "catalog_link"); ok {
		p.WhatsAppCatalogLink = ParseFlexString(r)
	}
	return p, nil
}

// DecodeProducts decodes a JSON array of products item-by-item without letting a bad item abort the rest.
func DecodeProducts(raw json.RawMessage) ([]Product, []string) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, []string{fmt.Sprintf("products section invalid array: %v", err)}
	}
	out := make([]Product, 0, len(items))
	var warnings []string
	for i, it := range items {
		p, err := DecodeProduct(it)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("products[%d] skipped: %v", i, err))
			continue
		}
		out = append(out, p)
	}
	return out, warnings
}

// DecodeCustomer resiliently decodes a Customer JSON object (accepting flexible timestamps & phone/name aliases).
func DecodeCustomer(raw json.RawMessage) (*Customer, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("customer item is not a JSON object: %w", err)
	}
	c := &Customer{}
	if r, ok := firstRaw(m, "wa_number", "customer_phone", "phone", "mobile", "whatsapp_number"); ok {
		c.WANumber = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "jid"); ok {
		c.JID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "full_name", "customer_name", "name"); ok {
		c.FullName = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "delivery_address", "address"); ok {
		c.DeliveryAddress = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "contact_phone", "delivery_phone", "alternate_phone"); ok {
		c.ContactPhone = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "latitude", "lat"); ok {
		c.Latitude = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "longitude", "lng", "lon"); ok {
		c.Longitude = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "last_order_details"); ok {
		c.LastOrderDetails = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "current_state", "state"); ok {
		c.CurrentState = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "payment_status"); ok {
		c.PaymentStatus = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "current_order_id"); ok {
		c.CurrentOrderID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "source"); ok {
		c.Source = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "created_at"); ok {
		if t, err := ParseFlexibleTime(r); err == nil {
			c.CreatedAt = t
		}
	}
	if r, ok := firstRaw(m, "updated_at"); ok {
		if t, err := ParseFlexibleTime(r); err == nil {
			c.UpdatedAt = t
		}
	}
	return c, nil
}

// DecodeCustomers decodes an array of customers item-by-item.
func DecodeCustomers(raw json.RawMessage) ([]Customer, []string) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, []string{fmt.Sprintf("customers section invalid array: %v", err)}
	}
	out := make([]Customer, 0, len(items))
	var warnings []string
	for i, it := range items {
		c, err := DecodeCustomer(it)
		if err != nil || c == nil {
			warnings = append(warnings, fmt.Sprintf("customers[%d] skipped: %v", i, err))
			continue
		}
		out = append(out, *c)
	}
	return out, warnings
}

// DecodeCartItem resiliently decodes a CartItem / Order Item accepting product_id or id as string OR number.
func DecodeCartItem(raw json.RawMessage) (CartItem, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return CartItem{}, fmt.Errorf("cart item is not a JSON object: %w", err)
	}
	it := CartItem{Qty: 1}
	if r, ok := firstRaw(m, "product_id", "id"); ok {
		it.ProductID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "product_name", "name", "title"); ok {
		it.ProductName = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "unit_price", "price", "sale_price"); ok {
		it.UnitPrice = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "unit_type", "unit"); ok {
		it.UnitType = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "qty", "quantity"); ok {
		it.Qty = ParseFlexInt(r)
	}
	if r, ok := firstRaw(m, "selected_variant", "variant", "selectedVariant"); ok {
		it.SelectedVariant = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "line_total", "total", "subtotal"); ok {
		it.LineTotal = ParseFlexFloat(r)
	}
	if it.LineTotal <= 0 && it.UnitPrice > 0 && it.Qty > 0 {
		it.LineTotal = it.UnitPrice * float64(it.Qty)
	}
	if r, ok := firstRaw(m, "source"); ok {
		it.Source = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "updated_at", "created_at"); ok {
		if t, err := ParseFlexibleTime(r); err == nil {
			it.UpdatedAt = t
		}
	}
	return it, nil
}

// DecodeCartItems decodes an array (or JSON-encoded string array) of CartItems resiliently.
func DecodeCartItems(raw json.RawMessage) ([]CartItem, []string) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil, nil
	}
	// Handle SQLite JSON-stringified items array e.g. "[{\"product_id\":1,...}]"
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			s = strings.TrimSpace(s)
			if s == "" || s == "null" {
				return nil, nil
			}
			t = []byte(s)
		}
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(t, &rawItems); err != nil {
		return nil, []string{fmt.Sprintf("items is not a valid array: %v", err)}
	}
	out := make([]CartItem, 0, len(rawItems))
	var warnings []string
	for i, ri := range rawItems {
		item, err := DecodeCartItem(ri)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("items[%d] skipped: %v", i, err))
			continue
		}
		out = append(out, item)
	}
	return out, warnings
}

// DecodeActiveCarts supports BOTH map[phone][]CartItem AND array [{"customer_phone":"...", "items":[...]}].
func DecodeActiveCarts(raw json.RawMessage) (map[string][]CartItem, []string) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil, nil
	}
	result := make(map[string][]CartItem)
	var warnings []string

	if t[0] == '{' {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(t, &m); err != nil {
			return nil, []string{fmt.Sprintf("active_carts invalid object: %v", err)}
		}
		for phone, itemsRaw := range m {
			items, w := DecodeCartItems(itemsRaw)
			if len(w) > 0 {
				for _, msg := range w {
					warnings = append(warnings, fmt.Sprintf("active_carts[%s]: %s", phone, msg))
				}
			}
			result[phone] = items
		}
		return result, warnings
	}

	if t[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(t, &arr); err != nil {
			return nil, []string{fmt.Sprintf("active_carts invalid array: %v", err)}
		}
		for i, entryRaw := range arr {
			var em map[string]json.RawMessage
			if err := json.Unmarshal(entryRaw, &em); err != nil {
				warnings = append(warnings, fmt.Sprintf("active_carts[%d] skipped: %v", i, err))
				continue
			}
			phoneRaw, ok := firstRaw(em, "customer_phone", "wa_number", "phone")
			if !ok {
				continue
			}
			phone := ParseFlexString(phoneRaw)
			itemsRaw, _ := firstRaw(em, "items", "cart_items")
			items, w := DecodeCartItems(itemsRaw)
			if len(w) > 0 {
				for _, msg := range w {
					warnings = append(warnings, fmt.Sprintf("active_carts[%s]: %s", phone, msg))
				}
			}
			result[phone] = items
		}
		return result, warnings
	}

	return nil, []string{"active_carts section must be an object or array"}
}

// DecodeOrder resiliently decodes an Order object with flexible timestamps and numeric/string IDs.
func DecodeOrder(raw json.RawMessage) (*Order, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("order item is not a JSON object: %w", err)
	}
	o := &Order{}
	if r, ok := firstRaw(m, "order_id", "id"); ok {
		o.OrderID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "customer_phone", "wa_number", "phone"); ok {
		o.CustomerPhone = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "jid"); ok {
		o.JID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "customer_name", "full_name", "name"); ok {
		o.CustomerName = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "delivery_phone", "contact_phone"); ok {
		o.DeliveryPhone = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "delivery_address", "address"); ok {
		o.DeliveryAddress = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "items", "order_items"); ok {
		items, _ := DecodeCartItems(r)
		o.Items = items
	}
	if r, ok := firstRaw(m, "subtotal", "sub_total"); ok {
		o.Subtotal = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "delivery_fee", "delivery_charge"); ok {
		o.DeliveryFee = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "total_amount", "total", "amount", "grand_total"); ok {
		o.TotalAmount = ParseFlexFloat(r)
	}
	if r, ok := firstRaw(m, "payment_mode", "payment_method"); ok {
		o.PaymentMode = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "payment_status"); ok {
		o.PaymentStatus = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "order_status", "status"); ok {
		o.OrderStatus = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "source"); ok {
		o.Source = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "txn_id", "transaction_id", "payment_id"); ok {
		o.TxnID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "payment_link"); ok {
		o.PaymentLink = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "payment_qr_url"); ok {
		o.PaymentQRURL = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "delivery_partner_name", "rider_name"); ok {
		o.DeliveryPartnerName = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "delivery_partner_phone", "rider_phone"); ok {
		o.DeliveryPartnerPhone = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "delivery_otp", "otp"); ok {
		o.DeliveryOTP = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "estimated_delivery", "eta"); ok {
		o.EstimatedDelivery = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "status_updated_by"); ok {
		o.StatusUpdatedBy = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "created_at"); ok {
		if t, err := ParseFlexibleTime(r); err == nil {
			o.CreatedAt = t
		}
	}
	if r, ok := firstRaw(m, "updated_at"); ok {
		if t, err := ParseFlexibleTime(r); err == nil {
			o.UpdatedAt = t
		}
	}
	return o, nil
}

// DecodeOrders decodes an array of orders item-by-item without failing other orders or sections.
func DecodeOrders(raw json.RawMessage) ([]Order, []string) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, []string{fmt.Sprintf("orders section invalid array: %v", err)}
	}
	out := make([]Order, 0, len(items))
	var warnings []string
	for i, it := range items {
		ord, err := DecodeOrder(it)
		if err != nil || ord == nil {
			warnings = append(warnings, fmt.Sprintf("orders[%d] skipped: %v", i, err))
			continue
		}
		out = append(out, *ord)
	}
	return out, warnings
}

// DecodeWhatsAppSession resiliently decodes the persisted whatsapp_session object from Central Node.
func DecodeWhatsAppSession(raw json.RawMessage) (*WhatsAppSessionState, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("whatsapp_session section is not a JSON object: %w", err)
	}
	ws := &WhatsAppSessionState{}
	if r, ok := firstRaw(m, "has_session", "connected", "is_linked"); ok {
		ws.HasSession = ParseFlexBool(r, false)
	}
	if r, ok := firstRaw(m, "device_jid", "jid", "wa_jid"); ok {
		ws.DeviceJID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "phone", "wa_number", "number"); ok {
		ws.Phone = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "push_name", "name"); ok {
		ws.PushName = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "status", "state"); ok {
		ws.Status = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "session_blob_base64", "session_blob", "session_data", "whatsapp_session_blob", "blob", "data_base64"); ok {
		ws.SessionBlobBase64 = strings.TrimSpace(ParseFlexString(r))
	}
	if ws.SessionBlobBase64 != "" {
		ws.HasSession = true
	}
	ws.NeedsLogin = !ws.HasSession
	if r, ok := firstRaw(m, "last_synced_at", "updated_at"); ok {
		if t, err := ParseFlexibleTime(r); err == nil {
			ws.LastSyncedAt = t
		}
	}
	return ws, nil
}

// DecodeBootstrapPayload independently decodes each top-level section (vendor/business, ai, payment,
// products, customers, active_carts, orders, whatsapp_session) from either top-level JSON or wrapped
// {"data": {...}} JSON. A malformed item or section NEVER blocks hydrating the other sections.
func DecodeBootstrapPayload(body []byte) (*BootstrapData, []string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("empty JSON body")
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return nil, nil, fmt.Errorf("malformed JSON syntax: %w", err)
	}

	// Unwrap {"status":"success","data":{...}} if present while preserving any top-level sibling keys
	sectionMap := top
	if dataRaw, ok := top["data"]; ok && !isNullOrEmpty(dataRaw) {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(dataRaw, &inner); err == nil && len(inner) > 0 {
			merged := make(map[string]json.RawMessage, len(top)+len(inner))
			for k, v := range top {
				if k != "data" {
					merged[k] = v
				}
			}
			for k, v := range inner {
				merged[k] = v
			}
			sectionMap = merged
		}
	}

	result := &BootstrapData{}
	var warnings []string

	// 1. Vendor / Business
	if raw, ok := firstRaw(sectionMap, "vendor", "business", "store"); ok {
		if v, err := DecodeVendorProfile(raw); err != nil {
			warnings = append(warnings, err.Error())
		} else {
			result.Vendor = v
		}
	}

	// 2. AI Config
	if raw, ok := firstRaw(sectionMap, "ai", "ai_config", "ai_settings"); ok {
		if a, err := DecodeAIConfig(raw); err != nil {
			warnings = append(warnings, err.Error())
		} else {
			result.AI = a
		}
	}

	// 3. Payment Config
	if raw, ok := firstRaw(sectionMap, "payment", "payment_config", "payment_settings"); ok {
		if p, err := DecodePaymentConfig(raw); err != nil {
			warnings = append(warnings, err.Error())
		} else {
			result.Payment = p
		}
	}

	// 4. Products (item-by-item)
	if raw, ok := firstRaw(sectionMap, "products", "catalog"); ok {
		prods, w := DecodeProducts(raw)
		result.Products = prods
		warnings = append(warnings, w...)
	}

	// 5. Customers (item-by-item)
	if raw, ok := firstRaw(sectionMap, "customers"); ok {
		custs, w := DecodeCustomers(raw)
		result.Customers = custs
		warnings = append(warnings, w...)
	}

	// 6. Active Carts (entry-by-entry)
	if raw, ok := firstRaw(sectionMap, "active_carts", "carts"); ok {
		carts, w := DecodeActiveCarts(raw)
		result.ActiveCarts = carts
		warnings = append(warnings, w...)
	}

	// 7. Orders (item-by-item)
	if raw, ok := firstRaw(sectionMap, "orders"); ok {
		ords, w := DecodeOrders(raw)
		result.Orders = ords
		warnings = append(warnings, w...)
	}

	// 8. WhatsApp Session
	if raw, ok := firstRaw(sectionMap, "whatsapp_session", "whatsapp"); ok {
		if ws, err := DecodeWhatsAppSession(raw); err != nil {
			warnings = append(warnings, err.Error())
		} else {
			result.WhatsAppSession = ws
		}
	}

	return result, warnings, nil
}
