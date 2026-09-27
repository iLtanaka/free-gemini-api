package gemini

// chrome_manage.go — PRD Step 10 (Option A): the watchdog -> chrome-manage webhook.
//
// WHY THIS FILE EXISTS (read this before comparing it to the shipped patch)
// -------------------------------------------------------------------------
// `chrome-manage/patches/free-gemini-api-watchdog.md` is written against a
// monitor.go that does not exist in this tree. It assumes:
//
//   * checkAllCookieHealth() returning a []health slice, each element carrying
//     .Status and .AccountID
//   * profileIDForAccount()
//   * a watchdog that already reacts to "expiring_soon"
//
// The real monitor.go has none of that. InspectAccountCookieHealth() returns ONE
// aggregate CookieHealthInfo with no AccountID field, and StartCookieWatchdog()
// only reacts to "expired". Applying the shipped patch verbatim would not
// compile.
//
// So this file supplies the missing per-account layer, adapted to the real code,
// and monitor.go's watchdog gains a small loop that calls into it. The webhook
// itself (callChromeManageRefresh) is byte-for-byte the shipped design, because
// that part was correct.

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ChromeManageURL is the refresh service. Empty disables the webhook and leaves
// the existing BroadcastCookieRefresh() path as the only mechanism.
var ChromeManageURL = envOr("CHROME_MANAGE_URL", "http://localhost:8003")

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// AccountCookieHealth is the per-account counterpart of CookieHealthInfo.
type AccountCookieHealth struct {
	AccountID       string
	ProfileID       int
	HasProfile      bool
	Status          string // healthy | expiring_soon | expired | no_cookies
	MinTTLSeconds   float64
	MinTTLFormatted string
}

// profileAccountRe matches the `profile_<N>` account ids this farm seeds.
// The pool also holds `account_<hash>.json` files pushed by the Chrome
// extension; those carry no profile id and are skipped by the webhook.
var profileAccountRe = regexp.MustCompile(`^profile_(\d+)$`)

// ProfileIDForAccount maps a pool account id to a chrome-manage profile id.
// On this farm the profile id IS the account id for seeded entries, so this is
// usually the identity function -- the indirection is kept because it documents
// the join, and because extension-pushed accounts do not have one.
func ProfileIDForAccount(accountID string) (int, bool) {
	m := profileAccountRe.FindStringSubmatch(accountID)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// InspectPerAccountCookieHealth is the per-account scan the shipped patch
// assumed already existed. One entry per account_*.json in the pool, using the
// same target-cookie rule and the same 2h "expiring_soon" threshold as the
// aggregate InspectAccountCookieHealth().
func InspectPerAccountCookieHealth() []AccountCookieHealth {
	files := GetAvailableAccountCookieFiles()
	out := make([]AccountCookieHealth, 0, len(files))
	now := time.Now().Unix()

	for _, fPath := range files {
		accountID := accountIDFromPath(fPath)
		rec := AccountCookieHealth{AccountID: accountID, Status: "no_cookies"}
		if pid, ok := ProfileIDForAccount(accountID); ok {
			rec.ProfileID = pid
			rec.HasProfile = true
		}

		data, err := os.ReadFile(fPath)
		if err != nil {
			out = append(out, rec)
			continue
		}
		var cookies []CookieObject
		if err := json.Unmarshal(data, &cookies); err != nil {
			out = append(out, rec)
			continue
		}

		minRemaining := float64(999999999)
		found := false
		for _, ck := range cookies {
			if (ck.Name == "__Secure-1PSIDTS" || ck.Name == "__Secure-3PSIDTS" || ck.Name == "SIDCC") && ck.ExpirationDate > 1700000000 {
				found = true
				if rem := ck.ExpirationDate - float64(now); rem < minRemaining {
					minRemaining = rem
				}
			}
		}

		if !found {
			// Same convention as the aggregate checker: no dated target cookie
			// means nothing is expiring, so it reads healthy rather than broken.
			rec.Status = "healthy"
			rec.MinTTLSeconds = 86400
			rec.MinTTLFormatted = "> 24h"
			out = append(out, rec)
			continue
		}

		rec.MinTTLSeconds = minRemaining
		switch {
		case minRemaining <= 0:
			rec.Status = "expired"
			rec.MinTTLFormatted = "Expired"
		case minRemaining < 2*3600:
			rec.Status = "expiring_soon"
			rec.MinTTLFormatted = formatTTL(minRemaining)
		default:
			rec.Status = "healthy"
			rec.MinTTLFormatted = formatTTL(minRemaining)
		}
		out = append(out, rec)
	}
	return out
}

func formatTTL(seconds float64) string {
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	if h > 0 {
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
	return strconv.Itoa(m) + "m"
}

// accountIDFromPath turns "cookies/account_profile_3.json" into "profile_3".
func accountIDFromPath(p string) string {
	base := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		base = p[i+1:]
	}
	base = strings.TrimSuffix(base, ".json")
	base = strings.TrimPrefix(base, "account_")
	return base
}

// ---------------------------------------------------------------------------
// the webhook
// ---------------------------------------------------------------------------

var lastRefreshRequest = struct {
	sync.Mutex
	at map[int]time.Time
}{at: make(map[int]time.Time)}

const refreshCooldown = 30 * time.Minute

// shouldRequestRefresh rate-limits per profile. `expiring_soon` is a STATE, not
// an event -- it stays true for hours -- so an unguarded call on every
// 30-minute tick would fire ~48 refreshes a day per account. Keep the
// last-requested timestamp and skip a profile asked for inside the cooldown.
func shouldRequestRefresh(profileID int) bool {
	lastRefreshRequest.Lock()
	defer lastRefreshRequest.Unlock()
	if t, ok := lastRefreshRequest.at[profileID]; ok &&
		time.Since(t) < refreshCooldown {
		return false
	}
	lastRefreshRequest.at[profileID] = time.Now()
	return true
}

// callChromeManageRefresh asks chrome-manage to re-harvest one profile.
//
// Fire-and-forget by design. chrome-manage answers 202 immediately and runs the
// chain in the background (VM boot + SSH + harvest is 1-3 minutes on a cold
// host), so a short client timeout is correct -- it is NOT waiting for the
// refresh to finish. Do not "fix" this by raising the timeout: it would stall
// the watchdog's own health loop.
func callChromeManageRefresh(profileID int) {
	if ChromeManageURL == "" {
		return
	}

	body, err := json.Marshal(map[string]any{
		"profile_id": profileID,
		"reason":     "watchdog",
	})
	if err != nil {
		log.Printf("chrome-manage: marshal failed for profile %d: %v", profileID, err)
		return
	}

	req, err := http.NewRequest(
		http.MethodPost, ChromeManageURL+"/api/refresh", bytes.NewReader(body))
	if err != nil {
		log.Printf("chrome-manage: request build failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Not fatal: the chrome-manage 6-hourly poller is the safety net.
		log.Printf("chrome-manage: refresh call failed for profile %d: %v", profileID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("chrome-manage: profile %d -> HTTP %d", profileID, resp.StatusCode)
		return
	}
	log.Printf("chrome-manage: refresh accepted for profile %d (HTTP %d)", profileID, resp.StatusCode)
}
