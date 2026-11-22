package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"wa-server/central"
	"wa-server/ramstore"
)

// PaymentPollParams configures a single non-blocking background 3-second payment verification loop.
type PaymentPollParams struct {
	OrderID         string
	CustomerPhone   string
	CustomerName    string
	DeliveryAddress string
	TotalAmount     float64
	PaytmMID        string
	VerifyEndpoint  string
	PollInterval    time.Duration
	PollTimeout     time.Duration
}

type activePollerEntry struct {
	cancel context.CancelFunc
	token  uint64
}

var (
	activePollersMu sync.Mutex
	activePollers   = make(map[string]activePollerEntry)
	pollerTokenSeq  uint64

	notifiedPaidMu     sync.Mutex
	notifiedPaidOrders = make(map[string]time.Time)
)

// IsOrderPaidNotified returns true if a paid confirmation WhatsApp alert has already been sent for orderID.
func IsOrderPaidNotified(orderID string) bool {
	id := strings.TrimSpace(orderID)
	if id == "" {
		return false
	}
	notifiedPaidMu.Lock()
	defer notifiedPaidMu.Unlock()
	_, exists := notifiedPaidOrders[id]
	return exists
}

// MarkOrderPaidNotifiedOnce atomically marks orderID as paid-notified and returns true ONLY on the first call.
// This prevents duplicate WhatsApp confirmation messages between the 3s verification poller and /api/sync/order.
func MarkOrderPaidNotifiedOnce(orderID string) bool {
	id := strings.TrimSpace(orderID)
	if id == "" {
		return false
	}
	now := time.Now()
	notifiedPaidMu.Lock()
	defer notifiedPaidMu.Unlock()

	// Opportunistic cleanup of entries older than 24h so 512MB RAM stays bounded
	if len(notifiedPaidOrders) > 500 {
		for k, ts := range notifiedPaidOrders {
			if now.Sub(ts) > 24*time.Hour {
				delete(notifiedPaidOrders, k)
			}
		}
	}
	if _, exists := notifiedPaidOrders[id]; exists {
		return false
	}
	notifiedPaidOrders[id] = now
	return true
}

// StopPaymentVerificationPoller cancels and removes any active 3-second verification loop for orderID.
func StopPaymentVerificationPoller(orderID string) {
	id := strings.TrimSpace(orderID)
	if id == "" {
		return
	}
	activePollersMu.Lock()
	entry, exists := activePollers[id]
	if exists {
		delete(activePollers, id)
	}
	activePollersMu.Unlock()
	if exists && entry.cancel != nil {
		entry.cancel()
		log.Printf("🛑 [Payment Poller] Stopped active verification loop for Order #%s", id)
	}
}

// GenerateBrandedBannerQR calls WaMart's Branded Banner QR Worker to create a Cloudinary banner QR image URL.
func GenerateBrandedBannerQR(qrGeneratorURL, upiID, payeeName, orderID string, totalAmount float64, cloudName string) (string, error) {
	genURL := strings.TrimSpace(qrGeneratorURL)
	if genURL == "" {
		genURL = ramstore.DefaultQRGeneratorURL
	}
	vpa := strings.TrimSpace(upiID)
	if vpa == "" {
		return "", fmt.Errorf("missing upi_id / merchant_vpa for banner QR generation")
	}
	name := strings.TrimSpace(payeeName)
	if name == "" {
		name = strings.TrimSpace(ramstore.Global.GetVendor().BusinessName)
		if name == "" {
			name = "Store"
		}
	}
	cloud := strings.TrimSpace(cloudName)
	if cloud == "" {
		cloud = ramstore.DefaultQRCloudName
	}

	reqPayload := map[string]interface{}{
		"vpa":       vpa,
		"name":      name,
		"orderId":   strings.TrimSpace(orderID),
		"amount":    totalAmount,
		"cloudName": cloud,
	}
	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", genURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "VendorWhatsAppBotNode/2.4")

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("banner QR worker HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	var parsed struct {
		Success   bool   `json:"success"`
		OrderID   string `json:"orderId"`
		ImageURL  string `json:"imageUrl"`
		ImageURL2 string `json:"image_url"`
		URL       string `json:"url"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("invalid banner QR worker JSON: %w", err)
	}
	imgURL := strings.TrimSpace(parsed.ImageURL)
	if imgURL == "" {
		imgURL = strings.TrimSpace(parsed.ImageURL2)
	}
	if imgURL == "" {
		imgURL = strings.TrimSpace(parsed.URL)
	}
	if imgURL == "" {
		return "", fmt.Errorf("banner QR worker returned empty imageUrl (%s)", parsed.Error)
	}
	return imgURL, nil
}

// StartPaymentVerificationPoller launches a single non-blocking 3-second polling goroutine for orderID.
// Duplicate calls for the same order_id are safely ignored.
func StartPaymentVerificationPoller(params PaymentPollParams) {
	orderID := strings.TrimSpace(params.OrderID)
	cleanPhone := FormatPhone(params.CustomerPhone)
	if orderID == "" || cleanPhone == "" {
		return
	}
	if IsOrderPaidNotified(orderID) {
		return
	}

	payCfg := ramstore.Global.GetPayment()
	interval := params.PollInterval
	if interval <= 0 {
		if payCfg.VerifyPollIntervalMS > 0 {
			interval = time.Duration(payCfg.VerifyPollIntervalMS) * time.Millisecond
		} else {
			interval = time.Duration(ramstore.DefaultVerifyPollIntervalMS) * time.Millisecond
		}
	}
	timeout := params.PollTimeout
	if timeout <= 0 {
		if payCfg.VerifyPollTimeoutMS > 0 {
			timeout = time.Duration(payCfg.VerifyPollTimeoutMS) * time.Millisecond
		} else {
			timeout = time.Duration(ramstore.DefaultVerifyPollTimeoutMS) * time.Millisecond
		}
	}
	verifyEndpoint := strings.TrimSpace(params.VerifyEndpoint)
	if verifyEndpoint == "" {
		verifyEndpoint = payCfg.VerifyEndpoint
	}
	if verifyEndpoint == "" {
		verifyEndpoint = ramstore.DefaultVerifyEndpoint
	}
	paytmMID := strings.TrimSpace(params.PaytmMID)
	if paytmMID == "" {
		paytmMID = strings.TrimSpace(payCfg.PaytmMID)
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

	log.Printf("🔄 [Payment Poller] Started %v verification loop for Order #%s (timeout=%v, endpoint=%s)", interval, orderID, timeout, verifyEndpoint)

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
				// Send timeout notice ONLY if the timeout actually elapsed (not when cancelled by webhook or paid check)
				if ctx.Err() == context.DeadlineExceeded && !IsOrderPaidNotified(orderID) {
					log.Printf("⏱️ [Payment Poller] Timeout elapsed for Order #%s after %v", orderID, timeout)
					timeoutMsg := fmt.Sprintf("Payment verification window for Order #%s timed out. If you already paid, please wait or contact the store with your payment screenshot.", orderID)
					vendorUser := ramstore.Global.GetVendor().Username
					if waClient := GetAnyLoggedInClient(); waClient != nil && waClient.IsLoggedIn() {
						_ = SendMessage(waClient, cleanPhone, timeoutMsg)
					}
					SaveCustomerChatAndPrune(vendorUser, cleanPhone, "assistant", timeoutMsg)
					PushToLiveChat(vendorUser, cleanPhone, timeoutMsg, "outgoing_ai")
				}
				return

			case <-ticker.C:
				if IsOrderPaidNotified(orderID) {
					return
				}
				vendor := ramstore.Global.GetVendor()
				verifyReq := central.VerifyPaymentRequest{
					OrderID:  orderID,
					ORDERID:  orderID,
					Username: vendor.Username,
					TenantID: vendor.Username,
					PaytmMID: paytmMID,
					Source:   "vendor_bot",
				}
				vResp, err := central.Client.VerifyPayment(verifyEndpoint, verifyReq)
				if err != nil {
					log.Printf("⚠️ [Payment Poller] Transient verify error for Order #%s (retrying): %v", orderID, err)
					continue
				}
				if vResp == nil {
					continue
				}

				// 1. PAID / VERIFIED
				if vResp.IsPaid || vResp.Verified || strings.EqualFold(strings.TrimSpace(vResp.PaymentStatus), "PAID") {
					log.Printf("✅ [Payment Poller] Payment VERIFIED for Order #%s (txn_id=%s)", orderID, vResp.TxnID)
					paidAmount := params.TotalAmount
					if vResp.TotalAmount > 0 {
						paidAmount = vResp.TotalAmount
					}

					// Update RAM order & customer state and clear active cart
					if existing := ramstore.Global.GetOrder(orderID); existing != nil {
						existing.PaymentStatus = "PAID"
						existing.OrderStatus = "CONFIRMED"
						existing.StatusUpdatedBy = "central"
						if paidAmount > 0 {
							existing.TotalAmount = paidAmount
						}
						ramstore.Global.UpsertOrderFromCentral(existing)
					}
					ramstore.Global.ClearCart(cleanPhone)
					_ = SetCustomerState(vendor.Username, cleanPhone, "ORDER_CONFIRMED")
					_ = SetCustomerPaymentStatus(vendor.Username, cleanPhone, "PAID")
					_ = SetCustomerCurrentOrderID(vendor.Username, cleanPhone, "")

					if MarkOrderPaidNotifiedOnce(orderID) {
						SendPaidOrderConfirmationWhatsApp(orderID, cleanPhone, params.CustomerName, params.DeliveryAddress, vResp.TxnID, paidAmount)
					}
					return
				}

				// 2. CANCELLED
				if vResp.Cancelled ||
					strings.EqualFold(strings.TrimSpace(vResp.PaymentStatus), "CANCELLED") ||
					strings.EqualFold(strings.TrimSpace(vResp.OrderStatus), "CANCELLED") {
					log.Printf("❌ [Payment Poller] Order #%s cancelled — stopping verification loop", orderID)
					if existing := ramstore.Global.GetOrder(orderID); existing != nil {
						existing.PaymentStatus = "CANCELLED"
						existing.OrderStatus = "CANCELLED"
						ramstore.Global.UpsertOrderFromCentral(existing)
					}
					return
				}
			}
		}
	}()
}

// SendPaidOrderConfirmationWhatsApp sends the Payment & Order Confirmation to the customer
// and a brief paid-order alert to vendor.NotificationPhone if configured.
func SendPaidOrderConfirmationWhatsApp(orderID, customerPhone, customerName, deliveryAddress, txnID string, totalAmount float64) {
	cleanPhone := FormatPhone(customerPhone)
	cleanOrdID := strings.TrimSpace(orderID)
	vendor := ramstore.Global.GetVendor()
	storeName := strings.TrimSpace(vendor.BusinessName)
	if storeName == "" {
		storeName = "Our Store"
	}

	// Fill missing order/customer fields from RAM if available
	if ord := ramstore.Global.GetOrder(cleanOrdID); ord != nil {
		if totalAmount <= 0 {
			totalAmount = ord.TotalAmount
		}
		if strings.TrimSpace(deliveryAddress) == "" {
			deliveryAddress = strings.TrimSpace(ord.DeliveryAddress)
		}
		if strings.TrimSpace(customerName) == "" {
			customerName = strings.TrimSpace(ord.CustomerName)
		}
		if cleanPhone == "" {
			cleanPhone = FormatPhone(ord.CustomerPhone)
		}
	}
	if cust := ramstore.Global.GetCustomer(cleanPhone); cust != nil {
		if strings.TrimSpace(deliveryAddress) == "" {
			deliveryAddress = strings.TrimSpace(cust.DeliveryAddress)
		}
		if strings.TrimSpace(customerName) == "" {
			customerName = strings.TrimSpace(cust.FullName)
		}
	}
	if strings.TrimSpace(deliveryAddress) == "" {
		deliveryAddress = "As provided"
	}
	if strings.TrimSpace(customerName) == "" {
		customerName = "Customer"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("✅ *Payment & Order Confirmed!*\n🏪 *%s*\n\n", storeName))
	sb.WriteString(fmt.Sprintf("• *Order ID:* #%s\n", cleanOrdID))
	sb.WriteString(fmt.Sprintf("• *Amount Paid:* ₹%.2f\n", totalAmount))
	if strings.TrimSpace(txnID) != "" {
		sb.WriteString(fmt.Sprintf("• *Transaction ID:* %s\n", strings.TrimSpace(txnID)))
	}
	sb.WriteString("• *Status:* CONFIRMED & PAID\n")
	sb.WriteString(fmt.Sprintf("• *Delivery Address:* %s\n", deliveryAddress))
	sb.WriteString("\nThank you for your payment! We are preparing your order now. 🎉🚚✨")

	confMsg := sb.String()
	waClient := GetAnyLoggedInClient()
	if cleanPhone != "" {
		if waClient != nil && waClient.IsLoggedIn() {
			if err := SendMessage(waClient, cleanPhone, confMsg); err != nil {
				log.Printf("⚠️ [Paid Order Confirmation] WhatsApp send failed for +%s (Order #%s): %v", cleanPhone, cleanOrdID, err)
			}
		}
		SaveCustomerChatAndPrune(vendor.Username, cleanPhone, "assistant", confMsg)
		PushToLiveChat(vendor.Username, cleanPhone, confMsg, "outgoing_ai")
	}

	// Send brief new-paid-order alert to vendor.notification_phone if configured
	notifPhone := FormatPhone(vendor.NotificationPhone)
	if notifPhone != "" && waClient != nil && waClient.IsLoggedIn() {
		var alert strings.Builder
		alert.WriteString(fmt.Sprintf("💰 *New Paid Order — #%s*\n\n", cleanOrdID))
		alert.WriteString(fmt.Sprintf("• *Customer:* %s (+%s)\n", customerName, cleanPhone))
		alert.WriteString(fmt.Sprintf("• *Amount Paid:* ₹%.2f\n", totalAmount))
		if strings.TrimSpace(txnID) != "" {
			alert.WriteString(fmt.Sprintf("• *Txn ID:* %s\n", strings.TrimSpace(txnID)))
		}
		alert.WriteString(fmt.Sprintf("• *Address:* %s", deliveryAddress))
		go func(dest, txt string) {
			_ = SendMessage(waClient, dest, txt)
		}(notifPhone, alert.String())
	}
}
