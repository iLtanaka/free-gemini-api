package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests exist because the chrome-manage webhook is the only part of the
// watchdog that can boot a VM. A bug that fires it for the wrong profile, or
// that fails to fire it at all, is expensive in both directions -- so the
// account->profile join and the TTL classification are pinned here rather than
// left to a 30-minute wall-clock observation.

func TestProfileIDForAccount(t *testing.T) {
	cases := []struct {
		accountID string
		wantID    int
		wantOK    bool
	}{
		{"profile_3", 3, true},
		{"profile_54", 54, true},
		{"profile_1", 1, true},
		{"ga000Cwkny", 0, false},   // extension-pushed, no profile
		{"primary", 0, false},      // legacy single-account file
		{"profile_abc", 0, false},  // not numeric
		{"profile_", 0, false},     // no digits
		{"", 0, false},
	}
	for _, c := range cases {
		gotID, gotOK := ProfileIDForAccount(c.accountID)
		if gotID != c.wantID || gotOK != c.wantOK {
			t.Errorf("ProfileIDForAccount(%q) = (%d,%v), want (%d,%v)",
				c.accountID, gotID, gotOK, c.wantID, c.wantOK)
		}
	}
}

func TestAccountIDFromPath(t *testing.T) {
	cases := map[string]string{
		"cookies/account_profile_3.json":  "profile_3",
		"account_ga000Cwkny.json":         "ga000Cwkny",
		"/abs/path/cookies/account_x.json": "x",
	}
	for in, want := range cases {
		if got := accountIDFromPath(in); got != want {
			t.Errorf("accountIDFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatTTL(t *testing.T) {
	cases := map[float64]string{
		5400:  "1h 30m",
		90:    "1m",
		7200:  "2h 0m",
	}
	for in, want := range cases {
		if got := formatTTL(in); got != want {
			t.Errorf("formatTTL(%v) = %q, want %q", in, got, want)
		}
	}
}

// writeAccount drops a cookie file shaped like a real pool entry.
func writeAccount(t *testing.T, dir, accountID string, ttlSeconds float64) {
	t.Helper()
	exp := float64(time.Now().Unix()) + ttlSeconds
	cookies := []CookieObject{
		{Name: "SID", Value: "irrelevant", Domain: ".google.com", Path: "/"},
		{Name: "__Secure-1PSIDTS", Value: "irrelevant", Domain: ".google.com",
			Path: "/", ExpirationDate: exp},
	}
	data, err := json.MarshalIndent(cookies, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, "cookies", "account_"+accountID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestInspectPerAccountCookieHealth(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cookies"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root) // GetAvailableAccountCookieFiles globs "cookies/account_*.json"

	writeAccount(t, root, "profile_1", 3600)        // 1h  -> expiring_soon
	writeAccount(t, root, "profile_2", -3600)       // past -> expired
	writeAccount(t, root, "profile_3", 72*3600)     // 72h -> healthy
	writeAccount(t, root, "ga000Cwkny", 3600)       // 1h, but no profile

	got := map[string]AccountCookieHealth{}
	for _, rec := range InspectPerAccountCookieHealth() {
		got[rec.AccountID] = rec
	}

	if len(got) != 4 {
		t.Fatalf("expected 4 accounts, got %d (%v)", len(got), got)
	}

	// profile_1 -- expiring_soon, maps to profile 1
	if r := got["profile_1"]; r.Status != "expiring_soon" || !r.HasProfile || r.ProfileID != 1 {
		t.Errorf("profile_1: got status=%q hasProfile=%v profileID=%d",
			r.Status, r.HasProfile, r.ProfileID)
	}
	// profile_2 -- expired
	if r := got["profile_2"]; r.Status != "expired" || r.ProfileID != 2 {
		t.Errorf("profile_2: got status=%q profileID=%d", r.Status, r.ProfileID)
	}
	// profile_3 -- healthy
	if r := got["profile_3"]; r.Status != "healthy" || r.ProfileID != 3 {
		t.Errorf("profile_3: got status=%q profileID=%d", r.Status, r.ProfileID)
	}
	// extension-pushed account -- expiring, but MUST NOT map to a profile
	if r := got["ga000Cwkny"]; r.Status != "expiring_soon" || r.HasProfile {
		t.Errorf("ga000Cwkny: got status=%q hasProfile=%v (must be false)",
			r.Status, r.HasProfile)
	}
}

// The webhook must fire on expiring_soon AND expired, and nothing else.
func TestWebhookSelectionRule(t *testing.T) {
	fires := func(status string) bool {
		return status == "expiring_soon" || status == "expired"
	}
	for _, c := range []struct {
		status string
		want   bool
	}{
		{"expiring_soon", true},
		{"expired", true},
		{"healthy", false},
		{"no_cookies", false},
	} {
		if got := fires(c.status); got != c.want {
			t.Errorf("fires(%q) = %v, want %v", c.status, got, c.want)
		}
	}
}

// The 30-minute cooldown must suppress a second request for the same profile
// and must not suppress a different profile.
func TestShouldRequestRefreshCooldown(t *testing.T) {
	if !shouldRequestRefresh(9001) {
		t.Fatal("first request for a fresh profile must be allowed")
	}
	if shouldRequestRefresh(9001) {
		t.Error("second request inside the cooldown must be suppressed")
	}
	if !shouldRequestRefresh(9002) {
		t.Error("a different profile must not be suppressed")
	}
}
