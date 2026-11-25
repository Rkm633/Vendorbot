package ramstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// =============================================================================
// DUAL-MODE BOT CONSTANTS & VENDOR MANAGEMENT RAM STATE (Admin Mode)
// =============================================================================

const (
	BotModeVendorStore        = "vendor_store"
	BotModeAdminVendorManager = "admin_vendor_manager"
)

// NormalizeBotMode normalizes raw bot_mode strings into one of the two supported modes.
// Returns "" when input is empty or unrecognized so callers can apply fallback precedence cleanly.
func NormalizeBotMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case BotModeAdminVendorManager, "admin_manager", "vendor_manager", "admin":
		return BotModeAdminVendorManager
	case BotModeVendorStore, "vendor", "store":
		return BotModeVendorStore
	default:
		return ""
	}
}

// ResolveEnvBotMode resolves the optional BOT_MODE env var, defaulting to "vendor_store".
func ResolveEnvBotMode() string {
	if m := NormalizeBotMode(os.Getenv("BOT_MODE")); m != "" {
		return m
	}
	return BotModeVendorStore
}

// VendorManagementSettings holds Central Admin automation toggles and templates in RAM.
type VendorManagementSettings struct {
	AutoRenewalReminders      bool   `json:"auto_renewal_reminders"`
	RenewalReminderDays       []int  `json:"renewal_reminder_days"`
	AutoOnboardingWelcome     bool   `json:"auto_onboarding_welcome"`
	WhatsAppPlanRenewalQR     bool   `json:"whatsapp_plan_renewal_qr"`
	VendorSelfServiceCommands bool   `json:"vendor_self_service_commands"`
	MallApprovalAlerts        bool   `json:"mall_approval_alerts"`
	SupportEscalationPhone    string `json:"support_escalation_phone"`
	WelcomeTemplate           string `json:"welcome_template"`
	RenewalTemplate           string `json:"renewal_template"`
}

// OwnerPaymentGateway holds the platform owner's UPI / Paytm credentials for vendor plan renewals.
type OwnerPaymentGateway struct {
	UPIVPA    string `json:"upi_vpa"`
	PaytmMID  string `json:"paytm_mid"`
	PayeeName string `json:"payee_name"`
}

// SubscriptionPlan represents an active WaMart vendor subscription plan cached in RAM.
type SubscriptionPlan struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	MonthlyPrice float64 `json:"monthly_price"`
	YearlyPrice  float64 `json:"yearly_price"`
	ValidityDays int     `json:"validity_days"`
	Description  string  `json:"description,omitempty"`
}

// ManagedVendor represents a WaMart vendor record cached in RAM for O(1) phone lookup in Admin Mode.
type ManagedVendor struct {
	Username       string `json:"username"`
	StoreName      string `json:"store_name"`
	OwnerPhone     string `json:"owner_phone"`
	PlanID         string `json:"plan_id"`
	PlanName       string `json:"plan_name"`
	PlanValidUntil string `json:"plan_valid_until"`
	DaysRemaining  int    `json:"days_remaining"`
	IsPlanExpired  bool   `json:"is_plan_expired"`
	UPIID          string `json:"upi_id"`
	BotConnected   bool   `json:"bot_connected"`
	BotPublicURL   string `json:"bot_public_url"`
	StoreURL       string `json:"store_url"`
	MallApproved   *bool  `json:"mall_approved,omitempty"`
}

// VendorManagementState stores all hot-reloaded Admin Vendor Manager configuration & vendor directory in RAM.
type VendorManagementState struct {
	Settings            VendorManagementSettings `json:"settings"`
	OwnerPaymentGateway OwnerPaymentGateway      `json:"owner_payment_gateway"`
	SubscriptionPlans   []SubscriptionPlan       `json:"subscription_plans"`
	ManagedVendors      []ManagedVendor          `json:"managed_vendors"`
	UpdatedAt           time.Time                `json:"updated_at,omitempty"`
}

// DefaultVendorManagementState initializes safe defaults for Admin Vendor Manager mode.
func DefaultVendorManagementState() VendorManagementState {
	return VendorManagementState{
		Settings: VendorManagementSettings{
			AutoRenewalReminders:      false,
			RenewalReminderDays:       []int{7, 3, 1},
			AutoOnboardingWelcome:     false,
			WhatsAppPlanRenewalQR:     true,
			VendorSelfServiceCommands: true,
			MallApprovalAlerts:        true,
		},
		SubscriptionPlans: []SubscriptionPlan{},
		ManagedVendors:    []ManagedVendor{},
	}
}

// PhoneLookupKeys returns all normalized lookup keys (12-digit Indian / normalized digits + 10-digit suffix)
// so O(1) map lookup succeeds regardless of whether a phone is formatted as 10 or 12 digits.
func PhoneLookupKeys(rawPhone string) []string {
	digits := nonDigitsRe.ReplaceAllString(strings.TrimSpace(rawPhone), "")
	if digits == "" {
		return nil
	}
	seen := make(map[string]bool, 3)
	var keys []string
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	norm := NormalizePhone(digits)
	add(norm)
	add(digits)
	if len(digits) > 10 {
		add(digits[len(digits)-10:])
	}
	if len(norm) > 10 {
		add(norm[len(norm)-10:])
	}
	return keys
}

// ParseFlexIntSlice decodes JSON arrays of numbers/strings (e.g. [7, 3, 1] or "7,3,1") into []int.
func ParseFlexIntSlice(raw json.RawMessage) []int {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			s = strings.TrimSpace(s)
			if s == "" {
				return nil
			}
			if strings.HasPrefix(s, "[") {
				t = []byte(s)
			} else {
				parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' })
				var out []int
				for _, p := range parts {
					if ptr := ParseFlexIntPtr(json.RawMessage(`"` + strings.TrimSpace(p) + `"`)); ptr != nil {
						out = append(out, *ptr)
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
	out := make([]int, 0, len(items))
	for _, it := range items {
		if ptr := ParseFlexIntPtr(it); ptr != nil {
			out = append(out, *ptr)
		}
	}
	return out
}

// DecodeVendorManagementSettings resiliently decodes vendor_management.settings.
func DecodeVendorManagementSettings(raw json.RawMessage) VendorManagementSettings {
	cfg := DefaultVendorManagementState().Settings
	if isNullOrEmpty(raw) {
		return cfg
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return cfg
	}
	if r, ok := firstRaw(m, "auto_renewal_reminders"); ok {
		cfg.AutoRenewalReminders = ParseFlexBool(r, false)
	}
	if r, ok := firstRaw(m, "renewal_reminder_days"); ok {
		if days := ParseFlexIntSlice(r); days != nil {
			cfg.RenewalReminderDays = days
		}
	}
	if r, ok := firstRaw(m, "auto_onboarding_welcome"); ok {
		cfg.AutoOnboardingWelcome = ParseFlexBool(r, false)
	}
	if r, ok := firstRaw(m, "whatsapp_plan_renewal_qr"); ok {
		cfg.WhatsAppPlanRenewalQR = ParseFlexBool(r, true)
	}
	if r, ok := firstRaw(m, "vendor_self_service_commands"); ok {
		cfg.VendorSelfServiceCommands = ParseFlexBool(r, true)
	}
	if r, ok := firstRaw(m, "mall_approval_alerts"); ok {
		cfg.MallApprovalAlerts = ParseFlexBool(r, true)
	}
	if r, ok := firstRaw(m, "support_escalation_phone", "escalation_phone", "admin_support_phone"); ok {
		cfg.SupportEscalationPhone = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "welcome_template", "onboarding_welcome_template"); ok {
		cfg.WelcomeTemplate = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "renewal_template", "renewal_reminder_template"); ok {
		cfg.RenewalTemplate = ParseFlexString(r)
	}
	return cfg
}

// DecodeOwnerPaymentGateway resiliently decodes vendor_management.owner_payment_gateway.
func DecodeOwnerPaymentGateway(raw json.RawMessage) OwnerPaymentGateway {
	var gw OwnerPaymentGateway
	if isNullOrEmpty(raw) {
		return gw
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return gw
	}
	if r, ok := firstRaw(m, "upi_vpa", "upi_id", "merchant_vpa", "vpa"); ok {
		gw.UPIVPA = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "paytm_mid", "mid", "merchant_id"); ok {
		gw.PaytmMID = ParseFlexString(r)
	}
	if r, ok := firstRaw(m, "payee_name", "merchant_name", "upi_name", "name"); ok {
		gw.PayeeName = ParseFlexString(r)
	}
	return gw
}

// DecodeSubscriptionPlans resiliently decodes vendor_management.subscription_plans.
func DecodeSubscriptionPlans(raw json.RawMessage) ([]SubscriptionPlan, []string) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, []string{fmt.Sprintf("subscription_plans invalid array: %v", err)}
	}
	out := make([]SubscriptionPlan, 0, len(items))
	var warnings []string
	for i, it := range items {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(it, &m); err != nil {
			warnings = append(warnings, fmt.Sprintf("subscription_plans[%d] skipped: %v", i, err))
			continue
		}
		plan := SubscriptionPlan{}
		if r, ok := firstRaw(m, "id", "plan_id"); ok {
			plan.ID = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "name", "plan_name", "title"); ok {
			plan.Name = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "monthly_price", "price_monthly", "price"); ok {
			plan.MonthlyPrice = ParseFlexFloat(r)
		}
		if r, ok := firstRaw(m, "yearly_price", "annual_price", "price_yearly"); ok {
			plan.YearlyPrice = ParseFlexFloat(r)
		}
		if r, ok := firstRaw(m, "validity_days", "duration_days", "days"); ok {
			plan.ValidityDays = ParseFlexInt(r)
		}
		if r, ok := firstRaw(m, "description", "summary"); ok {
			plan.Description = ParseFlexString(r)
		}
		if plan.ValidityDays <= 0 {
			plan.ValidityDays = 30
		}
		if plan.Name == "" && plan.ID == "" {
			continue
		}
		if plan.ID == "" {
			plan.ID = strings.ToLower(strings.ReplaceAll(plan.Name, " ", "_"))
		}
		out = append(out, plan)
	}
	return out, warnings
}

// DecodeManagedVendors resiliently decodes vendor_management.managed_vendors.
func DecodeManagedVendors(raw json.RawMessage) ([]ManagedVendor, []string) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, []string{fmt.Sprintf("managed_vendors invalid array: %v", err)}
	}
	out := make([]ManagedVendor, 0, len(items))
	var warnings []string
	for i, it := range items {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(it, &m); err != nil {
			warnings = append(warnings, fmt.Sprintf("managed_vendors[%d] skipped: %v", i, err))
			continue
		}
		mv := ManagedVendor{}
		if r, ok := firstRaw(m, "username", "user_name", "handle"); ok {
			mv.Username = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "store_name", "business_name", "shop_name", "name"); ok {
			mv.StoreName = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "owner_phone", "phone", "notification_phone", "whatsapp_phone", "mobile"); ok {
			mv.OwnerPhone = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "plan_id"); ok {
			mv.PlanID = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "plan_name", "plan"); ok {
			mv.PlanName = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "plan_valid_until", "subscription_valid_until", "valid_until"); ok {
			mv.PlanValidUntil = ParseFlexString(r)
		}
		hasDaysRem := false
		if r, ok := firstRaw(m, "days_remaining"); ok {
			if ptr := ParseFlexIntPtr(r); ptr != nil {
				mv.DaysRemaining = *ptr
				hasDaysRem = true
			}
		}
		hasExpiredFlag := false
		if r, ok := firstRaw(m, "is_plan_expired", "plan_expired", "expired"); ok {
			if ptr := ParseFlexBoolPtr(r); ptr != nil {
				mv.IsPlanExpired = *ptr
				hasExpiredFlag = true
			}
		}
		// Compute days_remaining / is_plan_expired from plan_valid_until if Central omitted them
		if (!hasDaysRem || !hasExpiredFlag) && strings.TrimSpace(mv.PlanValidUntil) != "" {
			if parsed, err := ParseFlexibleTimeString(mv.PlanValidUntil); err == nil && !parsed.IsZero() {
				if len(strings.TrimSpace(mv.PlanValidUntil)) == 10 {
					parsed = parsed.Add(24*time.Hour - time.Second)
				}
				diffDays := int(math.Ceil(time.Until(parsed).Hours() / 24.0))
				if !hasDaysRem {
					if diffDays < 0 {
						mv.DaysRemaining = 0
					} else {
						mv.DaysRemaining = diffDays
					}
				}
				if !hasExpiredFlag {
					mv.IsPlanExpired = time.Now().After(parsed)
				}
			}
		}
		if r, ok := firstRaw(m, "upi_id", "merchant_vpa", "vpa"); ok {
			mv.UPIID = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "bot_connected", "whatsapp_connected"); ok {
			mv.BotConnected = ParseFlexBool(r, false)
		}
		if r, ok := firstRaw(m, "bot_public_url", "bot_url"); ok {
			mv.BotPublicURL = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "store_url", "website_url", "shop_url"); ok {
			mv.StoreURL = ParseFlexString(r)
		}
		if r, ok := firstRaw(m, "mall_approved", "is_mall_approved", "mall_listed"); ok {
			mv.MallApproved = ParseFlexBoolPtr(r)
		}
		if mv.Username == "" && mv.StoreName == "" && mv.OwnerPhone == "" {
			continue
		}
		out = append(out, mv)
	}
	return out, warnings
}

// DecodeVendorManagement resiliently decodes the vendor_management block from bootstrap or /api/sync/vendor.
func DecodeVendorManagement(raw json.RawMessage) (*VendorManagementState, []string, error) {
	if isNullOrEmpty(raw) {
		return nil, nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("vendor_management section is not a JSON object: %w", err)
	}

	vm := DefaultVendorManagementState()
	var warnings []string

	if r, ok := firstRaw(m, "settings"); ok {
		vm.Settings = DecodeVendorManagementSettings(r)
	}
	if r, ok := firstRaw(m, "owner_payment_gateway", "payment_gateway"); ok {
		vm.OwnerPaymentGateway = DecodeOwnerPaymentGateway(r)
	}
	if r, ok := firstRaw(m, "subscription_plans", "plans"); ok {
		plans, w := DecodeSubscriptionPlans(r)
		if plans != nil {
			vm.SubscriptionPlans = plans
		}
		warnings = append(warnings, w...)
	}
	if r, ok := firstRaw(m, "managed_vendors", "vendors"); ok {
		vendors, w := DecodeManagedVendors(r)
		if vendors != nil {
			vm.ManagedVendors = vendors
		}
		warnings = append(warnings, w...)
	}
	vm.UpdatedAt = time.Now()
	return &vm, warnings, nil
}
