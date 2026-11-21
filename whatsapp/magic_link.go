package whatsapp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"wa-server/central"
	"wa-server/ramstore"

	"go.mau.fi/whatsmeow"
)

func appendAuthToken(rawURL, token string) string {
	u := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if u == "" {
		return ""
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%sauth_token=%s", u, sep, token)
}

// GenerateDualMagicLinks creates a 24-byte hex auto-login token, syncs it asynchronously to Central Brain Node,
// and returns (token, vendorStoreURL, mainMallURL, error).
func GenerateDualMagicLinks(_, jid string) (string, string, string, error) {
	cleanJID := strings.TrimSpace(jid)
	if cleanJID == "" {
		return "", "", "", fmt.Errorf("jid is required")
	}

	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", "", "", fmt.Errorf("crypto rand error: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	expiresAt := time.Now().Add(24 * time.Hour)

	cleanPhone := FormatPhone(strings.Split(cleanJID, "@")[0])
	central.Client.PushMagicLinkAsync(token, cleanJID, cleanPhone, expiresAt)

	vendor := ramstore.Global.GetVendor()
	mallBase := strings.TrimRight(strings.TrimSpace(vendor.MallURL), "/")
	if mallBase == "" {
		mallBase = strings.TrimRight(strings.TrimSpace(ramstore.Global.GetSyncStatus().CentralURL), "/")
	}
	if mallBase == "" {
		mallBase = "https://mall.example.com"
	}

	var storeMagicURL string
	if strings.TrimSpace(vendor.StoreURL) != "" {
		storeMagicURL = appendAuthToken(vendor.StoreURL, token)
	} else {
		slug := strings.TrimSpace(vendor.StoreSlug)
		if slug == "" {
			slug = strings.TrimSpace(vendor.Username)
		}
		if slug == "" {
			slug = "store"
		}
		storeMagicURL = fmt.Sprintf("%s/%s?auth_token=%s", mallBase, slug, token)
	}

	mallMagicURL := appendAuthToken(mallBase, token)
	return token, storeMagicURL, mallMagicURL, nil
}

// GenerateMagicLinkToken maintains compatibility returning (token, vendorStoreURL, err).
func GenerateMagicLinkToken(username, jid string) (string, string, error) {
	token, storeURL, _, err := GenerateDualMagicLinks(username, jid)
	return token, storeURL, err
}

// ExecuteGenerateStoreLinkHand generates personalized auto-login links for BOTH the vendor's individual website
// AND the main multi-vendor mall, and delivers them clearly over WhatsApp.
func ExecuteGenerateStoreLinkHand(client *whatsmeow.Client, username, customerPhone string) error {
	cleanPhone := FormatPhone(customerPhone)
	if cleanPhone == "" {
		return fmt.Errorf("empty customer phone")
	}

	jid := cleanPhone + "@s.whatsapp.net"
	_, storeURL, mallURL, err := GenerateDualMagicLinks(username, jid)
	if err != nil {
		return err
	}

	storeName := strings.TrimSpace(ramstore.Global.GetVendor().BusinessName)
	if storeName == "" {
		storeName = "Our Store"
	}

	msgText := fmt.Sprintf(
		"🛍️ *ONLINE STORE & MULTI-VENDOR MALL LINKS* ✨\n\n"+
			"Namaste! Aap *%s* ka live catalog ya pure Mall ke sabhi stores website par dekh sakte hain:\n\n"+
			"🏪 *1. Humari Dukan (Direct Store):*\n%s\n\n"+
			"🏬 *2. Pure Multi-Vendor Mall me Shopping:*\n%s\n\n"+
			"✨ *Auto-Login Active:* Dono links par aapka WhatsApp cart, variants aur delivery address automatic sync rahenge!\n\n"+
			"_Koi bhi sawal ho toh yahan bejheejhak likhein!_ 🙏🚚",
		storeName, storeURL, mallURL,
	)

	if client != nil && client.IsLoggedIn() {
		err := SendMessage(client, cleanPhone, msgText)
		if err == nil {
			go SaveCustomerChatAndPrune(username, cleanPhone, "assistant", msgText)
		}
		return err
	}
	return nil
}
