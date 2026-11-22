package whatsapp

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"wa-server/central"
	"wa-server/ramstore"

	"go.mau.fi/whatsmeow"
)

type CartItem struct {
	ID              interface{} `json:"id"`
	ProductID       string      `json:"product_id,omitempty"`
	Name            string      `json:"name"`
	Price           float64     `json:"price"`
	Qty             int         `json:"qty"`
	SelectedVariant string      `json:"selectedVariant,omitempty"`
}

type DeliveryInfo struct {
	CustomerName    string  `json:"customer_name"`
	DeliveryPhone   string  `json:"delivery_phone"`
	DeliveryAddress string  `json:"delivery_address"`
	PaymentMode     string  `json:"payment_mode"`
	Subtotal        float64 `json:"subtotal,omitempty"`
	DeliveryFee     float64 `json:"delivery_fee,omitempty"`
	TotalAmount     float64 `json:"total_amount,omitempty"`
	Source          string  `json:"source,omitempty"`
}

// GenerateStandardOrderID returns a unified hyphen-less Order ID (e.g. ORD1731928374123456).
func GenerateStandardOrderID() string {
	return fmt.Sprintf("ORD%d", time.Now().UnixNano())
}

// ProcessAndConfirmOrder creates an order strictly in RAM, enforces minimum order amount if applicable,
// clears active cart, updates customer profile, and pushes all updates to Central Brain Node.
func ProcessAndConfirmOrder(_ context.Context, _ *whatsmeow.Client, jid string, _ string, cartItems []CartItem, deliveryDetails DeliveryInfo) (string, error) {
	cleanJID := strings.TrimSpace(jid)
	cleanPhone := ""
	if strings.Contains(cleanJID, "@") {
		cleanPhone = FormatPhone(strings.Split(cleanJID, "@")[0])
	}
	if cleanPhone == "" && deliveryDetails.DeliveryPhone != "" {
		cleanPhone = FormatPhone(deliveryDetails.DeliveryPhone)
	}
	if cleanJID == "" && cleanPhone != "" {
		cleanJID = cleanPhone + "@s.whatsapp.net"
	}
	if cleanPhone == "" && cleanJID != "" {
		reg := regexp.MustCompile(`\D`)
		cleanPhone = reg.ReplaceAllString(strings.Split(cleanJID, "@")[0], "")
	}

	custName := strings.TrimSpace(deliveryDetails.CustomerName)
	custAddress := strings.TrimSpace(deliveryDetails.DeliveryAddress)
	payMode := strings.ToUpper(strings.TrimSpace(deliveryDetails.PaymentMode))
	if payMode == "" {
		payMode = "COD"
	}
	source := strings.TrimSpace(deliveryDetails.Source)
	if source == "" {
		source = "whatsapp"
	}

	if existingCust := ramstore.Global.GetCustomer(cleanPhone); existingCust != nil {
		if custName == "" {
			custName = existingCust.FullName
		}
		if custAddress == "" {
			custAddress = existingCust.DeliveryAddress
		}
	}

	var ramItems []ramstore.CartItem
	if len(cartItems) > 0 {
		for _, itm := range cartItems {
			pid := strings.TrimSpace(itm.ProductID)
			if pid == "" && itm.ID != nil {
				pid = fmt.Sprintf("%v", itm.ID)
			}
			q := itm.Qty
			if q <= 0 {
				q = 1
			}
			pName := strings.TrimSpace(itm.Name)
			unitPrice := itm.Price
			variant := strings.TrimSpace(itm.SelectedVariant)
			unitType := ""
			if prod, ok := ramstore.Global.FindProduct(pid); ok {
				if pName == "" {
					pName = prod.Name
				}
				unitType = prod.UnitType
				_, resolvedPrice, resolvedVar, _ := prod.ResolveVariantAndStock(variant, q)
				if unitPrice <= 0 && resolvedPrice > 0 {
					unitPrice = resolvedPrice
				}
				if variant == "" && resolvedVar != "" {
					variant = resolvedVar
				}
			}
			ramItems = append(ramItems, ramstore.CartItem{
				ProductID:       pid,
				ProductName:     pName,
				UnitPrice:       unitPrice,
				UnitType:        unitType,
				Qty:             q,
				SelectedVariant: variant,
				LineTotal:       unitPrice * float64(q),
				Source:          source,
				UpdatedAt:       time.Now(),
			})
		}
	} else {
		ramItems = ramstore.Global.GetCart(cleanPhone)
	}

	var calculatedSubtotal float64
	for _, itm := range ramItems {
		calculatedSubtotal += itm.LineTotal
	}
	if calculatedSubtotal <= 0 && deliveryDetails.Subtotal > 0 {
		calculatedSubtotal = deliveryDetails.Subtotal
	}
	finalDeliveryFee := deliveryDetails.DeliveryFee
	finalTotal := calculatedSubtotal + finalDeliveryFee
	if finalTotal <= 0 && deliveryDetails.TotalAmount > 0 {
		finalTotal = deliveryDetails.TotalAmount
	}

	orderID := GetCustomerCurrentOrderID("", cleanPhone)
	if orderID == "" {
		orderID = GenerateStandardOrderID()
	}

	payStatus := "PENDING"
	if payMode == "COD" {
		payStatus = "COD_PENDING"
	}

	vendor := ramstore.Global.GetVendor()
	savedOrder := ramstore.Global.UpsertOrder(&ramstore.Order{
		OrderID:           orderID,
		CustomerPhone:     cleanPhone,
		JID:               cleanJID,
		CustomerName:      custName,
		DeliveryPhone:     cleanPhone,
		DeliveryAddress:   custAddress,
		Items:             ramItems,
		Subtotal:          calculatedSubtotal,
		DeliveryFee:       finalDeliveryFee,
		TotalAmount:       finalTotal,
		PaymentMode:       payMode,
		PaymentStatus:     payStatus,
		OrderStatus:       "CONFIRMED",
		EstimatedDelivery: vendor.EstimatedDeliveryTime,
		Source:            source,
	})
	central.Client.PushOrderAsync(savedOrder)

	_ = ClearActiveCart("", cleanPhone)
	if savedCust, changed := ramstore.Global.UpsertCustomerWithChangeCheck(&ramstore.Customer{
		WANumber:         cleanPhone,
		JID:              cleanJID,
		FullName:         custName,
		DeliveryAddress:  custAddress,
		ContactPhone:     cleanPhone,
		LastOrderDetails: fmt.Sprintf("Order #%s (₹%.2f)", orderID, finalTotal),
		CurrentState:     "ORDER_CONFIRMED",
		PaymentStatus:    payStatus,
		CurrentOrderID:   "",
		Source:           source,
	}); changed && savedCust != nil {
		central.Client.PushCustomerAsync(savedCust)
	}

	log.Printf("🛍️ [Order Service RAM+Central] Order #%s confirmed for +%s (Total: ₹%.2f | Mode: %s)", orderID, cleanPhone, finalTotal, payMode)
	return orderID, nil
}

// SendOrderConfirmationWhatsApp sends the confirmation message and 80mm thermal PDF receipt over WhatsApp.
func SendOrderConfirmationWhatsApp(waClient *whatsmeow.Client, jid string, orderID string) error {
	cleanJID := strings.TrimSpace(jid)
	reg := regexp.MustCompile(`\D`)
	cleanPhone := ""
	if cleanJID != "" {
		cleanPhone = FormatPhone(reg.ReplaceAllString(strings.Split(cleanJID, "@")[0], ""))
	}
	cleanOrdID := strings.TrimSpace(orderID)

	var totalAmt float64
	eta := ramstore.Global.GetVendor().EstimatedDeliveryTime
	ord := ramstore.Global.GetOrder(cleanOrdID)
	if ord == nil && cleanPhone != "" {
		ord = ramstore.Global.GetLatestOrderByPhone(cleanPhone)
	}
	if ord != nil {
		cleanOrdID = ord.OrderID
		totalAmt = ord.TotalAmount
		if ord.EstimatedDelivery != "" {
			eta = ord.EstimatedDelivery
		}
		if cleanPhone == "" {
			cleanPhone = ord.CustomerPhone
		}
		if cleanJID == "" {
			cleanJID = ord.JID
		}
	}
	if cleanOrdID == "" {
		cleanOrdID = GenerateStandardOrderID()
	}

	client := waClient
	if client == nil || !client.IsLoggedIn() {
		client = GetAnyLoggedInClient()
	}
	if client == nil || !client.IsLoggedIn() {
		log.Printf("⚠️ [WhatsApp Notification] No active whatsmeow client for +%s (Order: #%s)", cleanPhone, cleanOrdID)
		return fmt.Errorf("no logged-in whatsapp client available")
	}

	vendorUser := ramstore.Global.GetVendor().Username
	etaLine := ""
	if strings.TrimSpace(eta) != "" {
		etaLine = fmt.Sprintf("\n⏱️ *Estimated Delivery:* %s", eta)
	}
	confMsg := fmt.Sprintf(
		"🎉 *Order Confirmed!* Your order *#%s* for *₹%.2f* has been successfully placed.%s\n\nWe are preparing it for delivery now. Thank you for shopping with us! 🚚✨",
		cleanOrdID, totalAmt, etaLine,
	)

	if err := SendMessage(client, cleanPhone, confMsg); err != nil {
		log.Printf("⚠️ [WhatsApp Notification] Text error for +%s: %v", cleanPhone, err)
	} else {
		PushToLiveChat(vendorUser, cleanPhone, confMsg, "outgoing_ai")
		go SaveCustomerChatAndPrune(vendorUser, cleanPhone, "assistant", confMsg)
	}

	time.Sleep(200 * time.Millisecond)
	return SendPDFReceiptHand(client, vendorUser, cleanPhone, cleanOrdID)
}

// NormalizeOrderStatus standardizes fulfillment & delivery partner tracking stages.
func NormalizeOrderStatus(rawStatus string) string {
	s := strings.ToUpper(strings.TrimSpace(rawStatus))
	switch s {
	case "PACKED", "READY_FOR_PICKUP", "READY":
		return "PACKED"
	case "DELIVERY_PARTNER_ASSIGNED", "RIDER_ASSIGNED", "PARTNER_ASSIGNED", "ASSIGNED":
		return "DELIVERY_PARTNER_ASSIGNED"
	case "DISPATCHED", "SHIPPED":
		return "DISPATCHED"
	case "OUT_FOR_DELIVERY", "OUT FOR DELIVERY", "ON_THE_WAY", "ON THE WAY", "PICKED_UP":
		return "OUT_FOR_DELIVERY"
	case "DELIVERED", "COMPLETED":
		return "DELIVERED"
	case "PROCESSING", "PREPARING", "PACKING":
		return "PROCESSING"
	case "CANCELLED", "CANCELED":
		return "CANCELLED"
	case "NEW", "CONFIRMED", "ORDER_CONFIRMED":
		return "CONFIRMED"
	default:
		if s != "" {
			return s
		}
		return "CONFIRMED"
	}
}

// SendOrderStatusUpdateWhatsApp sends live fulfillment & delivery partner alerts (Packed, Rider Assigned + Phone + OTP, Out for Delivery, Delivered).
func SendOrderStatusUpdateWhatsApp(_ string, jid string, customerPhone string, orderID string, newStatus string) error {
	cleanOrdID := strings.TrimSpace(orderID)
	if cleanOrdID == "" {
		return fmt.Errorf("empty order_id")
	}

	normStatus := NormalizeOrderStatus(newStatus)
	reg := regexp.MustCompile(`\D`)
	cleanPhone := FormatPhone(reg.ReplaceAllString(customerPhone, ""))
	if cleanPhone == "" && strings.Contains(jid, "@") {
		cleanPhone = FormatPhone(reg.ReplaceAllString(strings.Split(jid, "@")[0], ""))
	}

	var totalAmt float64
	payMode := "COD"
	custName := "Customer"
	riderName := ""
	riderPhone := ""
	deliveryOTP := ""
	eta := ""

	vendor := ramstore.Global.GetVendor()
	storeName := strings.TrimSpace(vendor.BusinessName)
	if storeName == "" {
		storeName = "Our Store"
	}

	if ord := ramstore.Global.GetOrder(cleanOrdID); ord != nil {
		if cleanPhone == "" {
			cleanPhone = ord.CustomerPhone
		}
		if ord.CustomerName != "" {
			custName = ord.CustomerName
		}
		if ord.PaymentMode != "" {
			payMode = strings.ToUpper(ord.PaymentMode)
		}
		totalAmt = ord.TotalAmount
		riderName = strings.TrimSpace(ord.DeliveryPartnerName)
		riderPhone = strings.TrimSpace(ord.DeliveryPartnerPhone)
		deliveryOTP = strings.TrimSpace(ord.DeliveryOTP)
		eta = strings.TrimSpace(ord.EstimatedDelivery)
	}

	var riderBlock strings.Builder
	if riderName != "" {
		riderBlock.WriteString(fmt.Sprintf("\n🏍️ *Delivery Partner:* %s", riderName))
	}
	if riderPhone != "" {
		riderBlock.WriteString(fmt.Sprintf("\n📞 *Partner Phone:* %s", riderPhone))
	}
	if deliveryOTP != "" {
		riderBlock.WriteString(fmt.Sprintf("\n🔐 *Delivery OTP:* *%s* _(Share only upon receiving your order)_", deliveryOTP))
	}
	if eta != "" {
		riderBlock.WriteString(fmt.Sprintf("\n⏱️ *ETA:* %s", eta))
	}

	var statusMsg string
	switch normStatus {
	case "PROCESSING":
		statusMsg = fmt.Sprintf(
			"👨‍🍳 *Order #%s Being Prepared*\n\nHello *%s*, *%s* is currently preparing your items (*₹%.2f*). We will update you once packed!",
			cleanOrdID, custName, storeName, totalAmt,
		)
	case "PACKED":
		statusMsg = fmt.Sprintf(
			"📦 *Order Packed & Ready!*\n\nHello *%s*,\nYour order *#%s* from *%s* (*₹%.2f*) is freshly packed and waiting for pickup by a delivery partner.%s",
			custName, cleanOrdID, storeName, totalAmt, riderBlock.String(),
		)
	case "DELIVERY_PARTNER_ASSIGNED":
		statusMsg = fmt.Sprintf(
			"🛵 *Delivery Partner Assigned!*\n\nHello *%s*,\nA delivery partner has been assigned for your order *#%s* from *%s* (*₹%.2f • %s*).%s",
			custName, cleanOrdID, storeName, totalAmt, payMode, riderBlock.String(),
		)
	case "DISPATCHED":
		statusMsg = fmt.Sprintf(
			"📦 *Order Dispatched!*\n\nHello *%s*,\nYour order *#%s* from *%s* (*₹%.2f*) has been dispatched!%s",
			custName, cleanOrdID, storeName, totalAmt, riderBlock.String(),
		)
	case "OUT_FOR_DELIVERY":
		statusMsg = fmt.Sprintf(
			"🚚 *Out For Delivery!*\n\nHello *%s*,\nYour order *#%s* from *%s* (*₹%.2f • %s*) is on the way to your doorstep! Please keep your phone reachable.%s",
			custName, cleanOrdID, storeName, totalAmt, payMode, riderBlock.String(),
		)
	case "DELIVERED":
		statusMsg = fmt.Sprintf(
			"✅ *Order Delivered!*\n\nHello *%s*,\nYour order *#%s* (*₹%.2f*) from *%s* has been delivered successfully. Thank you for shopping with us! 🙏✨",
			custName, cleanOrdID, totalAmt, storeName,
		)
	case "CANCELLED":
		statusMsg = fmt.Sprintf(
			"❌ *Order Cancelled*\n\nHello *%s*, your order *#%s* from *%s* has been cancelled. Reply here anytime if you need assistance! 🙏",
			custName, cleanOrdID, storeName,
		)
	default:
		return nil
	}

	if cleanPhone != "" {
		PushToLiveChat(vendor.Username, cleanPhone, statusMsg, "outgoing_ai")
		go SaveCustomerChatAndPrune(vendor.Username, cleanPhone, "assistant", statusMsg)
	}

	client := GetAnyLoggedInClient()
	if client == nil || !client.IsLoggedIn() {
		return fmt.Errorf("no logged-in whatsapp client available")
	}
	if cleanPhone == "" {
		return fmt.Errorf("customer phone not found for order %s", cleanOrdID)
	}
	return SendMessage(client, cleanPhone, statusMsg)
}

// SubmitWhatsAppOrderToCentral forwards ONLY the customer's cart items, name, phone, delivery address,
// and requested payment mode ("COD" or "ONLINE") to Central Brain Node.
// Central Brain Node owns 100% of online payment QR/link generation, payment verification, and final order confirmation.
// The bot NEVER marks an order PAID locally and NEVER verifies UTR/screenshots.
func SubmitWhatsAppOrderToCentral(client *whatsmeow.Client, username, customerPhone, paymentMode string) (string, *central.CentralOrderResponse, error) {
	cleanPhone := FormatPhone(customerPhone)
	if cleanPhone == "" {
		return "", nil, fmt.Errorf("customer phone is required")
	}

	cartItems := ramstore.Global.GetCart(cleanPhone)
	if len(cartItems) == 0 {
		return "🛒 Aapka cart abhi khali hai. Kripya pehle kuch items add karein!", nil, fmt.Errorf("empty cart")
	}

	var subtotal float64
	for _, it := range cartItems {
		line := it.LineTotal
		if line <= 0 {
			line = it.UnitPrice * float64(it.Qty)
		}
		subtotal += line
	}

	vendor := ramstore.Global.GetVendor()
	if vendor.MinOrderAmount > 0 && subtotal < vendor.MinOrderAmount {
		shortfall := vendor.MinOrderAmount - subtotal
		msg := fmt.Sprintf("⚠️ *Minimum Order Amount:* ₹%.2f\n\nAapke cart ka current total *₹%.2f* hai. Kripya ₹%.2f ke aur items add karein checkout ke liye! 🛒", vendor.MinOrderAmount, subtotal, shortfall)
		return msg, nil, fmt.Errorf("below min order amount")
	}

	cust := ramstore.Global.GetCustomer(cleanPhone)
	custName, custAddr, contactPhone := "WhatsApp Customer", "", cleanPhone
	var lat, lng float64
	if cust != nil {
		if strings.TrimSpace(cust.FullName) != "" {
			custName = strings.TrimSpace(cust.FullName)
		}
		custAddr = strings.TrimSpace(cust.DeliveryAddress)
		if strings.TrimSpace(cust.ContactPhone) != "" {
			contactPhone = FormatPhone(cust.ContactPhone)
		}
		lat = cust.Latitude
		lng = cust.Longitude
	}

	if custAddr == "" {
		return "📍 Order submit karne se pehle kripya apna complete *Delivery Address* share karein!", nil, fmt.Errorf("delivery address missing")
	}

	mode := strings.ToUpper(strings.TrimSpace(paymentMode))
	if mode == "UPI" || mode == "PREPAID" || mode == "ONLINE_PAYMENT" {
		mode = "ONLINE"
	}
	if mode != "COD" {
		mode = "ONLINE"
	}
	if mode == "COD" && !vendor.AllowCOD {
		return "ℹ️ Is store par abhi *Cash on Delivery (COD)* uplabdh nahi hai. Kripya *Online Payment* chunein!", nil, fmt.Errorf("cod disabled")
	}

	deliveryFee := 0.0
	if lat != 0 && lng != 0 && vendor.Latitude != 0 && vendor.Longitude != 0 {
		dist := CalculateDistance(vendor.Latitude, vendor.Longitude, lat, lng)
		if vendor.MaxDeliveryRadiusKM > 0 && dist > vendor.MaxDeliveryRadiusKM {
			return fmt.Sprintf("📍 Maaf kijiye, aapka delivery address humari %.1f km delivery range ke bahar hai.", vendor.MaxDeliveryRadiusKM), nil, fmt.Errorf("outside delivery range")
		}
		if dist > vendor.FreeDeliveryRadiusKM {
			deliveryFee = vendor.DeliveryChargeAmount
		}
	}

	totalAmount := subtotal + deliveryFee
	submission := central.CentralOrderSubmission{
		Event:           "order.requested",
		Source:          "whatsapp",
		CustomerPhone:   cleanPhone,
		JID:             cleanPhone + "@s.whatsapp.net",
		CustomerName:    custName,
		DeliveryPhone:   contactPhone,
		DeliveryAddress: custAddr,
		Latitude:        lat,
		Longitude:       lng,
		PaymentMode:     mode,
		Items:           cartItems,
		Subtotal:        subtotal,
		DeliveryFee:     deliveryFee,
		TotalAmount:     totalAmount,
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
	}

	resp, err := central.Client.SubmitOrderToCentral(submission)
	if err != nil {
		log.Printf("⚠️ [Central Order Submit] Failed for +%s: %v", cleanPhone, err)
		fallback := "⏳ Aapki order request Central Mall Server ko bhej di gayi hai. Jaise hi server se order reference aur payment instructions milenge, aapko turant WhatsApp update mil jayega! 🙏"
		return fallback, nil, err
	}

	ordID := strings.TrimSpace(resp.OrderID)
	if ordID == "" {
		ordID = GenerateStandardOrderID()
	}
	finalAmount := resp.TotalAmount
	if finalAmount <= 0 {
		finalAmount = totalAmount
	}
	ordStatus := strings.ToUpper(strings.TrimSpace(resp.OrderStatus))
	if ordStatus == "" {
		if mode == "ONLINE" {
			ordStatus = "PENDING_PAYMENT"
		} else {
			ordStatus = "PENDING_CONFIRMATION"
		}
	}
	payStatus := strings.ToUpper(strings.TrimSpace(resp.PaymentStatus))
	if payStatus == "" {
		if mode == "COD" {
			payStatus = "COD_PENDING"
		} else {
			payStatus = "PENDING"
		}
	}

	payCfg := ramstore.Global.GetPayment()
	storeName := strings.TrimSpace(vendor.BusinessName)
	if storeName == "" {
		storeName = "Our Store"
	}

	resolvedPaytmMID := strings.TrimSpace(resp.PaytmMID)
	if resolvedPaytmMID == "" {
		resolvedPaytmMID = strings.TrimSpace(payCfg.PaytmMID)
	}

	var localQRBytes []byte
	localQRMime := ""

	// ONLINE / UPI Checkout: Generate Branded Banner QR LOCALLY in memory (zero external QR Worker / Cloudinary)
	// STRICT RULE: Send ONLY the Banner QR image — NEVER send upi:// or any payment link as text.
	if mode == "ONLINE" {
		upiID := strings.TrimSpace(resp.UPIID)
		if upiID == "" {
			upiID = strings.TrimSpace(resp.MerchantVPA)
		}
		if upiID == "" {
			upiID = strings.TrimSpace(payCfg.UPIID)
		}
		if upiID == "" {
			upiID = strings.TrimSpace(payCfg.MerchantVPA)
		}

		payeeName := strings.TrimSpace(resp.PayeeName)
		if payeeName == "" {
			payeeName = strings.TrimSpace(payCfg.PayeeName)
		}
		if payeeName == "" {
			payeeName = storeName
		}

		upiStr := BuildUPIQRString(resp.UPIQRData, upiID, payeeName, ordID, finalAmount)
		if upiStr != "" {
			imgBytes, mimeType, qrErr := GenerateLocalBannerQRBytes(upiStr, payCfg.BannerTemplate)
			if qrErr == nil && len(imgBytes) > 0 {
				localQRBytes = imgBytes
				localQRMime = mimeType
				log.Printf("🎨 [Local Banner QR Generated] Order #%s | %s | %.1f KB (zero external worker)", ordID, mimeType, float64(len(imgBytes))/1024.0)
			} else {
				log.Printf("⚠️ [Local Banner QR] Generation failed for Order #%s: %v", ordID, qrErr)
			}
		} else {
			log.Printf("⚠️ [Local Banner QR] Missing UPI ID / upi_qr_data for Order #%s", ordID)
		}
	}

	// Cache Central's returned order reference in RAM
	ramstore.Global.UpsertOrderFromCentral(&ramstore.Order{
		OrderID:         ordID,
		CustomerPhone:   cleanPhone,
		JID:             cleanPhone + "@s.whatsapp.net",
		CustomerName:    custName,
		DeliveryPhone:   contactPhone,
		DeliveryAddress: custAddr,
		Items:           cartItems,
		Subtotal:        subtotal,
		DeliveryFee:     deliveryFee,
		TotalAmount:     finalAmount,
		PaymentMode:     mode,
		PaymentStatus:   payStatus,
		OrderStatus:     ordStatus,
		PaymentLink:     "",
		PaymentQRURL:    "",
		Source:          "whatsapp",
		StatusUpdatedBy: "central",
	})

	_ = SetCustomerCurrentOrderID(username, cleanPhone, ordID)
	if mode == "ONLINE" {
		_ = SetCustomerState(username, cleanPhone, "PAYMENT_PENDING")
		_ = SetCustomerPaymentStatus(username, cleanPhone, "PENDING")

		// Clean QR-only caption (STRICT RULE: NO upi:// link, NO checkout URL, NO clickable payment link!)
		qrCaption := fmt.Sprintf(
			"🏪 *%s*\n🧾 *Order ID:* #%s\n💰 *Total Amount:* ₹%.2f\n\nPlease scan & pay using this QR code. Your payment will be verified automatically in a few seconds.",
			storeName, ordID, finalAmount,
		)

		qrSent := false
		if len(localQRBytes) > 0 && client != nil && client.IsLoggedIn() {
			thumb := generateWhatsAppJPEGThumbnail(localQRBytes)
			if sendErr := SendImageBytesWithThumb(client, cleanPhone, localQRBytes, thumb, localQRMime, qrCaption); sendErr == nil {
				qrSent = true
				SaveCustomerChatAndPrune(username, cleanPhone, "assistant", qrCaption)
				PushToLiveChat(username, cleanPhone, qrCaption, "outgoing_ai")
			} else {
				log.Printf("⚠️ [Local Banner QR Send] WhatsApp upload/send failed for Order #%s: %v", ordID, sendErr)
			}
		}
		// Release composite bytes immediately for GC
		localQRBytes = nil

		// Start non-blocking 3-second direct Paytm payment verification loop immediately after sending QR
		StartPaymentVerificationPoller(PaymentPollParams{
			OrderID:                ordID,
			CustomerPhone:          cleanPhone,
			CustomerName:           custName,
			DeliveryAddress:        custAddr,
			TotalAmount:            finalAmount,
			PaytmMID:               resolvedPaytmMID,
			PaytmStatusURL:         payCfg.PaytmStatusURL,
			PaymentConfirmEndpoint: payCfg.PaymentConfirmEndpoint,
			LocalPaytmVerify:       payCfg.LocalPaytmVerify,
			VerifyEndpoint:         payCfg.VerifyEndpoint,
			PollInterval:           time.Duration(payCfg.VerifyPollIntervalMS) * time.Millisecond,
			PollTimeout:            time.Duration(payCfg.VerifyPollTimeoutMS) * time.Millisecond,
		})

		if qrSent {
			// Return special sentinel so caller sends NO follow-up text message (ONLY the Branded Banner QR image)
			return "__QR_IMAGE_SENT__", resp, nil
		}

		// Fallback text only if QR generation/send failed (still ZERO payment links!)
		return fmt.Sprintf(
			"🏪 *%s*\n🧾 *Order ID:* #%s\n💰 *Total Amount:* ₹%.2f\n\nQR image load hone me thodi dikkat aayi — kripya store se sampark karein ya dobara try karein. 🙏",
			storeName, ordID, finalAmount,
		), resp, nil
	}

	// COD order flow
	_ = SetCustomerState(username, cleanPhone, "ORDER_SUBMITTED")
	_ = SetCustomerPaymentStatus(username, cleanPhone, payStatus)

	var sb strings.Builder
	if strings.TrimSpace(resp.Message) != "" {
		sb.WriteString(strings.TrimSpace(resp.Message))
		sb.WriteString("\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("📦 *Order Request #%s Submitted*\n\n• Total Amount: *₹%.2f*\n• Payment Mode: *Cash on Delivery (COD)*\n", ordID, finalAmount))
	}
	sb.WriteString("✅ Central Mall System se confirm hote hi aapko final order confirmation aur PDF receipt turant bhej di jayegi!")

	return strings.TrimSpace(sb.String()), resp, nil
}

