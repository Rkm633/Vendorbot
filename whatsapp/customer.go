package whatsapp

import (
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"time"

	"wa-server/central"
	"wa-server/ramstore"
)

// CustomerProfile represents customer profile details cached in RAM and synced to Central Brain Node.
type CustomerProfile struct {
	Username         string    `json:"username"`
	WANumber         string    `json:"wa_number"`
	JID              string    `json:"jid"`
	FullName         string    `json:"full_name"`
	DeliveryAddress  string    `json:"delivery_address"`
	ContactPhone     string    `json:"contact_phone"`
	Latitude         float64   `json:"latitude"`
	Longitude        float64   `json:"longitude"`
	LastOrderDetails string    `json:"last_order_details"`
	CurrentState     string    `json:"current_state"`
	PaymentStatus    string    `json:"payment_status"`
	CurrentOrderID   string    `json:"current_order_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func fromRAMCustomer(c *ramstore.Customer) *CustomerProfile {
	if c == nil {
		return nil
	}
	return &CustomerProfile{
		Username:         ramstore.Global.GetVendor().Username,
		WANumber:         c.WANumber,
		JID:              c.JID,
		FullName:         c.FullName,
		DeliveryAddress:  c.DeliveryAddress,
		ContactPhone:     c.ContactPhone,
		Latitude:         c.Latitude,
		Longitude:        c.Longitude,
		LastOrderDetails: c.LastOrderDetails,
		CurrentState:     c.CurrentState,
		PaymentStatus:    c.PaymentStatus,
		CurrentOrderID:   c.CurrentOrderID,
		CreatedAt:        c.CreatedAt,
		UpdatedAt:        c.UpdatedAt,
	}
}

// GetCustomerProfile retrieves customer profile from 0ms RAM store.
func GetCustomerProfile(_, waNumber string) (*CustomerProfile, error) {
	cleanPhone := FormatPhone(waNumber)
	if cleanPhone == "" {
		return nil, fmt.Errorf("empty customer phone number")
	}
	return fromRAMCustomer(ramstore.Global.GetCustomer(cleanPhone)), nil
}

// GetCustomerState retrieves customer's current state from RAM (default: "chatting").
func GetCustomerState(_, customerPhone string) string {
	c := ramstore.Global.GetCustomer(FormatPhone(customerPhone))
	if c != nil && c.CurrentState != "" {
		return c.CurrentState
	}
	return "chatting"
}

// SetCustomerState updates customer's current_state strictly in bot RAM.
func SetCustomerState(_, customerPhone, state string) error {
	cleanPhone := FormatPhone(customerPhone)
	if cleanPhone == "" {
		return fmt.Errorf("empty customer phone")
	}
	ramstore.Global.SetCustomerState(cleanPhone, state)
	return nil
}

// GetCustomerPaymentStatus retrieves customer's current payment_status from RAM.
func GetCustomerPaymentStatus(_, customerPhone string) string {
	c := ramstore.Global.GetCustomer(FormatPhone(customerPhone))
	if c != nil && c.PaymentStatus != "" {
		return c.PaymentStatus
	}
	return "UNPAID"
}

// SetCustomerPaymentStatus updates payment_status strictly in bot RAM.
func SetCustomerPaymentStatus(_, customerPhone, status string) error {
	cleanPhone := FormatPhone(customerPhone)
	if cleanPhone == "" {
		return fmt.Errorf("empty customer phone")
	}
	ramstore.Global.SetCustomerPaymentStatus(cleanPhone, status)
	return nil
}

// GetCustomerCurrentOrderID retrieves the active pending order ID from RAM.
func GetCustomerCurrentOrderID(_, customerPhone string) string {
	c := ramstore.Global.GetCustomer(FormatPhone(customerPhone))
	if c != nil && c.CurrentOrderID != "" {
		return c.CurrentOrderID
	}
	return ""
}

// SetCustomerCurrentOrderID updates current_order_id strictly in bot RAM.
func SetCustomerCurrentOrderID(_, customerPhone, orderID string) error {
	cleanPhone := FormatPhone(customerPhone)
	if cleanPhone == "" {
		return fmt.Errorf("empty customer phone")
	}
	ramstore.Global.SetCustomerCurrentOrderID(cleanPhone, orderID)
	return nil
}

// ClearActiveCart clears the customer's active cart in RAM and pushes the empty cart to Central Brain Node.
func ClearActiveCart(_, customerPhone string) error {
	cleanPhone := FormatPhone(customerPhone)
	if cleanPhone == "" {
		return nil
	}
	hadItems := len(ramstore.Global.GetCart(cleanPhone)) > 0
	ramstore.Global.ClearCart(cleanPhone)
	if hadItems {
		central.Client.PushCartAsync(cleanPhone, cleanPhone+"@s.whatsapp.net", []ramstore.CartItem{}, 0)
		log.Printf("🛒 [Active Cart Cleared in RAM & Synced]: Customer +%s", cleanPhone)
	}
	return nil
}

// UpsertCustomerProfile saves customer profile into RAM (0ms) and pushes async to Central Brain Node
// ONLY if the customer is brand-new or their name, phone, address, or GPS coordinates actually changed.
func UpsertCustomerProfile(_ string, profile *CustomerProfile) error {
	if profile == nil {
		return nil
	}
	cleanPhone := FormatPhone(profile.WANumber)
	if cleanPhone == "" {
		return fmt.Errorf("empty customer phone number")
	}

	saved, changed := ramstore.Global.UpsertCustomerWithChangeCheck(&ramstore.Customer{
		WANumber:         cleanPhone,
		JID:              profile.JID,
		FullName:         profile.FullName,
		DeliveryAddress:  profile.DeliveryAddress,
		ContactPhone:     profile.ContactPhone,
		Latitude:         profile.Latitude,
		Longitude:        profile.Longitude,
		LastOrderDetails: profile.LastOrderDetails,
		CurrentState:     profile.CurrentState,
		PaymentStatus:    profile.PaymentStatus,
		CurrentOrderID:   profile.CurrentOrderID,
		Source:           "whatsapp",
	})
	if changed && saved != nil {
		central.Client.PushCustomerAsync(saved)
	}
	return nil
}

// UpdateCustomerCoordinates saves GPS coordinates & reverse-geocoded address in RAM + Central Brain Node.
func UpdateCustomerCoordinates(username, waNumber string, lat, lng float64, address string) error {
	cleanPhone := FormatPhone(waNumber)
	if cleanPhone == "" {
		return fmt.Errorf("empty customer phone number")
	}
	cp, _ := GetCustomerProfile(username, cleanPhone)
	if cp == nil {
		cp = &CustomerProfile{WANumber: cleanPhone}
	}
	cp.Latitude = lat
	cp.Longitude = lng
	if address != "" && (cp.DeliveryAddress == "" || strings.HasPrefix(cp.DeliveryAddress, "Location / Area:") || !strings.Contains(cp.DeliveryAddress, address)) {
		cp.DeliveryAddress = address
	}
	return UpsertCustomerProfile(username, cp)
}

// CalculateDistance computes great-circle distance in kilometers via Haversine formula.
func CalculateDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371.0
	dLat := (lat2 - lat1) * (math.Pi / 180.0)
	dLon := (lon2 - lon1) * (math.Pi / 180.0)
	rLat1 := lat1 * (math.Pi / 180.0)
	rLat2 := lat2 * (math.Pi / 180.0)
	a := math.Sin(dLat/2.0)*math.Sin(dLat/2.0) + math.Cos(rLat1)*math.Cos(rLat2)*math.Sin(dLon/2.0)*math.Sin(dLon/2.0)
	c := 2.0 * math.Atan2(math.Sqrt(a), math.Sqrt(1.0-a))
	return R * c
}

// GetBusinessLocation returns vendor coordinates and delivery logistics from RAM.
func GetBusinessLocation(_ string) (lat float64, lng float64, maxRadiusKM float64, freeRadiusKM float64, deliveryCharge float64, requireLocation bool) {
	v := ramstore.Global.GetVendor()
	lat = v.Latitude
	lng = v.Longitude
	if lat == 0 && lng == 0 {
		lat = 26.8467
		lng = 80.9462
	}
	maxRadiusKM = v.MaxDeliveryRadiusKM
	if maxRadiusKM <= 0 {
		maxRadiusKM = 15.0
	}
	freeRadiusKM = v.FreeDeliveryRadiusKM
	deliveryCharge = v.DeliveryChargeAmount
	requireLocation = v.RequireCustomerLocation
	return
}

// ExtractAndPersistCustomerDetails heuristically extracts contact phone, address, and name from incoming messages.
func ExtractAndPersistCustomerDetails(username, phone, text string) {
	cleanPhone := FormatPhone(phone)
	trimmed := strings.TrimSpace(text)
	if cleanPhone == "" || trimmed == "" {
		return
	}
	lower := strings.ToLower(trimmed)
	if lower == "hi" || lower == "hello" || lower == "namaste" || lower == "yes" || lower == "ha" || lower == "haan" || lower == "ok" || lower == "done" {
		return
	}

	cp, _ := GetCustomerProfile(username, cleanPhone)
	if cp == nil {
		cp = &CustomerProfile{WANumber: cleanPhone}
	}

	updated := false
	phoneRegex := regexp.MustCompile(`\b[6-9]\d{9}\b`)
	if match := phoneRegex.FindString(trimmed); match != "" && match != cleanPhone && cp.ContactPhone != match {
		cp.ContactPhone = match
		updated = true
	}

	if strings.Contains(lower, "address") || strings.Contains(lower, "near ") || strings.Contains(lower, "road") ||
		strings.Contains(lower, "sector") || strings.Contains(lower, "gali") || strings.Contains(lower, "colony") ||
		strings.Contains(lower, "house") || strings.Contains(lower, "flat") || strings.Contains(lower, "nagar") {
		cleanAddr := trimmed
		if idx := strings.Index(lower, "address:"); idx != -1 {
			cleanAddr = strings.TrimSpace(trimmed[idx+8:])
		} else if idx := strings.Index(lower, "address is:"); idx != -1 {
			cleanAddr = strings.TrimSpace(trimmed[idx+11:])
		} else if idx := strings.Index(lower, "mera address hai"); idx != -1 {
			cleanAddr = strings.TrimSpace(trimmed[idx+16:])
		}
		if len(cleanAddr) >= 8 && cp.DeliveryAddress != cleanAddr {
			cp.DeliveryAddress = cleanAddr
			updated = true
		}
	}

	if strings.Contains(lower, "name:") || strings.Contains(lower, "naam:") || strings.Contains(lower, "mera naam") {
		cleanName := ""
		if idx := strings.Index(lower, "name:"); idx != -1 {
			cleanName = strings.TrimSpace(trimmed[idx+5:])
		} else if idx := strings.Index(lower, "naam:"); idx != -1 {
			cleanName = strings.TrimSpace(trimmed[idx+5:])
		} else if idx := strings.Index(lower, "mera naam"); idx != -1 {
			cleanName = strings.TrimSpace(trimmed[idx+9:])
		}
		if len(cleanName) >= 2 && len(cleanName) <= 50 && cp.FullName != cleanName {
			cp.FullName = cleanName
			updated = true
		}
	}

	if updated {
		_ = UpsertCustomerProfile(username, cp)
	}
}

type CartItemDetail struct {
	ProductID       string  `json:"product_id"`
	ProductName     string  `json:"product_name"`
	UnitPrice       float64 `json:"unit_price"`
	UnitType        string  `json:"unit_type,omitempty"`
	SelectedVariant string  `json:"selected_variant,omitempty"`
	Qty             int     `json:"qty"`
	Quantity        int     `json:"quantity,omitempty"`
	LineTotal       float64 `json:"line_total"`
}

type CartCalculationResult struct {
	Username      string           `json:"username"`
	CustomerPhone string           `json:"customer_phone"`
	TotalAmount   float64          `json:"total_amount"`
	PaytmMid      string           `json:"paytm_mid"`
	PaytmVpa      string           `json:"paytm_vpa"`
	PayeeName     string           `json:"payee_name"`
	OrderID       string           `json:"order_id"`
	Items         []CartItemDetail `json:"items"`
	ItemCount     int              `json:"item_count"`
}

// UpdateCustomerProfileFromData saves AI-extracted profile data into RAM + Central Brain Node.
func UpdateCustomerProfileFromData(username, customerPhone string, profile *AIProfileData) error {
	if profile == nil {
		return nil
	}
	cleanPhone := FormatPhone(customerPhone)
	if cleanPhone == "" {
		return fmt.Errorf("empty customer phone number")
	}
	name := strings.TrimSpace(profile.Name)
	addr := strings.TrimSpace(profile.Address)
	contactNum := strings.TrimSpace(profile.ContactNumber)
	if name == "" && addr == "" && contactNum == "" {
		return nil
	}

	cp, _ := GetCustomerProfile(username, cleanPhone)
	if cp == nil {
		cp = &CustomerProfile{WANumber: cleanPhone}
	}
	if name != "" {
		cp.FullName = name
	}
	if addr != "" {
		cp.DeliveryAddress = addr
	}
	if contactNum != "" {
		cp.ContactPhone = contactNum
	}
	return UpsertCustomerProfile(username, cp)
}

// AddItemsToActiveCart updates the customer's cart in RAM (0ms) with variant & stock validation and syncs to Central Brain Node.
func AddItemsToActiveCart(username, customerPhone string, cartItems []AICartItem) error {
	_, err := AddItemsToActiveCartWithStockCheck(username, customerPhone, cartItems)
	return err
}

// AddItemsToActiveCartWithStockCheck validates stock & variants and returns any stock rejection reasons.
func AddItemsToActiveCartWithStockCheck(username, customerPhone string, cartItems []AICartItem) ([]string, error) {
	if len(cartItems) == 0 {
		return nil, nil
	}
	cleanPhone := FormatPhone(customerPhone)
	if cleanPhone == "" {
		return nil, fmt.Errorf("empty customer phone number")
	}

	if cp, _ := GetCustomerProfile(username, cleanPhone); cp != nil {
		if cp.CurrentState == "ORDER_CONFIRMED" || cp.PaymentStatus == "PAID" || cp.PaymentStatus == "CANCELLED" {
			_ = ClearActiveCart(username, cleanPhone)
			_ = SetCustomerState(username, cleanPhone, "chatting")
			_ = SetCustomerPaymentStatus(username, cleanPhone, "UNPAID")
		}
	}

	incoming := make([]ramstore.CartItem, 0, len(cartItems))
	for _, it := range cartItems {
		pid := strings.TrimSpace(it.ProductID)
		if pid == "" {
			continue
		}
		qty := it.Qty
		if qty <= 0 {
			qty = 1
		}
		incoming = append(incoming, ramstore.CartItem{
			ProductID:       pid,
			SelectedVariant: strings.TrimSpace(it.Variant),
			Qty:             qty,
			Source:          "whatsapp",
		})
	}

	updatedItems, total, rejected := ramstore.Global.UpsertCartItemsValidated(cleanPhone, incoming, false, true)
	central.Client.PushCartAsync(cleanPhone, cleanPhone+"@s.whatsapp.net", updatedItems, total)
	log.Printf("🛒 [Active Cart RAM+Central Sync]: +%s | %d items | Total: ₹%.2f | Rejected: %d", cleanPhone, len(updatedItems), total, len(rejected))
	return rejected, nil
}

// FetchRealBusinessPaymentCredentials fetches Paytm/UPI credentials directly from RAM.
func FetchRealBusinessPaymentCredentials(_, _ string) (mid string, vpa string, payeeName string, err error) {
	pCfg := ramstore.Global.GetPayment()
	if strings.TrimSpace(pCfg.PaytmMID) != "" && strings.TrimSpace(pCfg.MerchantVPA) != "" {
		return strings.TrimSpace(pCfg.PaytmMID), strings.TrimSpace(pCfg.MerchantVPA), strings.TrimSpace(pCfg.PayeeName), nil
	}
	return "", "", "", fmt.Errorf("merchant Paytm MID and VPA are not configured in vendor payment settings")
}

// CalculateCartTotalAndFetchCredentials computes exact cart total from RAM products + fetches real payment credentials.
func CalculateCartTotalAndFetchCredentials(username, customerPhone string) (*CartCalculationResult, error) {
	cleanPhone := FormatPhone(customerPhone)
	vendor := ramstore.Global.GetVendor()
	res := &CartCalculationResult{
		Username:      vendor.Username,
		CustomerPhone: cleanPhone,
		Items:         []CartItemDetail{},
	}

	cartItems := ramstore.Global.GetCart(cleanPhone)
	var total float64
	var count int
	for _, it := range cartItems {
		unitPrice := it.UnitPrice
		pName := it.ProductName
		unitType := it.UnitType
		variant := it.SelectedVariant
		if prod, ok := ramstore.Global.FindProduct(it.ProductID); ok {
			if pName == "" {
				pName = prod.Name
			}
			if unitType == "" {
				unitType = prod.UnitType
			}
			if unitPrice <= 0 {
				_, resolvedPrice, resolvedVar, _ := prod.ResolveVariantAndStock(variant, it.Qty)
				if resolvedPrice > 0 {
					unitPrice = resolvedPrice
				} else {
					unitPrice = prod.EffectivePrice()
				}
				if variant == "" && resolvedVar != "" {
					variant = resolvedVar
				}
			}
		}
		lineTotal := unitPrice * float64(it.Qty)
		total += lineTotal
		count += it.Qty
		res.Items = append(res.Items, CartItemDetail{
			ProductID:       it.ProductID,
			ProductName:     pName,
			UnitPrice:       unitPrice,
			UnitType:        unitType,
			SelectedVariant: variant,
			Qty:             it.Qty,
			Quantity:        it.Qty,
			LineTotal:       lineTotal,
		})
	}

	res.TotalAmount = total
	res.ItemCount = count

	pMid, pVpa, bName, _ := FetchRealBusinessPaymentCredentials(username, cleanPhone)
	res.PaytmMid = pMid
	res.PaytmVpa = pVpa
	res.PayeeName = bName

	ordID := GetCustomerCurrentOrderID(username, cleanPhone)
	if ordID == "" {
		ordID = GenerateStandardOrderID()
		_ = SetCustomerCurrentOrderID(username, cleanPhone, ordID)
	}
	res.OrderID = ordID

	// Keep an in-progress order snapshot in RAM for QR/PDF workflows
	if len(res.Items) > 0 && res.TotalAmount > 0 {
		cust := ramstore.Global.GetCustomer(cleanPhone)
		cName := ""
		cAddr := ""
		if cust != nil {
			cName = cust.FullName
			cAddr = cust.DeliveryAddress
		}
		ramItems := make([]ramstore.CartItem, 0, len(res.Items))
		for _, it := range res.Items {
			ramItems = append(ramItems, ramstore.CartItem{
				ProductID:       it.ProductID,
				ProductName:     it.ProductName,
				UnitPrice:       it.UnitPrice,
				UnitType:        it.UnitType,
				SelectedVariant: it.SelectedVariant,
				Qty:             it.Qty,
				LineTotal:       it.LineTotal,
				Source:          "whatsapp",
			})
		}
		ramstore.Global.UpsertOrder(&ramstore.Order{
			OrderID:         ordID,
			CustomerPhone:   cleanPhone,
			JID:             cleanPhone + "@s.whatsapp.net",
			CustomerName:    cName,
			DeliveryPhone:   cleanPhone,
			DeliveryAddress: cAddr,
			Items:           ramItems,
			Subtotal:        res.TotalAmount,
			TotalAmount:     res.TotalAmount,
			PaymentMode:     "ONLINE",
			PaymentStatus:   "PENDING",
			OrderStatus:     "IN_PROGRESS",
			Source:          "whatsapp",
		})
	}

	return res, nil
}

// ResolveOrderAmountAndID resolves order amount & ID from RAM.
func ResolveOrderAmountAndID(username, senderPhone, _, _ string) (string, string) {
	cleanPhone := FormatPhone(senderPhone)
	calcRes, err := CalculateCartTotalAndFetchCredentials(username, cleanPhone)
	if err == nil && calcRes != nil && calcRes.TotalAmount > 0 {
		return fmt.Sprintf("%.2f", calcRes.TotalAmount), calcRes.OrderID
	}
	if ord := ramstore.Global.GetLatestOrderByPhone(cleanPhone); ord != nil && ord.TotalAmount > 0 {
		return fmt.Sprintf("%.2f", ord.TotalAmount), ord.OrderID
	}
	ordID := GetCustomerCurrentOrderID(username, cleanPhone)
	if ordID == "" {
		ordID = GenerateStandardOrderID()
		_ = SetCustomerCurrentOrderID(username, cleanPhone, ordID)
	}
	return "0.00", ordID
}

