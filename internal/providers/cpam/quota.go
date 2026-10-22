/**
 * CPAM gateway accounts' plan quotas.
 *
 * The user's CLI Proxy API instance fronts OAuth accounts whose plan state no
 * local file records: the agents talk to the proxy, and the proxy talks to the
 * upstream plan. The management API is the only surface that can read those
 * accounts' own usage endpoints — `GET /v0/management/auth-files` lists the
 * credentials the proxy holds, and `POST /v0/management/api-call` runs one
 * upstream request with the stored OAuth bearer substituted for `$TOKEN$`.
 *
 * Only claude and codex accounts are read: the upstream adapters are verified
 * against real responses, and antigravity quota already arrives through the
 * plugin's own statusLine collector.
 */
package cpam

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Defaults; both overridable through the environment.
const (
	DefaultBaseURL = "http://127.0.0.1:8317"
	FetchTimeout   = 30 * time.Second
)

// ClaudeUsageURL is Anthropic's OAuth usage endpoint, read through the proxy
// with the account's own bearer.
const ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// CodexUsageURL is ChatGPT's Codex usage endpoint, read the same way.
const CodexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// ErrInvalidKey is the proxy's rejection of a wrong management key.
var ErrInvalidKey = errors.New("cpam: the management key was rejected")

// Account is one enabled credential the proxy holds.
type Account struct {
	Provider  string // claude, codex, antigravity, ...
	Email     string
	AuthIndex string
	PlanType  string
}

type authFiles struct {
	Files []struct {
		Provider  string `json:"provider"`
		Email     string `json:"email"`
		AuthIndex string `json:"auth_index"`
		Disabled  bool   `json:"disabled"`
	} `json:"files"`
}

// ListAccounts returns the enabled claude/codex credentials the proxy holds.
func ListAccounts(baseURL, key string, client *http.Client) ([]Account, error) {
	body, _, err := managementGet(baseURL, key, "auth-files", client)
	if err != nil {
		return nil, err
	}
	var parsed authFiles
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("cpam: auth-files: %w", err)
	}
	var out []Account
	for _, file := range parsed.Files {
		provider := strings.ToLower(strings.TrimSpace(file.Provider))
		if provider != "claude" && provider != "codex" {
			continue
		}
		if file.Disabled || file.AuthIndex == "" {
			continue
		}
		out = append(out, Account{
			Provider:  provider,
			Email:     strings.TrimSpace(file.Email),
			AuthIndex: file.AuthIndex,
		})
	}
	return out, nil
}

// apiCallRequest is the management api-call body. The proxy substitutes
// $TOKEN$ in every header value with the stored OAuth bearer.
type apiCallRequest struct {
	AuthIndex string            `json:"authIndex"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Header    map[string]string `json:"header,omitempty"`
}

// apiCallResponse wraps the upstream exchange.
type apiCallResponse struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func managementGet(baseURL, key, path string, client *http.Client) ([]byte, int, error) {
	if client == nil {
		client = &http.Client{Timeout: FetchTimeout}
	}
	request, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/v0/management/"+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("cpam: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	body, status, err := doRequest(client, request)
	if err != nil {
		return nil, 0, err
	}
	if status == http.StatusUnauthorized {
		return nil, status, ErrInvalidKey
	}
	return body, status, nil
}

// apiCall runs one upstream request through the proxy and returns the raw
// upstream body.
func apiCall(baseURL, key string, request apiCallRequest, client *http.Client) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: FetchTimeout}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("cpam: %w", err)
	}
	httpRequest, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/v0/management/api-call", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("cpam: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+key)
	httpRequest.Header.Set("Content-Type", "application/json")
	body, status, err := doRequest(client, httpRequest)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, ErrInvalidKey
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("cpam: api-call: HTTP %d", status)
	}
	var parsed apiCallResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("cpam: api-call: %w", err)
	}
	if parsed.StatusCode < 200 || parsed.StatusCode >= 300 {
		return nil, fmt.Errorf("cpam: upstream HTTP %d", parsed.StatusCode)
	}
	return []byte(parsed.Body), nil
}

func doRequest(client *http.Client, request *http.Request) ([]byte, int, error) {
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("cpam: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("cpam: %w", err)
	}
	return body, response.StatusCode, nil
}

// Window is one normalized quota window.
type Window struct {
	UsedPercent float64
	// ResetAt is unix epoch seconds; nil when the upstream reports none.
	ResetAt *int64
	// WindowMinutes is the upstream's own window length in minutes; nil when
	// the payload does not state one (the panel then labels the window from
	// its slot: primary 5h, secondary 7d).
	WindowMinutes *int
}

// AccountUsage is one account's parsed upstream usage.
type AccountUsage struct {
	Account Account
	Primary *Window
	Weekly  *Window
}

// ProviderAccountLabel names the account inside the panel group: which
// provider's plan it is and whose.
func (u AccountUsage) ProviderAccountLabel() string {
	if u.Account.Email == "" {
		return u.Account.Provider
	}
	return u.Account.Provider + " · " + u.Account.Email
}

const claudeBeta = "oauth-2025-04-20"

// FetchUsage reads one account's upstream usage. Each provider's adapter
// accepts the shapes the upstream has actually served.
func FetchUsage(baseURL, key string, account Account, client *http.Client) (AccountUsage, error) {
	usage := AccountUsage{Account: account}
	switch account.Provider {
	case "claude":
		body, err := apiCall(baseURL, key, apiCallRequest{
			AuthIndex: account.AuthIndex,
			Method:    http.MethodGet,
			URL:       ClaudeUsageURL,
			Header: map[string]string{
				"Authorization":  "Bearer $TOKEN$",
				"Content-Type":   "application/json",
				"anthropic-beta": claudeBeta,
			},
		}, client)
		if err != nil {
			return usage, err
		}
		usage.Primary, usage.Weekly, err = parseClaudeUsage(body)
		return usage, err
	case "codex":
		body, err := apiCall(baseURL, key, apiCallRequest{
			AuthIndex: account.AuthIndex,
			Method:    http.MethodGet,
			URL:       CodexUsageURL,
			Header: map[string]string{
				"Authorization": "Bearer $TOKEN$",
				"Content-Type":  "application/json",
				"User-Agent":    "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal",
			},
		}, client)
		if err != nil {
			return usage, err
		}
		usage.Primary, usage.Weekly, err = parseCodexUsage(body)
		return usage, err
	default:
		return usage, fmt.Errorf("cpam: no usage adapter for %s", account.Provider)
	}
}

// parseClaudeUsage reads one utilization fraction (0..1) per window; a
// window the upstream omits reports nothing.
func parseClaudeUsage(body []byte) (*Window, *Window, error) {
	var parsed struct {
		FiveHour *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		} `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("cpam: claude usage: %w", err)
	}
	primary, err := claudeWindow(parsed.FiveHour)
	if err != nil {
		return nil, nil, err
	}
	weekly, err := claudeWindow(parsed.SevenDay)
	if err != nil {
		return nil, nil, err
	}
	return primary, weekly, nil
}

func claudeWindow(raw *struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}) (*Window, error) {
	if raw == nil || raw.Utilization == nil {
		return nil, nil
	}
	window := &Window{UsedPercent: *raw.Utilization * 100}
	if raw.ResetsAt != nil {
		if when, err := time.Parse(time.RFC3339, *raw.ResetsAt); err == nil {
			seconds := when.Unix()
			window.ResetAt = &seconds
		}
	}
	return window, nil
}

// codexWindow is one rate-limit slot in either response shape the upstream
// has served.
type codexWindow struct {
	UsedPercent      *float64 `json:"used_percent"`
	UsedPercentCamel *float64 `json:"usedPercent"`
	WindowSeconds    *int64   `json:"limit_window_seconds"`
	ResetAt          *int64   `json:"reset_at"`
	ResetInSeconds   *int64   `json:"resets_in_seconds"`
	ResetAtIso       *string  `json:"resets_at"`
}

func (w *codexWindow) toWindow() *Window {
	if w == nil {
		return nil
	}
	used := w.UsedPercent
	if used == nil {
		used = w.UsedPercentCamel
	}
	if used == nil {
		return nil
	}
	window := &Window{UsedPercent: *used}
	if w.WindowSeconds != nil {
		minutes := int(*w.WindowSeconds / 60)
		window.WindowMinutes = &minutes
	}
	switch {
	case w.ResetAt != nil:
		seconds := *w.ResetAt
		window.ResetAt = &seconds
	case w.ResetInSeconds != nil && *w.ResetInSeconds > 0:
		seconds := time.Now().Unix() + *w.ResetInSeconds
		window.ResetAt = &seconds
	case w.ResetAtIso != nil:
		if when, err := time.Parse(time.RFC3339, *w.ResetAtIso); err == nil {
			seconds := when.Unix()
			window.ResetAt = &seconds
		}
	}
	return window
}

// parseCodexUsage reads the rate-limit windows. The live shape is
// rate_limit.{primary_window,secondary_window} with used_percent and a unix
// reset_at; the shape the pi extension documents (rate_limits.{primary,
// secondary} with used_percent and resets_in_seconds) is accepted too.
func parseCodexUsage(body []byte) (*Window, *Window, error) {
	var parsed struct {
		RateLimit *struct {
			PrimaryWindow   *codexWindow `json:"primary_window"`
			SecondaryWindow *codexWindow `json:"secondary_window"`
			Primary         *codexWindow `json:"primary"`
			Secondary       *codexWindow `json:"secondary"`
		} `json:"rate_limit"`
		RateLimits *struct {
			Primary   *codexWindow `json:"primary"`
			Secondary *codexWindow `json:"secondary"`
		} `json:"rate_limits"`
		PlanType *string `json:"plan_type"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("cpam: codex usage: %w", err)
	}
	if parsed.RateLimit == nil && parsed.RateLimits == nil {
		return nil, nil, fmt.Errorf("cpam: codex usage: no rate-limit object in the payload")
	}
	var primary, weekly *Window
	if parsed.RateLimit != nil {
		primary = firstWindow(parsed.RateLimit.PrimaryWindow, parsed.RateLimit.Primary)
		weekly = firstWindow(parsed.RateLimit.SecondaryWindow, parsed.RateLimit.Secondary)
	} else {
		primary = parsed.RateLimits.Primary.toWindow()
		weekly = parsed.RateLimits.Secondary.toWindow()
	}
	return primary, weekly, nil
}

// firstWindow prefers the live field name and falls back to the documented one.
func firstWindow(live, documented *codexWindow) *Window {
	if live := live.toWindow(); live != nil {
		return live
	}
	return documented.toWindow()
}
