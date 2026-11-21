package whatsapp

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"wa-server/ramstore"

	"github.com/jung-kurt/gofpdf"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// GenerateThermalReceiptPDF builds a dynamic 80mm thermal receipt PDF strictly from RAM (zero DB, zero disk I/O).
func GenerateThermalReceiptPDF(orderID string) (*bytes.Buffer, error) {
	cleanOrderID := strings.TrimSpace(orderID)
	ord := ramstore.Global.GetOrder(cleanOrderID)
	if ord == nil {
		ord = ramstore.Global.GetLatestOrderByPhone("")
	}

	vendor := ramstore.Global.GetVendor()
	bName := strings.TrimSpace(vendor.BusinessName)
	if bName == "" {
		bName = "Vendor Store"
	}
	bPhone := FormatPhone(vendor.NotificationPhone)

	var custPhone, custName, custAddress, payMode, payStatus, createdAtStr string
	var subtotalAmt, deliveryFee, totalAmt float64
	var itemList []ramstore.CartItem

	if ord != nil {
		cleanOrderID = ord.OrderID
		custPhone = ord.CustomerPhone
		custName = ord.CustomerName
		custAddress = ord.DeliveryAddress
		payMode = ord.PaymentMode
		payStatus = ord.PaymentStatus
		subtotalAmt = ord.Subtotal
		deliveryFee = ord.DeliveryFee
		totalAmt = ord.TotalAmount
		itemList = ord.Items
		if !ord.CreatedAt.IsZero() {
			createdAtStr = ord.CreatedAt.Format("02 Jan 2006 15:04")
		}
	}

	if cleanOrderID == "" {
		cleanOrderID = GenerateStandardOrderID()
	}
	if createdAtStr == "" {
		createdAtStr = time.Now().Format("02 Jan 2006 15:04")
	}

	if len(itemList) == 0 {
		itemList = append(itemList, ramstore.CartItem{
			ProductName: "Order Items Package",
			Qty:         1,
			LineTotal:   totalAmt,
		})
	}
	if subtotalAmt <= 0 {
		for _, it := range itemList {
			subtotalAmt += it.LineTotal
		}
	}
	if totalAmt <= 0 {
		totalAmt = subtotalAmt + deliveryFee
	}

	pageHeight := 98.0 + float64(len(itemList))*8.5
	if pageHeight < 115.0 {
		pageHeight = 115.0
	}

	pdf := gofpdf.NewCustom(&gofpdf.InitType{
		OrientationStr: "P",
		UnitStr:        "mm",
		Size:           gofpdf.SizeType{Wd: 80, Ht: pageHeight},
	})

	pdf.SetMargins(4, 4, 4)
	pdf.SetAutoPageBreak(false, 0)
	pdf.AddPage()

	pdf.SetFont("Arial", "B", 12)
	pdf.CellFormat(72, 6, bName, "", 1, "C", false, 0, "")

	if bPhone != "" {
		pdf.SetFont("Arial", "", 8)
		pdf.CellFormat(72, 4, "Ph: +"+bPhone, "", 1, "C", false, 0, "")
	}

	pdf.SetFont("Arial", "", 8)
	pdf.Ln(2)
	pdf.CellFormat(72, 3, "--------------------------------------------------", "", 1, "C", false, 0, "")

	pdf.SetFont("Arial", "B", 9)
	pdf.CellFormat(72, 4, "OFFICIAL ORDER RECEIPT", "", 1, "C", false, 0, "")
	pdf.SetFont("Arial", "", 8)
	pdf.CellFormat(72, 4, fmt.Sprintf("Order ID: #%s", cleanOrderID), "", 1, "L", false, 0, "")
	pdf.CellFormat(72, 4, fmt.Sprintf("Date: %s", createdAtStr), "", 1, "L", false, 0, "")

	if custName != "" && custName != "Customer" {
		pdf.CellFormat(72, 4, fmt.Sprintf("Customer: %s", custName), "", 1, "L", false, 0, "")
	}
	if custPhone != "" {
		pdf.CellFormat(72, 4, fmt.Sprintf("Phone: +%s", custPhone), "", 1, "L", false, 0, "")
	}
	if custAddress != "" {
		pdf.MultiCell(72, 4, fmt.Sprintf("Address: %s", custAddress), "", "L", false)
	}

	pdf.CellFormat(72, 3, "--------------------------------------------------", "", 1, "C", false, 0, "")
	pdf.SetFont("Arial", "B", 8)
	pdf.CellFormat(46, 4, "QTY x ITEM", "", 0, "L", false, 0, "")
	pdf.CellFormat(26, 4, "PRICE", "", 1, "R", false, 0, "")
	pdf.CellFormat(72, 3, "--------------------------------------------------", "", 1, "C", false, 0, "")

	pdf.SetFont("Arial", "", 8)
	for _, it := range itemList {
		q := it.Qty
		if q <= 0 {
			q = 1
		}
		name := strings.TrimSpace(it.ProductName)
		if name == "" {
			name = "Item"
		}
		itemStr := fmt.Sprintf("%dx %s", q, name)
		if len(itemStr) > 28 {
			itemStr = itemStr[:26] + ".."
		}
		price := it.LineTotal
		if price <= 0 && it.UnitPrice > 0 {
			price = it.UnitPrice * float64(q)
		}
		pdf.CellFormat(46, 4.5, itemStr, "", 0, "L", false, 0, "")
		pdf.CellFormat(26, 4.5, fmt.Sprintf("Rs. %.2f", price), "", 1, "R", false, 0, "")
	}

	pdf.CellFormat(72, 3, "--------------------------------------------------", "", 1, "C", false, 0, "")
	pdf.SetFont("Arial", "", 8)
	pdf.CellFormat(46, 4, "Subtotal:", "", 0, "L", false, 0, "")
	pdf.CellFormat(26, 4, fmt.Sprintf("Rs. %.2f", subtotalAmt), "", 1, "R", false, 0, "")

	pdf.CellFormat(46, 4, "Delivery Fee:", "", 0, "L", false, 0, "")
	pdf.CellFormat(26, 4, fmt.Sprintf("Rs. %.2f", deliveryFee), "", 1, "R", false, 0, "")

	pdf.CellFormat(72, 3, "--------------------------------------------------", "", 1, "C", false, 0, "")
	pdf.SetFont("Arial", "B", 9)
	pdf.CellFormat(46, 5, "TOTAL AMOUNT:", "", 0, "L", false, 0, "")
	pdf.CellFormat(26, 5, fmt.Sprintf("Rs. %.2f", totalAmt), "", 1, "R", false, 0, "")

	pdf.Ln(1)
	cleanStatus := strings.ToLower(payStatus)
	cleanMode := strings.ToLower(payMode)

	isPaid := strings.Contains(cleanStatus, "paid") || cleanMode == "online" || cleanMode == "upi" || cleanMode == "qr"
	statusText := "Status: CASH ON DELIVERY"
	if isPaid {
		statusText = "Status: PAID (Online)"
	}

	pdf.SetFont("Arial", "B", 8)
	pdf.CellFormat(72, 5, statusText, "", 1, "C", false, 0, "")

	pdf.Ln(2)
	pdf.CellFormat(72, 3, "--------------------------------------------------", "", 1, "C", false, 0, "")
	pdf.SetFont("Arial", "I", 7)
	pdf.CellFormat(72, 4, "Thank you for shopping with us!", "", 1, "C", false, 0, "")

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("pdf generation failed: %w", err)
	}
	return &buf, nil
}

// SendPDFReceiptHand generates the thermal receipt PDF in RAM and delivers it as a WhatsApp DocumentMessage.
func SendPDFReceiptHand(client *whatsmeow.Client, username, customerPhone, orderID string) error {
	if client == nil || !client.IsLoggedIn() {
		return fmt.Errorf("whatsapp client is disconnected")
	}

	cleanPhone := FormatPhone(customerPhone)
	cleanOrdID := strings.TrimSpace(orderID)
	if cleanOrdID == "" {
		cleanOrdID = GetCustomerCurrentOrderID(username, cleanPhone)
	}
	if cleanOrdID == "" {
		if latest := ramstore.Global.GetLatestOrderByPhone(cleanPhone); latest != nil {
			cleanOrdID = latest.OrderID
		}
	}

	pdfBuf, err := GenerateThermalReceiptPDF(cleanOrdID)
	if err != nil {
		log.Printf("⚠️ [Thermal PDF Hand] PDF generation error for +%s (Order: %s): %v", cleanPhone, cleanOrdID, err)
		return err
	}
	if pdfBuf == nil || pdfBuf.Len() == 0 {
		return fmt.Errorf("generated PDF buffer is empty")
	}

	uploaded, errUp := client.Upload(context.Background(), pdfBuf.Bytes(), whatsmeow.MediaDocument)
	if errUp != nil {
		log.Printf("⚠️ [Thermal PDF Hand] WhatsApp upload error for +%s: %v", cleanPhone, errUp)
		return fmt.Errorf("whatsapp media upload failed: %w", errUp)
	}

	lidCacheLock.RLock()
	targetJID, hasTargetJID := phoneToJID[cleanPhone]
	if !hasTargetJID {
		targetJID, hasTargetJID = phoneToJID[customerPhone]
	}
	lidCacheLock.RUnlock()

	if !hasTargetJID {
		primaryServer := types.DefaultUserServer
		if strings.HasPrefix(cleanPhone, "117") || strings.HasPrefix(cleanPhone, "223") || strings.HasPrefix(cleanPhone, "100") {
			primaryServer = "lid"
		}
		targetJID = types.NewJID(cleanPhone, primaryServer)
	}

	dispID := strings.TrimPrefix(cleanOrdID, "#")
	if dispID == "" {
		dispID = "Order"
	}

	docMsg := &waE2E.Message{
		DocumentMessage: &waE2E.DocumentMessage{
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String("application/pdf"),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(pdfBuf.Len())),
			Title:         proto.String("Order Receipt #" + dispID),
			FileName:      proto.String("Receipt_" + dispID + ".pdf"),
		},
	}

	if _, errSend := client.SendMessage(context.Background(), targetJID, docMsg); errSend != nil {
		log.Printf("⚠️ [Thermal PDF Hand] SendMessage error for +%s: %v", cleanPhone, errSend)
		return errSend
	}

	log.Printf("📄 [Thermal PDF Hand] Receipt_%s.pdf delivered to +%s", dispID, cleanPhone)
	return nil
}

// TriggerWebOrderConfirmationPush sends formatted text confirmation and thermal PDF receipt when Central Node pushes a website order.
func TriggerWebOrderConfirmationPush(vendorUser, customerPhoneOrJID, orderID string, totalAmount float64) error {
	cleanPhone := FormatPhone(customerPhoneOrJID)
	cleanOrdID := strings.TrimSpace(orderID)

	client := GetAnyLoggedInClient()
	if client == nil || !client.IsLoggedIn() {
		log.Printf("⚠️ [Web Order Push] WhatsApp client not connected for order #%s", cleanOrdID)
		return fmt.Errorf("no logged-in WhatsApp client available")
	}

	confMsg := fmt.Sprintf("🎉 *Order Received!* Your website order #%s for ₹%.2f has been successfully placed. We are processing it now.", cleanOrdID, totalAmount)
	if err := SendMessage(client, cleanPhone, confMsg); err != nil {
		log.Printf("⚠️ [Web Order Push] Text error for +%s: %v", cleanPhone, err)
	} else {
		PushToLiveChat(vendorUser, cleanPhone, confMsg, "outgoing_ai")
		go SaveCustomerChatAndPrune(vendorUser, cleanPhone, "assistant", confMsg)
	}

	time.Sleep(300 * time.Millisecond)
	return SendPDFReceiptHand(client, vendorUser, cleanPhone, cleanOrdID)
}
