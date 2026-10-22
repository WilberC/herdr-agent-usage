/**
 * The antigravity adapter: one retrieveUserQuotaSummary call per account,
 * mirrored from the CPA panel's own data path.
 *
 * The body is {project} — NOT {metadata} — and the hosts are tried in the
 * order daily -> daily-sandbox -> prod: the same account reports DIFFERENT
 * quota per host environment, and Antigravity traffic is served by the daily
 * host, so it is authoritative. A credential whose auth-files entry carries
 * no project_id discovers it through loadCodeAssist first, falling back to
 * the legacy metadata call on the prod host.
 */
package cpam

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var antigravityHosts = []string{
	"https://daily-cloudcode-pa.googleapis.com",
	"https://daily-cloudcode-pa.sandbox.googleapis.com",
	"https://cloudcode-pa.googleapis.com",
}

const antigravityUserAgent = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"

func antigravityHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    antigravityUserAgent,
	}
}

func fetchAntigravityUsage(baseURL, key string, account Account, client *http.Client) (AccountUsage, error) {
	usage := AccountUsage{Account: account}
	project := account.ProjectID
	if project == "" {
		discovered, err := discoverAntigravityProject(baseURL, key, account.AuthIndex, client)
		if err != nil {
			return usage, err
		}
		if discovered == "" {
			// No project anywhere: the legacy prod-host call is the shape a
			// plain gemini-cli credential answers.
			return antigravityQuotaCall(baseURL, key, account, antigravityHosts[len(antigravityHosts)-1], "", client)
		}
		project = discovered
	}
	var lastErr error
	for _, host := range antigravityHosts {
		usage, err := antigravityQuotaCall(baseURL, key, account, host, project, client)
		if err == nil {
			return usage, nil
		}
		lastErr = err
	}
	return AccountUsage{Account: account}, lastErr
}

// discoverAntigravityProject reads the account's cloudaicompanionProject the
// way the IDE registration does.
func discoverAntigravityProject(baseURL, key, authIndex string, client *http.Client) (string, error) {
	body, err := apiCall(baseURL, key, apiCallRequest{
		AuthIndex: authIndex,
		Method:    http.MethodPost,
		URL:       antigravityHosts[0] + "/v1internal:loadCodeAssist",
		Header:    antigravityHeaders(),
		Body:      `{"metadata":{"ideType":"ANTIGRAVITY"}}`,
	}, client)
	if err != nil {
		return "", err
	}
	var parsed struct {
		CloudaicompanionProject any `json:"cloudaicompanionProject"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("cpam: antigravity project discovery: %w", err)
	}
	switch value := parsed.CloudaicompanionProject.(type) {
	case string:
		return value, nil
	case map[string]any:
		if id, ok := value["id"].(string); ok {
			return id, nil
		}
	}
	return "", nil
}

// antigravityQuotaCall reads one quota summary and normalizes its buckets.
func antigravityQuotaCall(baseURL, key string, account Account, host, project string, client *http.Client) (AccountUsage, error) {
	empty := AccountUsage{Account: account}
	bodyText := `{"metadata":{"ideType":"ANTIGRAVITY"}}`
	if project != "" {
		encoded, _ := json.Marshal(map[string]string{"project": project})
		bodyText = string(encoded)
	}
	body, err := apiCall(baseURL, key, apiCallRequest{
		AuthIndex: account.AuthIndex,
		Method:    http.MethodPost,
		URL:       host + "/v1internal:retrieveUserQuotaSummary",
		Header:    antigravityHeaders(),
		Body:      bodyText,
	}, client)
	if err != nil {
		return empty, err
	}
	primary, weekly, err := parseAntigravityUsage(body)
	if err != nil {
		return empty, err
	}
	return AccountUsage{Account: account, Primary: primary, Weekly: weekly}, nil
}

// parseAntigravityUsage reads groups[].buckets[]: remainingFraction is 0..1
// and each bucket names its window. One account can carry several model
// groups; the binding (lowest remaining) bucket of each window slot is the
// account's quota status, so a collapsed row always reads like the worst
// window the account actually faces.
func parseAntigravityUsage(body []byte) (*Window, *Window, error) {
	var parsed struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Buckets     []struct {
				RemainingFraction *float64 `json:"remainingFraction"`
				Window            string   `json:"window"`
				ResetTime         string   `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("cpam: antigravity usage: %w", err)
	}
	var primary, weekly *Window
	for _, group := range parsed.Groups {
		for _, bucket := range group.Buckets {
			if bucket.RemainingFraction == nil {
				continue
			}
			window := &Window{UsedPercent: (1 - *bucket.RemainingFraction) * 100}
			if when, err := time.Parse(time.RFC3339, bucket.ResetTime); err == nil {
				seconds := when.Unix()
				window.ResetAt = &seconds
			}
			switch bucket.Window {
			case "5h":
				if primary == nil || window.UsedPercent > primary.UsedPercent {
					primary = window
				}
			case "weekly":
				if weekly == nil || window.UsedPercent > weekly.UsedPercent {
					weekly = window
				}
			}
		}
	}
	return primary, weekly, nil
}
