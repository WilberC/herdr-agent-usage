/**
 * NaN Cloud subscription quota.
 *
 * NaN keeps no rate-limit headers and no local session files: the plan state
 * lives on the quota endpoint the official dashboard reads, authenticated by
 * the same API key the chat API uses. The payload sits outside NaN's
 * published OpenAPI, so parsing reads it defensively — a model that reports
 * no allowance is skipped exactly as the dashboard skips it, while a metered
 * model whose usage cannot be read fails the whole read, so a partial payload
 * never replaces a complete snapshot with a cheaper-looking one.
 */
package nan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// QuotaURL is NaN Cloud's quota endpoint, the one the official dashboard reads.
// A var so tests can point it at a stub server.
var QuotaURL = "https://cloud-api.nan.builders/api/usage/quota"

// FetchTimeout bounds one quota request.
const FetchTimeout = 10 * time.Second

// rawQuota mirrors the on-the-wire payload. Every field is optional because
// the endpoint is undocumented.
type rawQuota struct {
	PeriodStart string     `json:"periodStart"`
	Models      []rawModel `json:"models"`
}

type rawModel struct {
	Model            string       `json:"model"`
	TokensUsed       json.Number  `json:"tokensUsed"`
	Cap              json.Number  `json:"cap"`
	FullCap          json.Number  `json:"fullCap"`
	PeriodEnd        string       `json:"periodEnd"`
	FullWindowTokens *json.Number `json:"fullWindowTokens"`
	WindowTokens     *json.Number `json:"windowTokens"`
	WindowHours      *json.Number `json:"windowHours"`
	WindowTokensUsed *json.Number `json:"windowTokensUsed"`
	WindowResetsAt   string       `json:"windowResetsAt"`
}

// RollingWindow is the per-model usage window some models apply on top of
// their billing-period allowance.
type RollingWindow struct {
	TokensUsed int64
	Budget     int64
	Hours      int64
	ResetsAt   *time.Time
}

// ModelQuota is one metered model's billing-period allowance.
type ModelQuota struct {
	Model       string
	TokensUsed  int64
	Allowance   int64
	PeriodStart *time.Time
	PeriodEnd   *time.Time
	Window      *RollingWindow
}

// Quota is one parsed snapshot of the whole account.
type Quota struct {
	PeriodStart *time.Time
	Models      []ModelQuota
	FetchedAtMs int64
}

// parseTime accepts RFC 3339 timestamps and bare dates.
func parseTime(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return &parsed
		}
	}
	return nil
}

// parsePositive accepts a JSON number greater than zero; everything else is
// absent, and absence means the field carries no information.
func parsePositive(value json.Number) (int64, bool) {
	parsed, err := value.Float64()
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return int64(parsed), true
}

// deref reads an optional JSON number as zero.
func deref(value *json.Number) json.Number {
	if value == nil {
		return json.Number("0")
	}
	return *value
}

// parseNonNegative accepts a JSON number of at least zero.
func parseNonNegative(value json.Number) (int64, bool) {
	parsed, err := value.Float64()
	if err != nil || parsed < 0 {
		return 0, false
	}
	return int64(parsed), true
}

// ParseQuota decodes one payload. A metered model (one with an allowance)
// whose usage cannot be read fails the whole snapshot: a partial read would
// understate every row it feeds, so the caller keeps the last valid one.
func ParseQuota(payload []byte, nowMs int64) (Quota, error) {
	var raw rawQuota
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Quota{}, fmt.Errorf("nan quota: %w", err)
	}
	quota := Quota{FetchedAtMs: nowMs}
	if raw.PeriodStart != "" {
		quota.PeriodStart = parseTime(raw.PeriodStart)
	}
	for _, entry := range raw.Models {
		if entry.Model == "" {
			continue
		}
		allowance, ok := parsePositive(entry.FullCap)
		if !ok {
			allowance, ok = parsePositive(entry.Cap)
		}
		// The dashboard draws nothing for a model without an allowance.
		if !ok {
			continue
		}
		tokensUsed, ok := parseNonNegative(entry.TokensUsed)
		if !ok {
			return Quota{}, fmt.Errorf("nan quota: model %s reports no readable usage", entry.Model)
		}
		model := ModelQuota{
			Model:       entry.Model,
			TokensUsed:  tokensUsed,
			Allowance:   allowance,
			PeriodStart: quota.PeriodStart,
			PeriodEnd:   parseTime(entry.PeriodEnd),
		}
		// A model reporting its window budget is metered per window even when
		// the payload omits the used figure — absent means zero, exactly as
		// the dashboard draws an untouched window.
		if entry.FullWindowTokens != nil || entry.WindowTokens != nil {
			budget, ok := parsePositive(deref(entry.FullWindowTokens))
			if !ok {
				budget, ok = parsePositive(deref(entry.WindowTokens))
			}
			if ok {
				used := int64(0)
				if entry.WindowTokensUsed != nil {
					used, _ = parseNonNegative(*entry.WindowTokensUsed)
				}
				hours := int64(0)
				if entry.WindowHours != nil {
					hours, _ = parsePositive(*entry.WindowHours)
				}
				window := &RollingWindow{TokensUsed: used, Budget: budget, Hours: hours}
				if reset := parseTime(entry.WindowResetsAt); reset != nil {
					window.ResetsAt = reset
				}
				model.Window = window
			}
		}
		quota.Models = append(quota.Models, model)
	}
	return quota, nil
}

// commandRunner runs the credential command; tests stub it.
var commandRunner = func(command string) ([]byte, error) {
	return exec.Command("/bin/sh", "-c", command).Output()
}

// ResolveAPIKey returns the nan API key: NAN_API_KEY verbatim, else the
// trimmed first output line of the command NAN_API_KEY_COMMAND names. The
// command form keeps the key inside a secret store instead of a config file
// or the process environment.
func ResolveAPIKey(getenv func(string) string) (string, error) {
	if key := strings.TrimSpace(getenv("NAN_API_KEY")); key != "" {
		return key, nil
	}
	command := strings.TrimSpace(getenv("NAN_API_KEY_COMMAND"))
	if command == "" {
		return "", errors.New("nan quota: neither NAN_API_KEY nor NAN_API_KEY_COMMAND is set")
	}
	output, err := commandRunner(command)
	if err != nil {
		return "", fmt.Errorf("nan quota: %s failed: %w", command, err)
	}
	key := strings.TrimSpace(string(output))
	if key == "" {
		return "", fmt.Errorf("nan quota: %s printed no key", command)
	}
	return key, nil
}

// Fetch reads one quota snapshot. Redirects are refused: the endpoint sits
// outside a published contract and the bearer must never be replayed to
// another origin.
func Fetch(apiKey string, nowMs int64, client *http.Client) (Quota, error) {
	if client == nil {
		client = &http.Client{
			Timeout:       FetchTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	request, err := http.NewRequest(http.MethodGet, QuotaURL, nil)
	if err != nil {
		return Quota{}, fmt.Errorf("nan quota: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := client.Do(request)
	if err != nil {
		return Quota{}, fmt.Errorf("nan quota: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Quota{}, fmt.Errorf("nan quota: HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Quota{}, fmt.Errorf("nan quota: %w", err)
	}
	return ParseQuota(payload, nowMs)
}
