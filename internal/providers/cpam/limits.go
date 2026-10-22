/**
 * The panel-facing view of the CPAM accounts' quotas.
 *
 * One display row per enabled claude/codex account behind the gateway,
 * grouped under "CPAM" the way multiple Claude accounts group. The poll is
 * throttled well below the panel's 15s refresh ticker (one auth-files read
 * plus one upstream usage call per account), and a failed poll keeps the
 * last valid snapshot rather than blanking the block.
 */
package cpam

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/senna-lang/herdr-agent-usage/internal/limitscore"
)

// MinPollIntervalMs is the smallest gap between two management polls. The
// upstream usage endpoints are real plan reads, so this stays conservative.
const MinPollIntervalMs = 5 * 60 * 1000

// commandRunner runs the credential command; tests stub it.
var commandRunner = func(command string) ([]byte, error) {
	return exec.Command("/bin/sh", "-c", command).Output()
}

// ResolveAPIKey returns the management key: CPAM_MANAGEMENT_KEY verbatim,
// else the trimmed output of the command CPAM_MANAGEMENT_KEY_COMMAND names.
// The command form keeps the key inside a secret store instead of a config
// file or the process environment.
func ResolveAPIKey(getenv func(string) string) (string, error) {
	if key := strings.TrimSpace(getenv("CPAM_MANAGEMENT_KEY")); key != "" {
		return key, nil
	}
	command := strings.TrimSpace(getenv("CPAM_MANAGEMENT_KEY_COMMAND"))
	if command == "" {
		return "", errors.New("cpam: neither CPAM_MANAGEMENT_KEY nor CPAM_MANAGEMENT_KEY_COMMAND is set")
	}
	output, err := commandRunner(command)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(output))
	if key == "" {
		return "", errors.New("cpam: the management key command printed nothing")
	}
	return key, nil
}

// ResolveBaseURL returns the gateway base URL: CPAM_BASE_URL, else the
// local-loopback default a stock CLI Proxy API serves.
func ResolveBaseURL(getenv func(string) string) string {
	if base := strings.TrimSpace(getenv("CPAM_BASE_URL")); base != "" {
		return base
	}
	return DefaultBaseURL
}

// getenv is the indirection tests use instead of touching the process env.
var getenv = os.Getenv

// fetchAll is the indirection tests use instead of the network.
var fetchAll = func(baseURL, key string, nowMs int64, client *http.Client) ([]AccountUsage, error) {
	return Poll(baseURL, key, nowMs, client)
}

// snapshot is the latest successful poll, guarded for the panel's concurrent
// refresh paths.
type snapshot struct {
	mu        sync.Mutex
	usage     []AccountUsage
	fetchedAt int64
}

var current snapshot

// Reset drops the cached snapshot. Tests call this; production never does.
func Reset() {
	current.mu.Lock()
	defer current.mu.Unlock()
	current.usage = nil
	current.fetchedAt = 0
}

// Limits returns one display row per enabled claude/codex account, or nil
// when no credential is configured. A fresh-enough cached snapshot is
// returned as is; a failed poll keeps the previous snapshot.
func Limits(nowMs int64) []limitscore.ProviderLimits {
	key, err := ResolveAPIKey(getenv)
	if err != nil {
		return nil
	}
	baseURL := ResolveBaseURL(getenv)

	current.mu.Lock()
	defer current.mu.Unlock()
	if current.usage == nil || nowMs-current.fetchedAt >= MinPollIntervalMs {
		if usage, pollErr := fetchAll(baseURL, key, nowMs, nil); pollErr == nil {
			current.usage = usage
			current.fetchedAt = nowMs
		}
	}
	if len(current.usage) == 0 {
		return nil
	}
	return displayRows(current.usage, current.fetchedAt)
}

// Poll reads every enabled claude/codex account's upstream usage. One
// account that fails to read drops only its own row: a quota the proxy
// cannot see right now is unknown, not zero.
func Poll(baseURL, key string, nowMs int64, client *http.Client) ([]AccountUsage, error) {
	accounts, err := ListAccounts(baseURL, key, client)
	if err != nil {
		return nil, err
	}
	var out []AccountUsage
	for _, account := range accounts {
		usage, err := FetchUsage(baseURL, key, account, client)
		if err != nil {
			continue
		}
		out = append(out, usage)
	}
	return out, nil
}

// displayRows converts one poll into panel rows: the account's provider and
// email name each row inside the shared "CPAM" group.
func displayRows(usage []AccountUsage, fetchedAtMs int64) []limitscore.ProviderLimits {
	rows := make([]limitscore.ProviderLimits, 0, len(usage))
	for _, account := range usage {
		row := limitscore.ProviderLimits{
			ProviderID:   "cpam",
			Label:        account.Account.Provider,
			AccountLabel: account.ProviderAccountLabel(),
			GroupLabel:   "CPAM",
			Source:       "cpam",
			FetchedAtMs:  fetchedAtMs,
			Primary:      toLimitWindow(account.Primary),
		}
		if account.Weekly != nil {
			row.Secondary = toLimitWindow(account.Weekly)
		}
		if account.Account.PlanType != "" {
			plan := account.Account.PlanType
			row.PlanType = &plan
		}
		rows = append(rows, row)
	}
	return rows
}

// toLimitWindow carries the upstream's own window length so the formatter's
// tag reads what the plan actually enforces.
func toLimitWindow(window *Window) *limitscore.LimitWindow {
	if window == nil {
		return nil
	}
	out := limitscore.LimitWindow{UsedPercentage: window.UsedPercent}
	if window.ResetAt != nil {
		seconds := *window.ResetAt
		out.ResetsAt = &seconds
	}
	if window.WindowMinutes != nil {
		minutes := *window.WindowMinutes
		out.WindowMinutes = &minutes
	}
	return &out
}
