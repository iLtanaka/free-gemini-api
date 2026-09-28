package api

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"goapi/db"
	"goapi/gemini"
	"log"
	"os"
	"strings"

	"github.com/gofiber/fiber/v3"
)

//go:embed admin_ui.html
var adminUIFS embed.FS

var adminToken string

// InitAdminAuth resolves the token that protects the account-pool admin API
// (list/tier/delete - not /api/sync-cookies, which the Chrome extension and
// the farm's chrome-manage tooling already call unauthenticated and which
// this project's protocol with the extension must not change). Reads
// ADMIN_TOKEN from the environment; if unset, generates a random one and
// logs it, so the admin surface is never open by accident even if the
// operator forgets to set it.
func InitAdminAuth() {
	adminToken = strings.TrimSpace(os.Getenv("ADMIN_TOKEN"))
	if adminToken != "" {
		log.Println("🔐 Admin UI: using ADMIN_TOKEN from environment")
		return
	}

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("❌ Failed to generate admin token: %v", err)
	}
	adminToken = hex.EncodeToString(buf)
	log.Printf("🔐 Admin UI token (no ADMIN_TOKEN set, generated for this run): %s", adminToken)
	log.Println("🔐 Open /admin and enter that token to manage the account pool. Set ADMIN_TOKEN to keep it stable across restarts.")
}

// adminAuthRequired guards the admin JSON API. The admin page itself
// (GET /admin) is served without auth since it holds no data - the token is
// entered client-side and sent as a Bearer header on every API call.
func adminAuthRequired(c fiber.Ctx) error {
	got := strings.TrimPrefix(c.Get("Authorization"), "Bearer ")
	got = strings.TrimSpace(got)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(adminToken)) != 1 {
		return c.Status(401).JSON(fiber.Map{"error": "Unauthorized. Pass the admin token as 'Authorization: Bearer <token>'."})
	}
	return c.Next()
}

// HandleAdminUI serves the static account-management page.
func HandleAdminUI(c fiber.Ctx) error {
	data, err := adminUIFS.ReadFile("admin_ui.html")
	if err != nil {
		return c.Status(500).SendString("admin UI missing")
	}
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.Send(data)
}

// adminAccountView merges live worker-pool state with the DB's tier label
// for one account. Never includes raw cookie contents - only the file path
// (a name, not a secret) and usage/health metadata.
type adminAccountView struct {
	ID                    string `json:"id"`
	Tier                  string `json:"tier"`
	Status                string `json:"status"`
	CookiePath            string `json:"cookie_path"`
	InFlight              int64  `json:"in_flight"`
	TotalServed           int64  `json:"total_served"`
	TotalErrors           int64  `json:"total_errors"`
	CooldownSecsRemaining int    `json:"cooldown_seconds_remaining"`
	LastUsed              string `json:"last_used"`
	InPool                bool   `json:"in_pool"`
}

// HandleAdminListAccounts returns every known account (currently pooled, or
// only tracked in the DB from a prior run) with its tier and live stats.
func HandleAdminListAccounts(c fiber.Ctx) error {
	dbRows, err := db.GetAccounts()
	if err != nil {
		log.Printf("⚠️ Failed to read accounts from DB: %v", err)
		dbRows = nil
	}

	tierByID := map[string]string{}
	for _, row := range dbRows {
		id, _ := row["account_id"].(string)
		tier, _ := row["tier"].(string)
		if id != "" {
			tierByID[id] = tier
		}
	}

	seen := map[string]bool{}
	var accounts []adminAccountView

	for _, w := range GetWorkerPool().GetStats().Workers {
		tier := tierByID[w.ID]
		if tier == "" {
			tier = "normal"
		}
		accounts = append(accounts, adminAccountView{
			ID:                    w.ID,
			Tier:                  tier,
			Status:                w.Status,
			CookiePath:            w.CookiePath,
			InFlight:              w.InFlight,
			TotalServed:           w.TotalServed,
			TotalErrors:           w.TotalErrors,
			CooldownSecsRemaining: w.CooldownSecsRemaining,
			LastUsed:              w.LastUsed,
			InPool:                true,
		})
		seen[w.ID] = true
	}

	// Accounts the DB remembers but that aren't currently loaded as a
	// worker (e.g. cookie file was removed outside the admin UI, or the
	// pool hasn't reloaded yet) still show up, flagged as not in-pool.
	for _, row := range dbRows {
		id, _ := row["account_id"].(string)
		if id == "" || seen[id] {
			continue
		}
		tier, _ := row["tier"].(string)
		if tier == "" {
			tier = "normal"
		}
		cookieFile, _ := row["cookie_file"].(string)
		lastUsed, _ := row["last_used_at"].(string)
		accounts = append(accounts, adminAccountView{
			ID:         id,
			Tier:       tier,
			Status:     "not_in_pool",
			CookiePath: cookieFile,
			LastUsed:   lastUsed,
			InPool:     false,
		})
	}

	return c.JSON(fiber.Map{"accounts": accounts})
}

// HandleAdminSetTier updates an account's manual tier label.
func HandleAdminSetTier(c fiber.Ctx) error {
	accountID := c.Params("id")
	var body struct {
		Tier string `json:"tier"`
	}
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid request"})
	}

	cookiePath := fmt.Sprintf("cookies/account_%s.json", accountID)
	if err := db.SetAccountTier(accountID, cookiePath, body.Tier); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"status": "success", "id": accountID, "tier": body.Tier})
}

// HandleAdminUsage returns aggregate stats plus an hourly request-count
// series for the admin dashboard's usage charts.
func HandleAdminUsage(c fiber.Ctx) error {
	hourly, err := db.GetHourlyRequestCounts(24)
	if err != nil {
		log.Printf("⚠️ Failed to read hourly request counts: %v", err)
		hourly = nil
	}
	stats, err := db.GetSystemStats()
	if err != nil {
		stats = map[string]any{}
	}
	return c.JSON(fiber.Map{
		"hourly_requests": hourly,
		"stats":           stats,
	})
}

// HandleAdminDeleteAccount removes an account's cookie file(s) and DB row,
// then reloads the worker pool so it drops out immediately.
func HandleAdminDeleteAccount(c fiber.Ctx) error {
	accountID := c.Params("id")

	if err := gemini.DeleteAccountCookieFiles(accountID); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": err.Error()})
	}
	if err := db.DeleteAccount(accountID); err != nil {
		log.Printf("⚠️ Failed to delete DB row for account %s: %v", accountID, err)
	}

	GetWorkerPool().ReloadPool()
	return c.JSON(fiber.Map{"status": "success", "id": accountID})
}
