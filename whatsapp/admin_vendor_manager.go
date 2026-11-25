package whatsapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"wa-server/central"
	"wa-server/ramstore"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// =============================================================================
// ADMIN VENDOR MANAGER MODE (bot_mode == "admin_vendor_manager")
// - Disables customer shopping / cart / catalog checkout completely
// - O(1) lookup of incoming sender phone in ramstore.Global managed_vendors
// - Instant vendor self-service commands (my plan, store status, mall status,
//   help/menu, renew/upgrade with dynamic Universal Banner UPI QR + 3s Paytm poll)
// - Natural-language WaMart Vendor Support Manager LLM (last 10 msgs in RAM only)
// - Lightweight daily in-memory deduplicated auto renewal reminders
// =============================================================================

type pendingPlanPicker struct {
	Plans     []ramstore.SubscriptionPlan
	ExpiresAt time.Time
}

var (
	pendingPlanPickMu  sync.Mutex
	pendingPlanPickers = make(map[string]pendingPlanPicker)

	renewalReminderOnce   sync.Once
	renewalReminderSentMu sync.Mutex
	renewalReminderSent   = make(map[string]string) // normalizedPhone -> "YYYY-MM-DD" (IST)

	onboardingWelcomeMu   sync.Mutex
	onboardingWelcomeSent = make(map[string]bool)
)

const defaultAdminWelcomeTemplate = "🎉 *Welcome to WaMart, {{store_name}}!*\n\nAapka store account activate ho gaya hai.\n• *Plan:* {{plan_name}}\n• *Valid Until:* {{valid_until}}\n\nQuick commands ke liye *help* ya *my plan* likh kar bhejein. 🚀"

const defaultAdminRenewalTemplate = "🔔 *WaMart Plan Renewal Reminder*\n\nHello *{{store_name}}*,\nAapka *{{plan_name}}* plan *{{valid_until}}* ko expire hone wala hai ({{days_remaining}} din bache hain).\n\nApna store aur WhatsApp bot bina kisi rukawat ke chalu rakhne ke liye bas *renew* likh kar bhejein aur turant UPI QR se renew karein! ✨"

func istLocation() *time.Location {
	return time.FixedZone("IST", 5*3600+30*60)
}

func adminBotUsername() string {
	if u := strings.TrimSpace(ramstore.Global.GetVendor().Username); u != "" {
		return u
	}
	return "admin_vendor_manager"
}

func renderVendorTemplate(tpl string, v ramstore.ManagedVendor) string {
	storeName := strings.TrimSpace(v.StoreName)
	if storeName == "" {
		storeName = strings.TrimSpace(v.Username)
	}
	if storeName == "" {
		storeName = "Partner"
	}
	planName := strings.TrimSpace(v.PlanName)
	if planName == "" {
		planName = "Active Plan"
	}
	validUntil := strings.TrimSpace(v.PlanValidUntil)
	if validUntil == "" {
		validUntil = "soon"
	}
	replacer := strings.NewReplacer(
		"{{store_name}}", storeName,
		"{{plan_name}}", planName,
		"{{valid_until}}", validUntil,
		"{{plan_valid_until}}", validUntil,
		"{{days_remaining}}", strconv.Itoa(v.DaysRemaining),
		"{{username}}", strings.TrimSpace(v.Username),
		"{{store_url}}", strings.TrimSpace(v.StoreURL),
	)
	return replacer.Replace(tpl)
}

func generatePlanRenewalOrderID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("WMREN%s%s", time.Now().UTC().Format("060102"), strings.ToUpper(hex.EncodeToString(b[:])))
}

func getPaidSubscriptionPlans(plans []ramstore.SubscriptionPlan) []ramstore.SubscriptionPlan {
	out := make([]ramstore.SubscriptionPlan, 0, len(plans))
	for _, p := range plans {
		if p.MonthlyPrice > 0 || p.YearlyPrice > 0 {
			out = append(out, p)
		}
	}
	return out
}

func selectBillingCycleAndPrice(plan ramstore.SubscriptionPlan, rawMsgLower string) (cycle string, amount float64, validityDays int) {
	wantsYearly := strings.Contains(rawMsgLower, "year") || strings.Contains(rawMsgLower, "annual") || strings.Contains(rawMsgLower, "12 month") || strings.Contains(rawMsgLower, "saal")
	if wantsYearly && plan.YearlyPrice > 0 {
		days := plan.ValidityDays
		if days < 365 {
			days = 365
		}
		return "yearly", plan.YearlyPrice, days
	}
	if plan.MonthlyPrice > 0 {
		days := plan.ValidityDays
		if days <= 0 {
			days = 30
		}
		return "monthly", plan.MonthlyPrice, days
	}
	days := plan.ValidityDays
	if days <= 0 {
		days = 365
	}
	return "yearly", plan.YearlyPrice, days
}

func setPendingPlanPicker(phone string, plans []ramstore.SubscriptionPlan) {
	clean := FormatPhone(phone)
	if clean == "" || len(plans) == 0 {
		return
	}
	pendingPlanPickMu.Lock()
	defer pendingPlanPickMu.Unlock()
	if len(pendingPlanPickers) > 300 {
		now := time.Now()
		for k, v := range pendingPlanPickers {
			if now.After(v.ExpiresAt) {
				delete(pendingPlanPickers, k)
			}
		}
	}
	cp := make([]ramstore.SubscriptionPlan, len(plans))
	copy(cp, plans)
	pendingPlanPickers[clean] = pendingPlanPicker{
		Plans:     cp,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
}

func getPendingPlanPicker(phone string) ([]ramstore.SubscriptionPlan, bool) {
	clean := FormatPhone(phone)
	if clean == "" {
		return nil, false
	}
	pendingPlanPickMu.Lock()
	defer pendingPlanPickMu.Unlock()
	entry, ok := pendingPlanPickers[clean]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.ExpiresAt) {
		delete(pendingPlanPickers, clean)
		return nil, false
	}
	return entry.Plans, true
}

func clearPendingPlanPicker(phone string) {
	clean := FormatPhone(phone)
	if clean == "" {
		return
	}
	pendingPlanPickMu.Lock()
	delete(pendingPlanPickers, clean)
	pendingPlanPickMu.Unlock()
}

func matchPlanFromText(query string, plans []ramstore.SubscriptionPlan) (*ramstore.SubscriptionPlan, bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" || len(plans) == 0 {
		return nil, false
	}
	// Strip command prefixes & cycle words to check index or name cleanly
	clean := q
	for _, prefix := range []string{"/renew", "/upgrade", "renew", "upgrade", "plan"} {
		clean = strings.TrimSpace(strings.TrimPrefix(clean, prefix))
	}
	noCycle := strings.TrimSpace(strings.NewReplacer(
		"yearly", "", "annual", "", "monthly", "", "month", "", "year", "", "12 months", "", "saal", "",
	).Replace(clean))

	// 1. Numeric selection: "1", "2", etc.
	if idx, err := strconv.Atoi(noCycle); err == nil && idx >= 1 && idx <= len(plans) {
		p := plans[idx-1]
		return &p, true
	}

	// 2. Exact or partial match on plan ID or Name
	if noCycle != "" {
		for i := range plans {
			idLow := strings.ToLower(strings.TrimSpace(plans[i].ID))
			nameLow := strings.ToLower(strings.TrimSpace(plans[i].Name))
			if idLow == noCycle || nameLow == noCycle {
				p := plans[i]
				return &p, true
			}
		}
		for i := range plans {
			idLow := strings.ToLower(strings.TrimSpace(plans[i].ID))
			nameLow := strings.ToLower(strings.TrimSpace(plans[i].Name))
			if (nameLow != "" && (strings.Contains(nameLow, noCycle) || strings.Contains(noCycle, nameLow))) ||
				(idLow != "" && strings.Contains(noCycle, idLow)) {
				p := plans[i]
				return &p, true
			}
		}
	}
	return nil, false
}

func formatAdminHelpMenu(vendor *ramstore.ManagedVendor, vm ramstore.VendorManagementState) string {
	var sb strings.Builder
	sb.WriteString("👋 *WaMart Vendor Support Menu*\n")
	if vendor != nil {
		name := strings.TrimSpace(vendor.StoreName)
		if name == "" {
			name = strings.TrimSpace(vendor.Username)
		}
		if name != "" {
			sb.WriteString(fmt.Sprintf("🏪 Store: *%s*\n", name))
		}
	}
	sb.WriteString("\nQuick self-service commands:\n")
	sb.WriteString("• *my plan* — Check active subscription, expiry & days remaining\n")
	sb.WriteString("• *renew* / *upgrade* — Renew or upgrade your plan via instant UPI QR\n")
	sb.WriteString("• *store status* — Check Store URL, UPI VPA, Bot & Plan status\n")
	sb.WriteString("• *mall status* — Check WaMart Digital Mall listing eligibility\n")
	sb.WriteString("• *help* — Show this menu again\n")
	sb.WriteString("\n💬 Ya phir aap koi bhi sawaal seedha type karke pooch sakte hain!")
	if esc := strings.TrimSpace(vm.Settings.SupportEscalationPhone); esc != "" {
		sb.WriteString(fmt.Sprintf("\n📞 Priority Support: *+%s*", FormatPhone(esc)))
	}
	return sb.String()
}

func formatUnregisteredVendorNotice(vm ramstore.VendorManagementState) string {
	var sb strings.Builder
	sb.WriteString("ℹ️ Ye WhatsApp number abhi kisi registered WaMart Vendor Store se linked nahi mila.\n\n")
	sb.WriteString("Kripya apne *registered owner WhatsApp number* se message karein, ya apna store username share karein.")
	if esc := strings.TrimSpace(vm.Settings.SupportEscalationPhone); esc != "" {
		sb.WriteString(fmt.Sprintf("\n📞 Admin Support: *+%s*", FormatPhone(esc)))
	}
	return sb.String()
}

func formatMyPlanReply(v *ramstore.ManagedVendor, vm ramstore.VendorManagementState) string {
	if v == nil {
		return formatUnregisteredVendorNotice(vm)
	}
	storeName := strings.TrimSpace(v.StoreName)
	if storeName == "" {
		storeName = v.Username
	}
	planName := strings.TrimSpace(v.PlanName)
	if planName == "" {
		planName = "Standard"
	}
	validUntil := strings.TrimSpace(v.PlanValidUntil)
	if validUntil == "" {
		validUntil = "Not set"
	}

	statusBadge := "✅ Active"
	if v.IsPlanExpired {
		statusBadge = "🔴 Expired"
	} else if v.DaysRemaining <= 7 {
		statusBadge = "🟡 Expiring Soon"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 *Subscription Plan Details*\n🏪 *%s*", storeName))
	if strings.TrimSpace(v.Username) != "" {
		sb.WriteString(fmt.Sprintf(" (`@%s`)", strings.TrimSpace(v.Username)))
	}
	sb.WriteString("\n\n")
	sb.WriteString(fmt.Sprintf("• *Active Plan:* %s\n", planName))
	sb.WriteString(fmt.Sprintf("• *Valid Until:* %s\n", validUntil))
	sb.WriteString(fmt.Sprintf("• *Days Remaining:* %d day(s)\n", v.DaysRemaining))
	sb.WriteString(fmt.Sprintf("• *Status:* %s\n", statusBadge))
	if vm.Settings.WhatsAppPlanRenewalQR && strings.TrimSpace(vm.OwnerPaymentGateway.UPIVPA) != "" {
		sb.WriteString("\n💡 Instant UPI QR se renew ya upgrade karne ke liye *renew* ya *upgrade* likhein.")
	}
	return sb.String()
}

func formatStoreStatusReply(v *ramstore.ManagedVendor, vm ramstore.VendorManagementState) string {
	if v == nil {
		return formatUnregisteredVendorNotice(vm)
	}
	storeName := strings.TrimSpace(v.StoreName)
	if storeName == "" {
		storeName = v.Username
	}
	storeURL := strings.TrimSpace(v.StoreURL)
	if storeURL == "" {
		storeURL = "Not configured"
	}
	upiStatus := "⚠️ Not configured"
	if strings.TrimSpace(v.UPIID) != "" {
		upiStatus = fmt.Sprintf("✅ Configured (%s)", strings.TrimSpace(v.UPIID))
	}
	botStatus := "🔴 Disconnected"
	if v.BotConnected {
		botStatus = "✅ Connected"
	}
	planStatus := fmt.Sprintf("✅ Active (%s • %d days left)", v.PlanName, v.DaysRemaining)
	if v.IsPlanExpired {
		planStatus = fmt.Sprintf("🔴 Expired (%s)", v.PlanName)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🏪 *Store Health & Status — %s*\n\n", storeName))
	sb.WriteString(fmt.Sprintf("• *Store URL:* %s\n", storeURL))
	sb.WriteString(fmt.Sprintf("• *UPI VPA:* %s\n", upiStatus))
	sb.WriteString(fmt.Sprintf("• *WhatsApp Bot:* %s\n", botStatus))
	if strings.TrimSpace(v.BotPublicURL) != "" {
		sb.WriteString(fmt.Sprintf("• *Bot Node:* %s\n", strings.TrimSpace(v.BotPublicURL)))
	}
	sb.WriteString(fmt.Sprintf("• *Plan Status:* %s\n", planStatus))
	if v.IsPlanExpired {
		sb.WriteString("\n⚡ Plan renew karne ke liye *renew* type karein.")
	}
	return sb.String()
}

func formatMallStatusReply(v *ramstore.ManagedVendor, vm ramstore.VendorManagementState) string {
	if v == nil {
		return formatUnregisteredVendorNotice(vm)
	}
	storeName := strings.TrimSpace(v.StoreName)
	if storeName == "" {
		storeName = v.Username
	}
	planName := strings.TrimSpace(v.PlanName)
	if planName == "" {
		planName = "Standard"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🏬 *WaMart Digital Mall Status — %s*\n\n", storeName))

	if v.IsPlanExpired {
		sb.WriteString("• *Mall Eligibility:* 🔴 Paused (Plan Expired)\n")
		sb.WriteString(fmt.Sprintf("• *Plan:* %s (Expired)\n", planName))
		sb.WriteString("\nApna store WaMart Digital Mall me dubara live karne ke liye *renew* likh kar apna plan renew karein! 🚀")
		return sb.String()
	}

	mallLabel := "✅ Eligible & Active"
	if v.MallApproved != nil && !*v.MallApproved {
		mallLabel = "⏳ Pending Admin Mall Approval"
	}
	sb.WriteString(fmt.Sprintf("• *Mall Eligibility:* %s\n", mallLabel))
	sb.WriteString(fmt.Sprintf("• *Active Plan:* %s (%d days remaining)\n", planName, v.DaysRemaining))
	if strings.TrimSpace(v.StoreURL) != "" {
		sb.WriteString(fmt.Sprintf("• *Storefront URL:* %s\n", strings.TrimSpace(v.StoreURL)))
	}
	if strings.TrimSpace(v.UPIID) == "" {
		sb.WriteString("• *Tip:* Faster online orders ke liye apne Vendor Portal me UPI VPA add kar lein.\n")
	}
	sb.WriteString("\nAapka active plan WaMart Digital Mall visibility ke liye fully eligible hai! ✨")
	return sb.String()
}

func formatPlanCatalogPicker(v *ramstore.ManagedVendor, plans []ramstore.SubscriptionPlan) string {
	var sb strings.Builder
	sb.WriteString("🚀 *WaMart Subscription Plans — Renew / Upgrade*\n")
	if v != nil && strings.TrimSpace(v.StoreName) != "" {
		sb.WriteString(fmt.Sprintf("🏪 Store: *%s*\n", strings.TrimSpace(v.StoreName)))
	}
	sb.WriteString("\n")
	for i, p := range plans {
		curTag := ""
		if v != nil && ((v.PlanID != "" && strings.EqualFold(v.PlanID, p.ID)) || (v.PlanName != "" && strings.EqualFold(v.PlanName, p.Name))) {
			curTag = " _(Current)_"
		}
		sb.WriteString(fmt.Sprintf("*%d. %s*%s\n", i+1, p.Name, curTag))
		var prices []string
		if p.MonthlyPrice > 0 {
			prices = append(prices, fmt.Sprintf("Monthly: *₹%.0f* (%d days)", p.MonthlyPrice, p.ValidityDays))
		}
		if p.YearlyPrice > 0 {
			prices = append(prices, fmt.Sprintf("Yearly: *₹%.0f*", p.YearlyPrice))
		}
		if len(prices) > 0 {
			sb.WriteString("   💰 " + strings.Join(prices, " | ") + "\n")
		}
		if strings.TrimSpace(p.Description) != "" {
			sb.WriteString("   📝 " + strings.TrimSpace(p.Description) + "\n")
		}
	}
	sb.WriteString("\n👇 Instant UPI QR generate karne ke liye plan ka *number ya naam* reply karein (jaise: *1* ya *1 yearly* ya *renew pro*).")
	return sb.String()
}

// executePlanRenewalQR generates the dynamic UPI QR stamped on the cached Universal Payment QR Banner,
// delivers it on WhatsApp, and starts the local 3-second Paytm status poller when paytm_mid is present.
func executePlanRenewalQR(
	client *whatsmeow.Client,
	phone string,
	vendor *ramstore.ManagedVendor,
	plan ramstore.SubscriptionPlan,
	billingCycle string,
	amount float64,
	validityDays int,
	vm ramstore.VendorManagementState,
	allowWhatsAppSideEffects bool,
) (caption string, qrSent bool, err error) {
	cleanPhone := FormatPhone(phone)
	gw := vm.OwnerPaymentGateway
	if strings.TrimSpace(gw.UPIVPA) == "" {
		return "", false, fmt.Errorf("owner_payment_gateway.upi_vpa is not configured")
	}
	payeeName := strings.TrimSpace(gw.PayeeName)
	if payeeName == "" {
		payeeName = "WaMart"
	}

	orderID := generatePlanRenewalOrderID()
	upiStr := BuildPlanRenewalUPIString(gw.UPIVPA, payeeName, orderID, amount)
	if upiStr == "" {
		return "", false, fmt.Errorf("failed building UPI intent")
	}

	payCfg := ramstore.Global.GetPayment()
	imgBytes, mimeType, qrErr := GenerateLocalBannerQRBytes(upiStr, payCfg.BannerTemplate)
	if qrErr != nil || len(imgBytes) == 0 {
		return "", false, fmt.Errorf("failed generating banner QR: %w", qrErr)
	}

	storeLabel := "Your Store"
	vendorUsername := ""
	if vendor != nil {
		if strings.TrimSpace(vendor.StoreName) != "" {
			storeLabel = strings.TrimSpace(vendor.StoreName)
		} else if strings.TrimSpace(vendor.Username) != "" {
			storeLabel = strings.TrimSpace(vendor.Username)
		}
		vendorUsername = strings.TrimSpace(vendor.Username)
	}

	cycleLabel := "Monthly"
	if strings.EqualFold(billingCycle, "yearly") {
		cycleLabel = "Yearly"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("💎 *WaMart Plan Renewal — %s*\n\n", plan.Name))
	sb.WriteString(fmt.Sprintf("• *Store:* %s\n", storeLabel))
	sb.WriteString(fmt.Sprintf("• *Plan:* %s (%s • %d days)\n", plan.Name, cycleLabel, validityDays))
	sb.WriteString(fmt.Sprintf("• *Amount:* ₹%.2f\n", amount))
	sb.WriteString(fmt.Sprintf("• *Order ID:* #%s\n\n", orderID))
	if strings.TrimSpace(gw.PaytmMID) != "" {
		sb.WriteString("📲 Upar diye gaye QR code ko scan karke pay karein. Payment hote hi aapka plan *automatically upgrade* ho jayega! ✨")
	} else {
		sb.WriteString("📲 Upar diye gaye QR code ko scan karke pay karein. Payment confirm hote hi aapka plan update kar diya jayega! ✨")
	}
	caption = sb.String()

	clearPendingPlanPicker(cleanPhone)

	if allowWhatsAppSideEffects && client != nil && client.IsLoggedIn() {
		thumb := generateWhatsAppJPEGThumbnail(imgBytes)
		if sendErr := SendImageBytesWithThumb(client, cleanPhone, imgBytes, thumb, mimeType, caption); sendErr != nil {
			return "", false, fmt.Errorf("failed sending renewal QR on WhatsApp: %w", sendErr)
		}
		qrSent = true
		botUser := adminBotUsername()
		SaveCustomerChatAndPrune(botUser, cleanPhone, "assistant", caption)
		PushToLiveChat(botUser, cleanPhone, caption, "outgoing_ai")

		log.Printf("🎨 [Admin Renewal QR Sent] Order #%s | Vendor @%s (+%s) | Plan: %s (%s) | ₹%.2f | %s (%.1f KB)",
			orderID, vendorUsername, cleanPhone, plan.Name, cycleLabel, amount, mimeType, float64(len(imgBytes))/1024.0)

		if strings.TrimSpace(gw.PaytmMID) != "" {
			startPlanRenewalPaytmPoller(planRenewalPollParams{
				OrderID:        orderID,
				OwnerPhone:     cleanPhone,
				VendorUsername: vendorUsername,
				StoreName:      storeLabel,
				Plan:           plan,
				BillingCycle:   billingCycle,
				ValidityDays:   validityDays,
				Amount:         amount,
				PaytmMID:       strings.TrimSpace(gw.PaytmMID),
				PaytmStatusURL: payCfg.PaytmStatusURL,
			})
		}
	}

	return caption, qrSent, nil
}

type planRenewalPollParams struct {
	OrderID        string
	OwnerPhone     string
	VendorUsername string
	StoreName      string
	Plan           ramstore.SubscriptionPlan
	BillingCycle   string
	ValidityDays   int
	Amount         float64
	PaytmMID       string
	PaytmStatusURL string
}

// startPlanRenewalPaytmPoller polls https://securegw.paytm.in/order/status locally every 3s up to 5 min.
// On TXN_SUCCESS, notifies Central Brain so the vendor's plan upgrades automatically and confirms on WhatsApp.
func startPlanRenewalPaytmPoller(params planRenewalPollParams) {
	orderID := strings.TrimSpace(params.OrderID)
	cleanPhone := FormatPhone(params.OwnerPhone)
	if orderID == "" || cleanPhone == "" || strings.TrimSpace(params.PaytmMID) == "" {
		return
	}
	if IsOrderPaidNotified(orderID) {
		return
	}

	payCfg := ramstore.Global.GetPayment()
	interval := time.Duration(ramstore.DefaultVerifyPollIntervalMS) * time.Millisecond
	if payCfg.VerifyPollIntervalMS > 0 {
		interval = time.Duration(payCfg.VerifyPollIntervalMS) * time.Millisecond
	}
	timeout := time.Duration(ramstore.DefaultVerifyPollTimeoutMS) * time.Millisecond
	if payCfg.VerifyPollTimeoutMS > 0 {
		timeout = time.Duration(payCfg.VerifyPollTimeoutMS) * time.Millisecond
	}
	statusURL := strings.TrimSpace(params.PaytmStatusURL)
	if statusURL == "" {
		statusURL = ramstore.DefaultPaytmStatusURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	activePollersMu.Lock()
	if _, exists := activePollers[orderID]; exists {
		activePollersMu.Unlock()
		cancel()
		return
	}
	pollerTokenSeq++
	myToken := pollerTokenSeq
	activePollers[orderID] = activePollerEntry{cancel: cancel, token: myToken}
	activePollersMu.Unlock()

	log.Printf("🔄 [Plan Renewal Poller] Started %v direct Paytm poll for Order #%s (vendor=@%s, plan=%s, timeout=%v)",
		interval, orderID, params.VendorUsername, params.Plan.Name, timeout)

	go func() {
		ticker := time.NewTicker(interval)
		defer func() {
			ticker.Stop()
			cancel()
			activePollersMu.Lock()
			if cur, ok := activePollers[orderID]; ok && cur.token == myToken {
				delete(activePollers, orderID)
			}
			activePollersMu.Unlock()
		}()

		for {
			select {
			case <-ctx.Done():
				if ctx.Err() == context.DeadlineExceeded && !IsOrderPaidNotified(orderID) {
					log.Printf("⏱️ [Plan Renewal Poller] Timeout elapsed for Order #%s after %v", orderID, timeout)
					timeoutMsg := fmt.Sprintf("⏱️ Plan renewal payment verification for Order #%s timed out. Agar aapne payment kar diya hai, toh kripya thoda wait karein ya support se sampark karein — ya dubara *renew* likhein.", orderID)
					botUser := adminBotUsername()
					if waClient := GetAnyLoggedInClient(); waClient != nil && waClient.IsLoggedIn() {
						_ = SendMessage(waClient, cleanPhone, timeoutMsg)
					}
					SaveCustomerChatAndPrune(botUser, cleanPhone, "assistant", timeoutMsg)
					PushToLiveChat(botUser, cleanPhone, timeoutMsg, "outgoing_ai")
				}
				return

			case <-ticker.C:
				if IsOrderPaidNotified(orderID) {
					return
				}
				pResp, err := CheckPaytmStatusDirect(statusURL, params.PaytmMID, orderID)
				if err != nil {
					log.Printf("⚠️ [Plan Renewal Paytm Poll] Transient error for Order #%s (retrying): %v", orderID, err)
					continue
				}
				if pResp == nil || !strings.EqualFold(strings.TrimSpace(pResp.STATUS), "TXN_SUCCESS") {
					continue
				}

				txnID := strings.TrimSpace(pResp.TXNID)
				paidAmount := params.Amount
				if parsedAmt, aErr := strconv.ParseFloat(strings.TrimSpace(pResp.TXNAMOUNT), 64); aErr == nil && parsedAmt > 0 {
					paidAmount = parsedAmt
				}

				if !MarkOrderPaidNotifiedOnce(orderID) {
					return
				}

				log.Printf("✅ [Plan Renewal Poller] TXN_SUCCESS for Order #%s (vendor=@%s, plan=%s, txn=%s, ₹%.2f)",
					orderID, params.VendorUsername, params.Plan.Name, txnID, paidAmount)

				// Optimistically update vendor plan in local RAM so immediate "my plan" queries show upgraded plan
				updatedVendor := ramstore.Global.MarkManagedVendorPlanRenewed(cleanPhone, params.Plan, params.BillingCycle)
				validUntilNote := ""
				if updatedVendor != nil && strings.TrimSpace(updatedVendor.PlanValidUntil) != "" {
					validUntilNote = updatedVendor.PlanValidUntil
				}

				// Notify Central Brain so the vendor's subscription upgrades automatically
				go func() {
					_ = central.Client.PushPlanRenewalConfirmedWithRetry(central.PlanRenewalConfirmedEvent{
						Event:          "plan.renewal.confirmed",
						Type:           "plan_renewal",
						OrderID:        orderID,
						TxnID:          txnID,
						Amount:         paidAmount,
						PaytmStatus:    "TXN_SUCCESS",
						Username:       params.VendorUsername,
						VendorUsername: params.VendorUsername,
						StoreName:      params.StoreName,
						OwnerPhone:     cleanPhone,
						CustomerPhone:  cleanPhone,
						PlanID:         params.Plan.ID,
						PlanName:       params.Plan.Name,
						BillingCycle:   params.BillingCycle,
						ValidityDays:   params.ValidityDays,
						Timestamp:      time.Now().UTC().Format(time.RFC3339),
					})
				}()

				// Deliver confirmation on WhatsApp
				var sb strings.Builder
				sb.WriteString("🎉 *Plan Renewal Confirmed!*\n\n")
				sb.WriteString(fmt.Sprintf("• *Store:* %s\n", params.StoreName))
				sb.WriteString(fmt.Sprintf("• *Upgraded Plan:* %s\n", params.Plan.Name))
				if validUntilNote != "" {
					sb.WriteString(fmt.Sprintf("• *New Valid Until:* %s\n", validUntilNote))
				}
				sb.WriteString(fmt.Sprintf("• *Amount Paid:* ₹%.2f\n", paidAmount))
				sb.WriteString(fmt.Sprintf("• *Order ID:* #%s\n", orderID))
				if txnID != "" {
					sb.WriteString(fmt.Sprintf("• *Transaction ID:* %s\n", txnID))
				}
				sb.WriteString("\n✅ Aapka WaMart subscription automatically upgrade ho gaya hai. Thank you for partnering with WaMart! 🚀")

				confMsg := sb.String()
				botUser := adminBotUsername()
				if waClient := GetAnyLoggedInClient(); waClient != nil && waClient.IsLoggedIn() {
					_ = SendMessage(waClient, cleanPhone, confMsg)
				}
				SaveCustomerChatAndPrune(botUser, cleanPhone, "assistant", confMsg)
				PushToLiveChat(botUser, cleanPhone, confMsg, "outgoing_ai")
				return
			}
		}
	}()
}

// tryHandleAdminSelfServiceCommand evaluates instant vendor commands when vendor_self_service_commands is enabled.
// Returns (handled, replyText, qrSent, commandName, err).
func tryHandleAdminSelfServiceCommand(
	phone string,
	incomingText string,
	vendor *ramstore.ManagedVendor,
	vm ramstore.VendorManagementState,
	allowWhatsAppSideEffects bool,
	client *whatsmeow.Client,
) (bool, string, bool, string, error) {
	if !vm.Settings.VendorSelfServiceCommands {
		return false, "", false, "", nil
	}

	raw := strings.TrimSpace(incomingText)
	low := strings.ToLower(strings.TrimPrefix(raw, "/"))
	low = strings.Join(strings.Fields(low), " ")

	switch low {
	case "my plan", "plan", "myplan", "subscription":
		clearPendingPlanPicker(phone)
		return true, formatMyPlanReply(vendor, vm), false, "my_plan", nil

	case "store status", "status", "bot status", "storestatus":
		clearPendingPlanPicker(phone)
		return true, formatStoreStatusReply(vendor, vm), false, "store_status", nil

	case "mall status", "mall", "mallstatus":
		clearPendingPlanPicker(phone)
		return true, formatMallStatusReply(vendor, vm), false, "mall_status", nil

	case "help", "menu", "commands":
		clearPendingPlanPicker(phone)
		return true, formatAdminHelpMenu(vendor, vm), false, "help", nil
	}

	// Check renew / upgrade command OR follow-up reply to a pending plan picker
	isRenewCmd := low == "renew" || strings.HasPrefix(low, "renew ") || low == "upgrade" || strings.HasPrefix(low, "upgrade ")
	pendingPlans, hasPendingPicker := getPendingPlanPicker(phone)

	if !isRenewCmd && hasPendingPicker {
		if matchedPlan, ok := matchPlanFromText(low, pendingPlans); ok && matchedPlan != nil {
			if vm.Settings.WhatsAppPlanRenewalQR && strings.TrimSpace(vm.OwnerPaymentGateway.UPIVPA) != "" {
				cycle, amount, validity := selectBillingCycleAndPrice(*matchedPlan, low)
				caption, qrSent, qrErr := executePlanRenewalQR(client, phone, vendor, *matchedPlan, cycle, amount, validity, vm, allowWhatsAppSideEffects)
				if qrErr != nil {
					return true, fmt.Sprintf("⚠️ Plan renewal QR generate nahi ho paya (%v). Kripya thodi der baad dubara try karein.", qrErr), false, "renew_qr_error", nil
				}
				return true, caption, qrSent, "renew_qr", nil
			}
		}
	}

	if isRenewCmd {
		paidPlans := getPaidSubscriptionPlans(vm.SubscriptionPlans)
		if !vm.Settings.WhatsAppPlanRenewalQR || strings.TrimSpace(vm.OwnerPaymentGateway.UPIVPA) == "" {
			var sb strings.Builder
			sb.WriteString("ℹ️ WhatsApp instant UPI QR renewal abhi enabled nahi hai.")
			if len(paidPlans) > 0 {
				sb.WriteString("\n\nAvailable WaMart Plans:\n")
				for _, p := range paidPlans {
					sb.WriteString(fmt.Sprintf("• *%s* — ₹%.0f/month", p.Name, p.MonthlyPrice))
					if p.YearlyPrice > 0 {
						sb.WriteString(fmt.Sprintf(" | ₹%.0f/year", p.YearlyPrice))
					}
					sb.WriteString("\n")
				}
			}
			sb.WriteString("\nKripya WaMart Vendor Portal se renew karein")
			if esc := strings.TrimSpace(vm.Settings.SupportEscalationPhone); esc != "" {
				sb.WriteString(fmt.Sprintf(" ya support par sampark karein: *+%s*", FormatPhone(esc)))
			}
			sb.WriteString(".")
			return true, sb.String(), false, "renew_unavailable", nil
		}

		if len(paidPlans) == 0 {
			return true, "ℹ️ Abhi koi active paid subscription plan configured nahi hai. Kripya WaMart Admin Support se sampark karein.", false, "renew_no_plans", nil
		}

		// Check if user specified a plan directly (e.g. "renew pro", "upgrade 2 yearly")
		var selectedPlan *ramstore.SubscriptionPlan
		if matched, ok := matchPlanFromText(low, paidPlans); ok && matched != nil {
			selectedPlan = matched
		} else if len(paidPlans) == 1 {
			p := paidPlans[0]
			selectedPlan = &p
		}

		if selectedPlan != nil {
			cycle, amount, validity := selectBillingCycleAndPrice(*selectedPlan, low)
			caption, qrSent, qrErr := executePlanRenewalQR(client, phone, vendor, *selectedPlan, cycle, amount, validity, vm, allowWhatsAppSideEffects)
			if qrErr != nil {
				return true, fmt.Sprintf("⚠️ Plan renewal QR generate nahi ho paya (%v). Kripya thodi der baad try karein.", qrErr), false, "renew_qr_error", nil
			}
			return true, caption, qrSent, "renew_qr", nil
		}

		// Multiple paid plans available: show interactive numbered picker & store 10-minute pending selection
		setPendingPlanPicker(phone, paidPlans)
		return true, formatPlanCatalogPicker(vendor, paidPlans), false, "renew_picker", nil
	}

	return false, "", false, "", nil
}

// buildAdminVendorManagerSystemPrompt crafts the system prompt for WaMart's Vendor Support Manager AI,
// injecting ai.system_prompt, ai.store_policies, matched vendor live context, and subscription_plans.
func buildAdminVendorManagerSystemPrompt(vendor *ramstore.ManagedVendor, vm ramstore.VendorManagementState, aiCfg ramstore.AIConfig, botProfile ramstore.VendorProfile) string {
	var sb strings.Builder

	sb.WriteString("You are WaMart's official Vendor Support Manager on WhatsApp. ")
	sb.WriteString("You help WaMart store owners with their subscription plans, renewals, store setup, WhatsApp bot connectivity, UPI configuration, and WaMart Digital Mall onboarding/eligibility.\n")
	sb.WriteString("STRICT RULES:\n")
	sb.WriteString("- NEVER act as a grocery store, retail shop, or take customer product/cart orders.\n")
	sb.WriteString("- Reply naturally, warmly, and concisely in the vendor's language (English or Hinglish matching their message) formatted cleanly for WhatsApp.\n")
	sb.WriteString("- Never invent plan prices, dates, or vendor metrics — use ONLY the live context provided below.\n")
	sb.WriteString("- Remind vendors they can use instant commands: `my plan`, `renew`, `store status`, `mall status`, `help` whenever helpful.\n\n")

	if sp := strings.TrimSpace(aiCfg.SystemPrompt); sp != "" {
		sb.WriteString("ADMIN CUSTOM INSTRUCTIONS:\n")
		sb.WriteString(sp + "\n\n")
	}

	policies := strings.TrimSpace(aiCfg.StorePolicies)
	if policies == "" {
		policies = strings.TrimSpace(botProfile.StorePolicies)
	}
	if policies != "" {
		sb.WriteString("PLATFORM POLICIES:\n")
		sb.WriteString(policies + "\n\n")
	}

	sb.WriteString("MATCHED VENDOR LIVE CONTEXT:\n")
	if vendor != nil {
		statusStr := "Active"
		if vendor.IsPlanExpired {
			statusStr = "Expired"
		}
		upiStr := strings.TrimSpace(vendor.UPIID)
		if upiStr == "" {
			upiStr = "Not Configured"
		}
		botStr := "Disconnected"
		if vendor.BotConnected {
			botStr = "Connected"
		}
		sb.WriteString(fmt.Sprintf("- Store Name: %s\n", vendor.StoreName))
		sb.WriteString(fmt.Sprintf("- Username: %s\n", vendor.Username))
		sb.WriteString(fmt.Sprintf("- Plan Name: %s (ID: %s)\n", vendor.PlanName, vendor.PlanID))
		sb.WriteString(fmt.Sprintf("- Plan Valid Until: %s\n", vendor.PlanValidUntil))
		sb.WriteString(fmt.Sprintf("- Days Remaining: %d\n", vendor.DaysRemaining))
		sb.WriteString(fmt.Sprintf("- Plan Status: %s\n", statusStr))
		sb.WriteString(fmt.Sprintf("- Bot Connected: %s\n", botStr))
		if strings.TrimSpace(vendor.BotPublicURL) != "" {
			sb.WriteString(fmt.Sprintf("- Bot Public URL: %s\n", vendor.BotPublicURL))
		}
		if strings.TrimSpace(vendor.StoreURL) != "" {
			sb.WriteString(fmt.Sprintf("- Store URL: %s\n", vendor.StoreURL))
		}
		sb.WriteString(fmt.Sprintf("- Vendor UPI ID: %s\n", upiStr))
	} else {
		sb.WriteString("- Sender phone is NOT matched in managed_vendors (unregistered or messaging from a different number). Politely assist with general WaMart vendor queries or ask them to message from their registered owner phone.\n")
	}

	if len(vm.SubscriptionPlans) > 0 {
		sb.WriteString("\nACTIVE WAMART SUBSCRIPTION PLANS:\n")
		for _, p := range vm.SubscriptionPlans {
			sb.WriteString(fmt.Sprintf("- %s (id=%s): Monthly ₹%.0f | Yearly ₹%.0f | Validity %d days",
				p.Name, p.ID, p.MonthlyPrice, p.YearlyPrice, p.ValidityDays))
			if strings.TrimSpace(p.Description) != "" {
				sb.WriteString(" — " + strings.TrimSpace(p.Description))
			}
			sb.WriteString("\n")
		}
	}

	if vm.Settings.WhatsAppPlanRenewalQR && strings.TrimSpace(vm.OwnerPaymentGateway.UPIVPA) != "" {
		sb.WriteString("\nNOTE: Instant WhatsApp UPI QR renewal is ENABLED. Tell the vendor to type `renew` or `upgrade` if they want to pay right now on WhatsApp.\n")
	}
	if esc := strings.TrimSpace(vm.Settings.SupportEscalationPhone); esc != "" {
		sb.WriteString(fmt.Sprintf("SUPPORT ESCALATION PHONE: +%s\n", FormatPhone(esc)))
	}

	return sb.String()
}

// ProcessAdminVendorText handles an incoming message in admin_vendor_manager mode.
// Used by both live WhatsApp messages (allowWhatsAppSideEffects=true) and /api/ai/test playground (allowWhatsAppSideEffects=false).
func ProcessAdminVendorText(phone, incomingText string, allowWhatsAppSideEffects bool, client *whatsmeow.Client) (replyText string, qrSent bool, handledCommand string, err error) {
	cleanPhone := FormatPhone(phone)
	vm := ramstore.Global.GetVendorManagement()
	matchedVendor, _ := ramstore.Global.LookupManagedVendorByPhone(cleanPhone)

	// 1. Instant Self-Service Commands (if enabled)
	handled, cmdReply, sentQR, cmdName, cmdErr := tryHandleAdminSelfServiceCommand(
		cleanPhone, incomingText, matchedVendor, vm, allowWhatsAppSideEffects, client,
	)
	if handled {
		return cmdReply, sentQR, cmdName, cmdErr
	}

	// 2. Natural-Language Vendor Support LLM (strictly last 10 messages in bot RAM only)
	botUser := adminBotUsername()
	cfg := GetUserConfigRAM(botUser)
	aiCfg := ramstore.Global.GetAI()
	botProfile := ramstore.Global.GetVendor()
	sysPrompt := buildAdminVendorManagerSystemPrompt(matchedVendor, vm, aiCfg, botProfile)

	history := getSlidingWindowRAM(botUser, cleanPhone)
	if len(history) == 0 || history[len(history)-1].Role != "user" || history[len(history)-1].Content != strings.TrimSpace(incomingText) {
		history = append(history, ChatMessage{Role: "user", Content: strings.TrimSpace(incomingText)})
	}
	if len(history) > ramstore.MaxChatsPerCustomer {
		history = history[len(history)-ramstore.MaxChatsPerCustomer:]
	}

	rawReply, llmErr := callAIWithKeyFallback(botUser, cfg, sysPrompt, history, false)
	if llmErr != nil {
		return "", false, "llm_error", llmErr
	}
	cleanReply := SanitizeAIReplyText(stripThinkBlocks(rawReply))
	if strings.TrimSpace(cleanReply) == "" {
		cleanReply = formatAdminHelpMenu(matchedVendor, vm)
	}
	return cleanReply, false, "llm", nil
}

// HandleAdminVendorMessage executes the full WhatsApp turn in admin_vendor_manager mode
// (typing indicator, O(1) vendor lookup, instant commands / QR banner renewal / LLM reply, RAM-only chat history).
func HandleAdminVendorMessage(username string, client *whatsmeow.Client, msgID types.MessageID, msgTime time.Time, chatJID, senderJID types.JID, rawMsg *waE2E.Message, phone, text string) {
	_ = msgTime
	botUser := strings.TrimSpace(username)
	if botUser == "" {
		botUser = adminBotUsername()
	}

	// Persist incoming vendor message strictly in bot RAM (max 10 msgs) + live dashboard stream
	SaveCustomerChatAndPrune(botUser, phone, "customer", text)
	PushToLiveChat(botUser, phone, text, "incoming")

	// Natural typing presence while generating command/QR/LLM response
	stopTyping := make(chan struct{})
	go func() {
		_ = client.SendChatPresence(context.Background(), chatJID, types.ChatPresenceComposing, types.ChatPresenceMediaText)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopTyping:
				_ = client.SendChatPresence(context.Background(), chatJID, types.ChatPresencePaused, types.ChatPresenceMediaText)
				return
			case <-ticker.C:
				_ = client.SendChatPresence(context.Background(), chatJID, types.ChatPresenceComposing, types.ChatPresenceMediaText)
			}
		}
	}()

	replyText, qrSent, cmdName, err := ProcessAdminVendorText(phone, text, true, client)
	time.Sleep(300 * time.Millisecond)
	close(stopTyping)

	if err != nil {
		log.Printf("⚠️ [Admin Vendor Manager] Error processing message from +%s (%s): %v", phone, cmdName, err)
		return
	}

	// If QR banner image was already sent with caption, do NOT send duplicate text message
	if qrSent {
		return
	}

	replyText = strings.TrimSpace(replyText)
	if replyText == "" {
		return
	}

	if len([]rune(strings.TrimSpace(text))) > 60 && strings.Contains(text, "?") {
		if errSend := SendReplyMessage(client, phone, replyText, msgID, senderJID, rawMsg); errSend != nil {
			_ = SendMessage(client, phone, replyText)
		}
	} else {
		_ = SendMessage(client, phone, replyText)
	}
	SaveCustomerChatAndPrune(botUser, phone, "assistant", replyText)
	PushToLiveChat(botUser, phone, replyText, "outgoing_ai")
}

// =============================================================================
// LIGHTWEIGHT DAILY RENEWAL REMINDER SCHEDULER & ONBOARDING WELCOMES (Admin Mode)
// =============================================================================

// StartAdminRenewalReminderScheduler launches a single lightweight background ticker
// (in-memory deduplicated per vendor/day) that checks managed_vendors in RAM when
// bot_mode == "admin_vendor_manager" and auto_renewal_reminders == true.
func StartAdminRenewalReminderScheduler() {
	renewalReminderOnce.Do(func() {
		go func() {
			// Initial post-boot check after WhatsApp connects & bootstrap hydrates
			time.Sleep(90 * time.Second)
			RunAutoRenewalReminderCheck()

			ticker := time.NewTicker(15 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				RunAutoRenewalReminderCheck()
			}
		}()
	})
}

// RunAutoRenewalReminderCheck scans managed_vendors in RAM and dispatches renewal_template
// once per calendar day (IST) to any vendor whose days_remaining matches renewal_reminder_days.
func RunAutoRenewalReminderCheck() int {
	if !ramstore.Global.IsAdminVendorManagerMode() {
		return 0
	}
	vm := ramstore.Global.GetVendorManagement()
	if !vm.Settings.AutoRenewalReminders || len(vm.Settings.RenewalReminderDays) == 0 || len(vm.ManagedVendors) == 0 {
		return 0
	}
	client := GetAnyLoggedInClient()
	if client == nil || !client.IsLoggedIn() {
		return 0
	}

	dayMatch := make(map[int]bool, len(vm.Settings.RenewalReminderDays))
	for _, d := range vm.Settings.RenewalReminderDays {
		dayMatch[d] = true
	}

	today := time.Now().In(istLocation()).Format("2006-01-02")

	// Prune stale dates from previous days so memory remains strictly bounded
	renewalReminderSentMu.Lock()
	for k, sentDate := range renewalReminderSent {
		if sentDate != today {
			delete(renewalReminderSent, k)
		}
	}
	renewalReminderSentMu.Unlock()

	tpl := strings.TrimSpace(vm.Settings.RenewalTemplate)
	if tpl == "" {
		tpl = defaultAdminRenewalTemplate
	}

	botUser := adminBotUsername()
	sentCount := 0
	for _, v := range vm.ManagedVendors {
		if !dayMatch[v.DaysRemaining] {
			continue
		}
		if v.IsPlanExpired && !dayMatch[0] {
			continue
		}
		cleanPhone := FormatPhone(v.OwnerPhone)
		if cleanPhone == "" {
			continue
		}

		renewalReminderSentMu.Lock()
		alreadySent := renewalReminderSent[cleanPhone] == today
		renewalReminderSentMu.Unlock()
		if alreadySent {
			continue
		}

		msg := renderVendorTemplate(tpl, v)
		if sentCount > 0 {
			time.Sleep(3 * time.Second)
		}
		if err := SendMessage(client, cleanPhone, msg); err != nil {
			log.Printf("⚠️ [Auto Renewal Reminder] Failed sending to @%s (+%s): %v", v.Username, cleanPhone, err)
			continue
		}

		renewalReminderSentMu.Lock()
		renewalReminderSent[cleanPhone] = today
		renewalReminderSentMu.Unlock()

		SaveCustomerChatAndPrune(botUser, cleanPhone, "assistant", msg)
		PushToLiveChat(botUser, cleanPhone, msg, "outgoing_ai")
		sentCount++
		log.Printf("🔔 [Auto Renewal Reminder] Sent to %s (@%s, +%s) — %d day(s) remaining", v.StoreName, v.Username, cleanPhone, v.DaysRemaining)
	}
	return sentCount
}

// SendOnboardingWelcomesIfEnabled sends welcome_template to newly added vendors received via live
// /api/sync/vendor hot-reloads (never fires on initial startup bootstrap to avoid restart spam).
func SendOnboardingWelcomesIfEnabled(newVendors []ramstore.ManagedVendor) {
	if len(newVendors) == 0 || !ramstore.Global.IsAdminVendorManagerMode() {
		return
	}
	vm := ramstore.Global.GetVendorManagement()
	if !vm.Settings.AutoOnboardingWelcome {
		return
	}
	client := GetAnyLoggedInClient()
	if client == nil || !client.IsLoggedIn() {
		return
	}

	tpl := strings.TrimSpace(vm.Settings.WelcomeTemplate)
	if tpl == "" {
		tpl = defaultAdminWelcomeTemplate
	}
	botUser := adminBotUsername()

	go func(list []ramstore.ManagedVendor, templateStr string) {
		for i, v := range list {
			cleanPhone := FormatPhone(v.OwnerPhone)
			if cleanPhone == "" {
				continue
			}
			onboardingWelcomeMu.Lock()
			if onboardingWelcomeSent[cleanPhone] {
				onboardingWelcomeMu.Unlock()
				continue
			}
			onboardingWelcomeSent[cleanPhone] = true
			onboardingWelcomeMu.Unlock()

			if i > 0 {
				time.Sleep(3 * time.Second)
			}
			msg := renderVendorTemplate(templateStr, v)
			if err := SendMessage(client, cleanPhone, msg); err != nil {
				log.Printf("⚠️ [Auto Onboarding Welcome] Failed sending to @%s (+%s): %v", v.Username, cleanPhone, err)
				continue
			}
			SaveCustomerChatAndPrune(botUser, cleanPhone, "assistant", msg)
			PushToLiveChat(botUser, cleanPhone, msg, "outgoing_ai")
			log.Printf("🎉 [Auto Onboarding Welcome] Sent to %s (@%s, +%s)", v.StoreName, v.Username, cleanPhone)
		}
	}(newVendors, tpl)
}
