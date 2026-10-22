package nan

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const liveShapePayload = `{"periodStart":"2026-10-01","models":[` +
	`{"model":"deepseek-v4-flash","tokensUsed":1823091326,"cap":3000000000,"remaining":1176908674,"updatedAt":"2026-10-21T06:31:49Z","periodEnd":"2026-11-01T00:00:00Z","extraTokens":0,"extraUsable":true},` +
	`{"model":"glm5.3-flash","tokensUsed":1160890190,"cap":2000000000,"remaining":839109810,"updatedAt":"2026-10-22T00:11:38Z","periodEnd":"2026-11-01T00:00:00Z","extraTokens":0,"extraUsable":true},` +
	`{"model":"glm5.3","tokensUsed":0,"cap":3000000000,"remaining":3000000000,"periodEnd":"2026-11-11T20:30:51Z","fullWindowTokens":400000000,"windowHours":4,"extraTokens":0,"extraUsable":false}]}`

func TestParseQuota_PerModelAllowanceAndResets(t *testing.T) {
	quota, err := ParseQuota([]byte(liveShapePayload), 0)
	if err != nil {
		t.Fatalf("the live-shaped payload must parse: %v", err)
	}
	if len(quota.Models) != 3 {
		t.Fatalf("three metered models, got %d", len(quota.Models))
	}
	deepseek := quota.Models[0]
	if deepseek.Model != "deepseek-v4-flash" || deepseek.TokensUsed != 1823091326 || deepseek.Allowance != 3000000000 {
		t.Fatalf("unexpected first model: %+v", deepseek)
	}
	if deepseek.PeriodEnd == nil || !deepseek.PeriodEnd.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the period end must parse as the reset: %+v", deepseek.PeriodEnd)
	}
	if deepseek.Window != nil {
		t.Fatalf("a model without a rolling window reports none: %+v", deepseek.Window)
	}
	glm := quota.Models[2]
	if glm.Window == nil || glm.Window.Budget != 400000000 || glm.Window.Hours != 4 {
		t.Fatalf("the rolling window must ride along: %+v", glm.Window)
	}
	if quota.PeriodStart == nil || !quota.PeriodStart.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the account period start must parse: %+v", quota.PeriodStart)
	}
}

func TestParseQuota_FullCapWinsOverCap(t *testing.T) {
	payload := `{"models":[{"model":"m","tokensUsed":10,"cap":100,"fullCap":500}]}`
	quota, err := ParseQuota([]byte(payload), 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if quota.Models[0].Allowance != 500 {
		t.Fatalf("the full-period cap is the allowance the dashboard divides by: %d", quota.Models[0].Allowance)
	}
}

func TestParseQuota_ModelWithoutAllowanceIsSkipped(t *testing.T) {
	payload := `{"models":[{"model":"free","tokensUsed":5},{"model":"m","tokensUsed":5,"cap":100}]}`
	quota, err := ParseQuota([]byte(payload), 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(quota.Models) != 1 || quota.Models[0].Model != "m" {
		t.Fatalf("the allowanceless model is skipped, the metered one kept: %+v", quota.Models)
	}
}

func TestParseQuota_UnreadableUsageFailsWholeRead(t *testing.T) {
	payload := `{"models":[{"model":"m","tokensUsed":5,"cap":100},{"model":"broken","tokensUsed":"many","cap":100}]}`
	if _, err := ParseQuota([]byte(payload), 0); err == nil {
		t.Fatal("a partial payload must fail the whole read instead of understating the account")
	}
}

func TestResolveAPIKey_DirectKeyWins(t *testing.T) {
	key, err := ResolveAPIKey(func(name string) string {
		if name == "NAN_API_KEY" {
			return " direct-key "
		}
		return "dotfiles gateway credential print --for nan"
	})
	if err != nil || key != "direct-key" {
		t.Fatalf("the direct key wins, trimmed: %q %v", key, err)
	}
}

func TestResolveAPIKey_CommandOutputIsTrimmed(t *testing.T) {
	origRunner := commandRunner
	commandRunner = func(command string) ([]byte, error) { return []byte("  nan-key\n"), nil }
	defer func() { commandRunner = origRunner }()
	key, err := ResolveAPIKey(func(name string) string {
		if name == "NAN_API_KEY_COMMAND" {
			return "dotfiles gateway credential print --for nan"
		}
		return ""
	})
	if err != nil || key != "nan-key" {
		t.Fatalf("the command key is trimmed: %q %v", key, err)
	}
}

func TestResolveAPIKey_NothingConfigured(t *testing.T) {
	if _, err := ResolveAPIKey(func(string) string { return "" }); err == nil {
		t.Fatal("no key and no command is a configuration error, not silence")
	}
}

func TestFetch_SendsBearerAndParses(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(liveShapePayload))
	}))
	defer server.Close()
	origURL := QuotaURL
	QuotaURL = server.URL
	defer func() { QuotaURL = origURL }()

	quota, err := Fetch("nan-key", 0, server.Client())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if auth != "Bearer nan-key" {
		t.Fatalf("the endpoint authenticates with the bearer: %q", auth)
	}
	if len(quota.Models) != 3 {
		t.Fatalf("the snapshot parses: %d models", len(quota.Models))
	}
}

func TestFetch_RefusesRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.origin.example/steal", http.StatusFound)
	}))
	defer server.Close()
	origURL := QuotaURL
	QuotaURL = server.URL
	defer func() { QuotaURL = origURL }()

	// A nil client keeps Fetch's own redirect refusal: the 302 surfaces as an
	// error instead of the bearer landing on the redirect target.
	if _, err := Fetch("nan-key", 0, nil); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("a redirect must surface as an error, never be followed: %v", err)
	}
}
