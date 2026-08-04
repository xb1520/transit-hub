package my_sites

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"transithub/backend/internal/modules/upstream"
)

func writeConnectionTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func platformTestSession(platform upstream.Platform, baseURL string) upstream.Session {
	if platform == upstream.PlatformNewAPI {
		return upstream.Session{Platform: platform, BaseURL: baseURL, Cookie: "session=test", UserID: "1"}
	}
	return upstream.Session{Platform: platform, BaseURL: baseURL, AccessToken: "access-token", TokenType: "Bearer"}
}

func TestBuildAccountPayload_DockingDefaults(t *testing.T) {
	payload := buildAccountPayload("openai", "https://upstream.example", "sk-test", []int{7}, "A-【site】-0.12x", 5, 0.12)

	if payload["platform"] != "openai" {
		t.Fatalf("platform = %v, want openai", payload["platform"])
	}
	if payload["concurrency"] != 5 {
		t.Fatalf("concurrency = %v, want 5", payload["concurrency"])
	}
	if payload["rate_multiplier"] != 0.12 {
		t.Fatalf("rate_multiplier = %v, want 0.12", payload["rate_multiplier"])
	}
	if _, ok := payload["extra"]; ok {
		t.Fatalf("extra should not be set (no auto passthrough), got %#v", payload["extra"])
	}
	creds, _ := payload["credentials"].(map[string]any)
	if creds["pool_mode"] != true {
		t.Fatalf("pool_mode = %v, want true", creds["pool_mode"])
	}
	if creds["pool_mode_retry_count"] != defaultSub2APIPoolModeRetryCount {
		t.Fatalf("pool_mode_retry_count = %v, want %d", creds["pool_mode_retry_count"], defaultSub2APIPoolModeRetryCount)
	}

	// 非法并发回落默认值，且不写透传开关。
	anthropic := buildAccountPayload("anthropic", "https://upstream.example", "sk-test", []int{1}, "B-name", 0, 1.0)
	if anthropic["rate_multiplier"] != 1.0 {
		t.Fatalf("anthropic rate_multiplier = %v, want 1.0", anthropic["rate_multiplier"])
	}
	if _, ok := anthropic["extra"]; ok {
		t.Fatalf("anthropic extra should not enable passthrough, got %#v", anthropic["extra"])
	}
	if anthropic["concurrency"] != defaultSub2APIAccountConcurrency {
		t.Fatalf("anthropic concurrency = %v, want %d", anthropic["concurrency"], defaultSub2APIAccountConcurrency)
	}
}

func TestEffectiveAccountRateMultiplier(t *testing.T) {
	raw := 1.0
	if got := effectiveAccountRateMultiplier(&raw, 0.12); got != 0.12 {
		t.Fatalf("1.0 * 0.12 = %v, want 0.12", got)
	}
	raw = 0.12
	if got := effectiveAccountRateMultiplier(&raw, 1); got != 0.12 {
		t.Fatalf("0.12 * 1 = %v, want 0.12", got)
	}
	if got := effectiveAccountRateMultiplier(nil, 0.12); got != 0.12 {
		t.Fatalf("nil * 0.12 = %v, want 0.12", got)
	}
	if got := effectiveAccountRateMultiplier(nil, 0); got != 1.0 {
		t.Fatalf("nil * invalid recharge = %v, want 1.0", got)
	}
}

func TestResolveGroupInfo_MatchesNameFallback(t *testing.T) {
	rate := 0.12
	platform := "openai"
	groups := []upstream.GroupInfo{{
		ID: "9", Name: "分组2", Platform: &platform, Multiplier: &rate, MultiplierDisplay: "0.12x",
	}}
	gotType, gotDisplay, gotRate := resolveGroupInfo(groups, "", "分组2")
	if gotType != "openai" || gotDisplay != "0.12x" || gotRate == nil || *gotRate != 0.12 {
		t.Fatalf("resolve by name failed: type=%q display=%q rate=%v", gotType, gotDisplay, gotRate)
	}
}

func TestCreateAdminResource_UsesUpstreamConcurrencyAndEffectiveRate(t *testing.T) {
	var createdPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"concurrency": 5, "balance": 10}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts":
			_ = json.NewDecoder(r.Body).Decode(&createdPayload)
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 99}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/99/models/sync-upstream":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"models": []string{"gpt-5.4"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/bulk-update":
			writeConnectionTestJSON(w, map[string]any{"success": true})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	rawRate := 1.0
	service := &Service{platformService: upstream.NewPlatformService(upstream.NewHTTPClient(server.Client()))}
	adminSession := platformTestSession(upstream.PlatformSub2API, server.URL)
	upstreamSession := platformTestSession(upstream.PlatformSub2API, server.URL)
	connectionCtx := connectionContext{
		state:           &State{Session: adminSession},
		upstreamSite:    &upstream.Site{Name: "玉面老白龙", BaseURL: "https://provider.example", RechargeRate: 0.12},
		upstreamSession: upstreamSession,
		groupType:       "openai",
		groupName:       "分组2",
		multiplierLabel: "1x",
		rateMultiplier:  &rawRate,
	}
	id, name, err := service.createAdminResource(connectionCtx, 0, []string{"7"}, "sk-upstream")
	if err != nil {
		t.Fatalf("createAdminResource: %v", err)
	}
	if id != "99" {
		t.Fatalf("id = %q, want 99", id)
	}
	if !strings.Contains(name, "0.12x") {
		t.Fatalf("name = %q, want effective rate 0.12x in suffix", name)
	}
	if createdPayload["concurrency"] != float64(5) {
		t.Fatalf("concurrency = %v, want 5 (upstream user concurrency)", createdPayload["concurrency"])
	}
	if createdPayload["rate_multiplier"] != 0.12 {
		t.Fatalf("rate_multiplier = %v, want 0.12 (1.0 * recharge 0.12)", createdPayload["rate_multiplier"])
	}
	if _, ok := createdPayload["extra"]; ok {
		t.Fatalf("must not enable passthrough extra: %#v", createdPayload["extra"])
	}
}

func TestManagedConnectionOperationsUseEachSidePlatform(t *testing.T) {
	var mu sync.Mutex
	var tokenName string
	var channelName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/keys":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 11, "key": "sk-sub2"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"role": "user", "concurrency": 5}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/groups/available":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": 7, "name": "vip", "platform": "openai", "rate_multiplier": 1.0}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/groups/rates":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 22}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/22/models/sync-upstream":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"models": []string{"gpt-5.4"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/bulk-update":
			writeConnectionTestJSON(w, map[string]any{"success": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/token/":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			tokenName, _ = body["name"].(string)
			mu.Unlock()
			writeConnectionTestJSON(w, map[string]any{"success": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/token/":
			mu.Lock()
			name := tokenName
			mu.Unlock()
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": 33, "name": name}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/token/33/key":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"key": "sk-newapi"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/channel/":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			channel, _ := body["channel"].(map[string]any)
			mu.Lock()
			channelName, _ = channel["name"].(string)
			mu.Unlock()
			writeConnectionTestJSON(w, map[string]any{"success": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/channel/":
			mu.Lock()
			name := channelName
			mu.Unlock()
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": 44, "name": name}}, "total": 1})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	service := &Service{platformService: upstream.NewPlatformService(upstream.NewHTTPClient(server.Client()))}
	platforms := []upstream.Platform{upstream.PlatformSub2API, upstream.PlatformNewAPI}
	for _, upstreamPlatform := range platforms {
		for _, adminPlatform := range platforms {
			name := string(upstreamPlatform) + "-to-" + string(adminPlatform)
			t.Run(name, func(t *testing.T) {
				upstreamSession := platformTestSession(upstreamPlatform, server.URL)
				keyID, key, err := service.createUpstreamCredential(upstreamSession, "credential-"+name, "7")
				if err != nil {
					t.Fatalf("create upstream credential: %v", err)
				}
				if keyID == "" || key == "" {
					t.Fatalf("missing upstream credential: id=%q key=%q", keyID, key)
				}

				adminSession := platformTestSession(adminPlatform, server.URL)
				connectionCtx := connectionContext{
					state:        &State{Session: adminSession},
					upstreamSite: &upstream.Site{Name: "source", BaseURL: "https://provider.example"},
					groupType:    "openai", groupName: "vip", multiplierLabel: "1.2x",
				}
				resourceID, _, err := service.createAdminResource(connectionCtx, 1, []string{"7"}, key)
				if err != nil {
					t.Fatalf("create admin resource: %v", err)
				}
				if resourceID == "" {
					t.Fatal("missing admin resource id")
				}
			})
		}
	}
}

func TestRealConnectCompensatesRemoteResourcesWhenPersistenceFails(t *testing.T) {
	var deletedAccount bool
	var deletedKey bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"role": "admin", "concurrency": 5}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/groups":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": 7, "name": "vip", "platform": "openai", "status": "active"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/groups/available":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": 7, "name": "vip", "platform": "openai", "rate_multiplier": 1.0}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/groups/rates":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/keys":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 11, "key": "sk-created"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 22}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/22/models/sync-upstream":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"models": []string{"gpt-5.4"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/bulk-update":
			writeConnectionTestJSON(w, map[string]any{"success": true})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/admin/accounts/22":
			deletedAccount = true
			writeConnectionTestJSON(w, map[string]any{"success": true})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/keys/11":
			deletedKey = true
			writeConnectionTestJSON(w, map[string]any{"success": true})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	session := platformTestSession(upstream.PlatformSub2API, server.URL)
	stateRepo := &testStateRepo{state: &State{UserID: "user-1", AdminAccountID: "admin-1", Session: session, Mappings: []GroupMapping{}}}
	connRepo := &testConnRepo{stateRepo: stateRepo, saveErr: errors.New("database unavailable")}
	lookup := testUpstreamLookup{sites: map[string]*upstream.Site{
		"site-1": {
			ID: "site-1", UserID: "user-1", AdminAccountID: "admin-1", Name: "source",
			BaseURL: server.URL, Platform: upstream.PlatformSub2API, Session: &session,
			Metrics: upstream.Metrics{Groups: []upstream.GroupInfo{{ID: "7", Name: "vip", Platform: stringPointer("openai")}}},
		},
	}}
	service := NewService(stateRepo, upstream.NewPlatformService(upstream.NewHTTPClient(server.Client())), lookup)
	service.SetAdminAccountResolver(testAdminResolver{currentID: "admin-1"})
	service.connRepository = connRepo
	addToPricing := false

	_, err := service.RealConnect(context.Background(), "user-1", RealConnectRequest{
		UpstreamSiteID: "site-1", UpstreamGroupID: "7", UpstreamGroupName: "vip",
		GroupType: "openai", OwnGroupIDs: []string{"7"}, AddToPricingMapping: &addToPricing,
	})
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("expected persistence error, got %v", err)
	}
	if !deletedAccount || !deletedKey {
		t.Fatalf("expected both compensating deletes, account=%t key=%t", deletedAccount, deletedKey)
	}
}

func stringPointer(value string) *string {
	return &value
}
