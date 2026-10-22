package cpam

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const authFilesPayload = `{"files":[
 {"provider":"claude","email":"wilber@example.com","auth_index":"claude-idx","disabled":false},
 {"provider":"codex","email":"wilber@example.com","auth_index":"codex-idx","disabled":false},
 {"provider":"antigravity","email":"other@example.com","auth_index":"agy-idx","disabled":false},
 {"provider":"codex","email":"off@example.com","auth_index":"off-idx","disabled":true}
]}`

// live Claude oauth/usage body: utilization fractions (0..1) and ISO resets.
const claudeUsagePayload = `{"five_hour":{"utilization":0.42,"resets_at":"2026-10-22T18:00:00Z"},
"seven_day":{"utilization":0.06,"resets_at":"2026-10-28T16:59:50Z"}}`

// live codex wham/usage body: singular rate_limit with *_window slots.
const codexUsagePayload = `{"email":"wilber@example.com","plan_type":"plus",
"rate_limit":{"allowed":true,"limit_reached":false,
"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":1792690165},
"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":1793125136}}}`

func TestListAccounts_KeepsEnabledClaudeAndCodexOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(authFilesPayload))
	}))
	defer server.Close()

	accounts, err := ListAccounts(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("the disabled codex and the antigravity account are not read: %+v", accounts)
	}
	if accounts[0].Provider != "claude" || accounts[1].Provider != "codex" {
		t.Fatalf("the accounts keep their provider: %+v", accounts)
	}
}

func TestListAccounts_RejectedKeySurfaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid management key"}`))
	}))
	defer server.Close()

	if _, err := ListAccounts(server.URL, "wrong", server.Client()); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("a rejected key is named, not swallowed: %v", err)
	}
}

func TestFetchUsage_ClaudeReadsUtilizationAndResets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status_code":200,"body":` + quote(claudeUsagePayload) + `}`))
	}))
	defer server.Close()

	usage, err := FetchUsage(server.URL, "key", Account{Provider: "claude", Email: "w@x", AuthIndex: "idx"}, server.Client())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if usage.Primary == nil || usage.Primary.UsedPercent != 42 {
		t.Fatalf("the 0..1 utilization becomes a percentage: %+v", usage.Primary)
	}
	if usage.Weekly == nil || usage.Weekly.UsedPercent != 6 {
		t.Fatalf("the weekly window rides: %+v", usage.Weekly)
	}
	want := time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC).Unix()
	if usage.Primary.ResetAt == nil || *usage.Primary.ResetAt != want {
		t.Fatalf("the ISO reset becomes unix seconds: %+v", usage.Primary.ResetAt)
	}
}

func TestFetchUsage_CodexReadsTheLiveShapeAndThePlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status_code":200,"body":` + quote(codexUsagePayload) + `}`))
	}))
	defer server.Close()

	usage, err := FetchUsage(server.URL, "key", Account{Provider: "codex", Email: "w@x", AuthIndex: "idx"}, server.Client())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if usage.Primary == nil || usage.Primary.UsedPercent != 0 {
		t.Fatalf("the primary window parses: %+v", usage.Primary)
	}
	if usage.Primary.WindowMinutes == nil || *usage.Primary.WindowMinutes != 300 {
		t.Fatalf("the 18000s window becomes 300 minutes: %+v", usage.Primary.WindowMinutes)
	}
	if usage.Weekly == nil || usage.Weekly.UsedPercent != 1 || *usage.Weekly.WindowMinutes != 7*24*60 {
		t.Fatalf("the weekly window parses: %+v", usage.Weekly)
	}
	if usage.Primary.ResetAt == nil || *usage.Primary.ResetAt != 1792690165 {
		t.Fatalf("the unix reset passes through: %+v", usage.Primary.ResetAt)
	}
}

func TestParseCodexUsage_AcceptsTheDocumentedShapeToo(t *testing.T) {
	documented := `{"plan_type":"pro","rate_limits":{"primary":{"used_percent":10,"resets_in_seconds":3600},"secondary":{"used_percent":20,"resets_in_seconds":90000}}}`
	primary, weekly, err := parseCodexUsage([]byte(documented))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if primary == nil || primary.UsedPercent != 10 || primary.ResetAt == nil {
		t.Fatalf("the documented primary parses with a derived reset: %+v", primary)
	}
	if weekly == nil || weekly.UsedPercent != 20 {
		t.Fatalf("the documented secondary parses: %+v", weekly)
	}
}

func TestPoll_OneAccountFailingDropsOnlyItsRow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			w.Write([]byte(authFilesPayload))
		case "/v0/management/api-call":
			var request apiCallRequest
			readBody(r, &request)
			if request.AuthIndex == "codex-idx" {
				w.Write([]byte(`{"status_code":500,"body":"boom"}`))
				return
			}
			w.Write([]byte(`{"status_code":200,"body":` + quote(claudeUsagePayload) + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	usage, err := Poll(server.URL, "key", 0, server.Client())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(usage) != 1 || usage[0].Account.Provider != "claude" {
		t.Fatalf("a failing account drops its row, the healthy one stays: %+v", usage)
	}
}

func TestLimits_CredentialCommandsAndBaseURLResolve(t *testing.T) {
	Reset()
	origFetchAll, origGetenv, origRunner := fetchAll, getenv, commandRunner
	defer func() { fetchAll, getenv, commandRunner = origFetchAll, origGetenv, origRunner }()

	getenv = func(name string) string {
		switch name {
		case "CPAM_BASE_URL":
			return "https://cpam.example"
		case "CPAM_MANAGEMENT_KEY_COMMAND":
			return "print-management-key"
		}
		return ""
	}
	commandRunner = func(command string) ([]byte, error) { return []byte("resolved-key\n"), nil }
	key, err := ResolveAPIKey(getenv)
	if err != nil || key != "resolved-key" {
		t.Fatalf("the command key is trimmed: %q %v", key, err)
	}

	calls := 0
	var seenBase, seenKey string
	fetchAll = func(baseURL, runKey string, _ int64, _ *http.Client) ([]AccountUsage, error) {
		calls++
		seenBase, seenKey = baseURL, runKey
		return nil, errors.New("offline")
	}
	if rows := Limits(0); rows != nil {
		t.Fatalf("an unreachable gateway shows no stale block on a cold start: %+v", rows)
	}
	if calls != 1 || seenBase != "https://cpam.example" || seenKey != "resolved-key" {
		t.Fatalf("the base URL and key reach the poll: %q %q %d", seenBase, seenKey, calls)
	}
}

func TestLimits_NoCredentialMeansNoBlock(t *testing.T) {
	Reset()
	origGetenv := getenv
	getenv = func(string) string { return "" }
	defer func() { getenv = origGetenv }()

	if rows := Limits(0); rows != nil {
		t.Fatalf("a machine without CPAM credentials shows no cpam block: %+v", rows)
	}
}

func TestDisplayRows_GroupAndPlanCarry(t *testing.T) {
	usage := []AccountUsage{
		{
			Account: Account{Provider: "codex", Email: "w@x", PlanType: "plus"},
			Primary: &Window{UsedPercent: 3, ResetAt: int64Ptr(1792690165), WindowMinutes: intPtr(300)},
			Weekly:  &Window{UsedPercent: 1, ResetAt: int64Ptr(1793125136), WindowMinutes: intPtr(10080)},
		},
	}
	rows := displayRows(usage, 1234)
	if len(rows) != 1 {
		t.Fatalf("one row per account: %d", len(rows))
	}
	row := rows[0]
	if row.GroupLabel != "CPAM" || row.AccountLabel != "codex · w@x" {
		t.Fatalf("the row names provider and email under the group: %+v", row)
	}
	if row.PlanType == nil || *row.PlanType != "plus" {
		t.Fatalf("the plan type rides: %+v", row.PlanType)
	}
	if row.Primary == nil || *row.Primary.WindowMinutes != 300 || *row.Primary.ResetsAt != 1792690165 {
		t.Fatalf("the primary window carries minutes and reset: %+v", row.Primary)
	}
	if row.Secondary == nil || *row.Secondary.WindowMinutes != 10080 {
		t.Fatalf("the weekly window carries minutes: %+v", row.Secondary)
	}
}

// quote wraps a raw upstream body as the api-call JSON string field.
func quote(body string) string {
	encoded, _ := json.Marshal(body)
	return string(encoded)
}

func readBody(r *http.Request, into any) {
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, into)
}

func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }
