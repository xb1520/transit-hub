package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestFetchSub2APISelfBalanceHistory_UsesPaymentAndRedeem(t *testing.T) {
	var hitPayment, hitRedeem, hitWrong bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/payment/orders/my":
			hitPayment = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"total": 2,
					"items": []any{
						map[string]any{
							"id": 1, "amount": 50.0, "status": "completed",
							"order_type": "balance", "payment_type": "alipay",
							"out_trade_no": "T1", "created_at": "2026-08-01T10:00:00Z",
						},
						map[string]any{
							"id": 2, "amount": 10.0, "status": "pending", // 未支付，应过滤
							"order_type": "balance", "created_at": "2026-08-01T11:00:00Z",
						},
					},
				},
			})
		case "/api/v1/redeem/history":
			hitRedeem = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": []any{
					map[string]any{
						"id": 9, "type": "balance", "value": 20.0, "code": "ABC",
						"used_at": "2026-08-01T12:00:00Z",
					},
					map[string]any{
						"id": 10, "type": "concurrency", "value": 5.0, // 非余额，应过滤
					},
				},
			})
		case "/api/v1/user/balance-history":
			hitWrong = true
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "tok", TokenType: "Bearer"}
	history, err := service.FetchSiteBalanceHistory(session, 1, 50)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !hitPayment || !hitRedeem {
		t.Fatalf("expected payment and redeem hits, payment=%v redeem=%v", hitPayment, hitRedeem)
	}
	if hitWrong {
		t.Fatal("should not call non-existent user balance-history")
	}
	if len(history.Items) != 2 {
		t.Fatalf("want 2 items (1 paid order + 1 redeem), got %d: %+v", len(history.Items), history.Items)
	}
	if history.Items[0].ID != "redeem:9" {
		t.Fatalf("first item should be redeem, got %s", history.Items[0].ID)
	}
	if history.Items[1].ID != "pay:1" {
		t.Fatalf("second item should be pay, got %s", history.Items[1].ID)
	}
}

func TestFetchNewAPISelfTopupHistory_PaginatesAll(t *testing.T) {
	// 构造 total > page_size 以触发第二页（实现固定 page_size=100）
	page1Items := make([]any, 0, 100)
	for i := 1; i <= 100; i++ {
		page1Items = append(page1Items, map[string]any{
			"id": i, "money": 1.0, "status": "success",
			"trade_no":    "T" + strconv.Itoa(i),
			"create_time": float64(1712345678 - i),
		})
	}
	var hitPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/topup/self" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		p := r.URL.Query().Get("p")
		hitPages = append(hitPages, p)
		if p == "1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"page": 1, "page_size": 100, "total": 101,
					"items": page1Items,
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"page": 2, "page_size": 100, "total": 101,
				"items": []any{
					map[string]any{
						"id": 101, "money": 3.0, "status": "success",
						"trade_no": "T101", "create_time": float64(1712000000),
					},
					map[string]any{
						"id": 102, "money": 9.0, "status": "pending", // 过滤
					},
				},
			},
		})
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "s=1", UserID: "1", QuotaPerUnit: 500000}
	history, err := service.FetchSiteBalanceHistory(session, 1, 50)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if history.Platform != PlatformNewAPI {
		t.Fatalf("platform = %s", history.Platform)
	}
	if len(history.Items) != 101 {
		t.Fatalf("want 101 success topups, got %d", len(history.Items))
	}
	if len(hitPages) < 2 {
		t.Fatalf("expected multi-page fetch, pages=%v", hitPages)
	}
}

func TestParseSub2APIPaymentOrderItem_FiltersUnpaid(t *testing.T) {
	_, ok := parseSub2APIPaymentOrderItem(map[string]any{"id": 1, "amount": 10.0, "status": "pending"})
	if ok {
		t.Fatal("pending should be filtered")
	}
	item, ok := parseSub2APIPaymentOrderItem(map[string]any{"id": 1, "amount": 10.0, "status": "paid"})
	if !ok || item.Amount == nil || *item.Amount != 10 {
		t.Fatalf("paid should parse: %+v ok=%v", item, ok)
	}
}
