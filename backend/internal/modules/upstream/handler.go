package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"transithub/backend/internal/shared/authctx"
	"transithub/backend/internal/shared/httpjson"
)

type Handler struct {
	service  *Service
	accounts HandlerAccountResolver
}

// HandlerAccountResolver 由 handler 用来在请求路径上解析当前工作区。
type HandlerAccountResolver interface {
	RequireCurrentID(ctx context.Context, userID string) (string, error)
}

func RegisterRoutes(mux *http.ServeMux, service *Service, accounts HandlerAccountResolver) {
	handler := &Handler{service: service, accounts: accounts}
	mux.HandleFunc("GET /api/upstream-sites", handler.list)
	mux.HandleFunc("POST /api/upstream-sites", handler.create)
	mux.HandleFunc("POST /api/upstream-sites/sync-all", handler.syncAll)
	mux.HandleFunc("GET /api/upstream-sites/sync-stream", handler.syncStream)
	mux.HandleFunc("GET /api/upstream-sites/", handler.getSubroutes)
	mux.HandleFunc("PUT /api/upstream-sites/", handler.update)
	mux.HandleFunc("PATCH /api/upstream-sites/", handler.update)
	mux.HandleFunc("POST /api/upstream-sites/", handler.postSubroutes)
	mux.HandleFunc("DELETE /api/upstream-sites/", handler.remove)
}

func (h *Handler) syncAll(w http.ResponseWriter, r *http.Request) {
	userID, ok := authctx.UserID(r.Context())
	if !ok {
		httpjson.WriteError(w, http.StatusUnauthorized, "auth.errors.unauthorized")
		return
	}
	if _, err := h.requireWorkspace(r.Context(), userID); err != nil {
		writeWorkspaceError(w, err)
		return
	}
	responses, err := h.service.SyncAll(r.Context(), userID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, responses)
}

// syncStream 以 SSE 流方式逐站同步，每个站点的进度实时推送到前端。
// 与 syncAll 的区别：站点按顺序处理（不并发），遇 Cloudflare 自动重试，
// 结果逐个流式返回而非等所有站点完成后一次性返回。
func (h *Handler) syncStream(w http.ResponseWriter, r *http.Request) {
	userID, ok := authctx.UserID(r.Context())
	if !ok {
		httpjson.WriteError(w, http.StatusUnauthorized, "auth.errors.unauthorized")
		return
	}
	if _, err := h.requireWorkspace(r.Context(), userID); err != nil {
		writeWorkspaceError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpjson.WriteError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	emit := func(event SyncEvent) {
		data, err := json.Marshal(event)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	if err := h.service.SyncAllStream(r.Context(), userID, emit); err != nil {
		return
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := authctx.UserID(r.Context())
	if !ok {
		httpjson.WriteError(w, http.StatusUnauthorized, "auth.errors.unauthorized")
		return
	}
	if _, err := h.requireWorkspace(r.Context(), userID); err != nil {
		writeWorkspaceError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, h.service.List(r.Context(), userID))
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := authctx.UserID(r.Context())
	if !ok {
		httpjson.WriteError(w, http.StatusUnauthorized, "auth.errors.unauthorized")
		return
	}
	if _, err := h.requireWorkspace(r.Context(), userID); err != nil {
		writeWorkspaceError(w, err)
		return
	}
	var dto CreateRequest
	if err := httpjson.Decode(r, &dto); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	response, err := h.service.Create(r.Context(), userID, dto)
	if err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpjson.Write(w, http.StatusCreated, response)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	userID, ok := authctx.UserID(r.Context())
	if !ok {
		httpjson.WriteError(w, http.StatusUnauthorized, "auth.errors.unauthorized")
		return
	}
	if _, err := h.requireWorkspace(r.Context(), userID); err != nil {
		writeWorkspaceError(w, err)
		return
	}

	// PATCH /api/upstream-sites/{id}/settings → 站点级配置
	if id, ok := pathID(r.URL.Path, "/api/upstream-sites/", "/settings"); ok && r.Method == http.MethodPatch {
		h.updateSettings(w, r, userID, id)
		return
	}

	id, ok := pathID(r.URL.Path, "/api/upstream-sites/", "")
	if !ok {
		httpjson.WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	var dto UpdateRequest
	if err := httpjson.Decode(r, &dto); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	response, err := h.service.Update(r.Context(), userID, id, dto)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, response)
}

// updateSettings 更新单个站点的预警覆盖配置（余额阈值）。
func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request, userID, siteID string) {
	var dto SiteSettings
	if err := httpjson.Decode(r, &dto); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	response, err := h.service.UpdateSettings(r.Context(), userID, siteID, dto)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, response)
}

func (h *Handler) getSubroutes(w http.ResponseWriter, r *http.Request) {
	userID, ok := authctx.UserID(r.Context())
	if !ok {
		httpjson.WriteError(w, http.StatusUnauthorized, "auth.errors.unauthorized")
		return
	}
	if _, err := h.requireWorkspace(r.Context(), userID); err != nil {
		writeWorkspaceError(w, err)
		return
	}
	id, rest, ok := pathIDWithRest(r.URL.Path, "/api/upstream-sites/")
	if !ok {
		httpjson.WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	switch rest {
	case "settlements":
		h.listSettlements(w, r, userID, id)
	case "settlement-summary":
		h.settlementSummary(w, r, userID, id)
	case "ledger":
		h.listLedger(w, r, userID, id)
	case "recharge-candidates":
		h.listRechargeCandidates(w, r, userID, id)
	default:
		httpjson.WriteError(w, http.StatusNotFound, "Not Found")
	}
}

func (h *Handler) postSubroutes(w http.ResponseWriter, r *http.Request) {
	userID, ok := authctx.UserID(r.Context())
	if !ok {
		httpjson.WriteError(w, http.StatusUnauthorized, "auth.errors.unauthorized")
		return
	}
	if _, err := h.requireWorkspace(r.Context(), userID); err != nil {
		writeWorkspaceError(w, err)
		return
	}
	// 兼容旧路径：POST .../sync
	if id, ok := pathID(r.URL.Path, "/api/upstream-sites/", "/sync"); ok {
		response, err := h.service.Sync(r.Context(), userID, id)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		httpjson.Write(w, http.StatusCreated, response)
		return
	}
	id, rest, ok := pathIDWithRest(r.URL.Path, "/api/upstream-sites/")
	if !ok {
		httpjson.WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	if rest == "settlements" {
		h.createSettlement(w, r, userID, id)
		return
	}
	if strings.HasPrefix(rest, "settlements/") && strings.HasSuffix(rest, "/void") {
		recordID := strings.TrimSuffix(strings.TrimPrefix(rest, "settlements/"), "/void")
		recordID = strings.Trim(recordID, "/")
		h.voidSettlement(w, r, userID, id, recordID)
		return
	}
	if rest == "ledger/mark" {
		h.markRecharge(w, r, userID, id)
		return
	}
	if rest == "ledger/subscription-topup" {
		h.createSubscriptionTopup(w, r, userID, id)
		return
	}
	httpjson.WriteError(w, http.StatusNotFound, "Not Found")
}

func (h *Handler) listSettlements(w http.ResponseWriter, r *http.Request, userID, siteID string) {
	items, err := h.service.ListSettlements(r.Context(), userID, siteID, 50)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createSettlement(w http.ResponseWriter, r *http.Request, userID, siteID string) {
	var input CreateSettlementInput
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	item, err := h.service.CreateSettlement(r.Context(), userID, siteID, input)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, item)
}

func (h *Handler) voidSettlement(w http.ResponseWriter, r *http.Request, userID, siteID, recordID string) {
	item, err := h.service.VoidSettlement(r.Context(), userID, siteID, recordID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, item)
}

func (h *Handler) deleteSettlement(w http.ResponseWriter, r *http.Request, userID, siteID, recordID string) {
	if err := h.service.DeleteSettlement(r.Context(), userID, siteID, recordID); err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"success": true})
}

func (h *Handler) createSubscriptionTopup(w http.ResponseWriter, r *http.Request, userID, siteID string) {
	var input CreateSubscriptionTopupInput
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	item, err := h.service.CreateSubscriptionTopup(r.Context(), userID, siteID, input)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, item)
}

func (h *Handler) settlementSummary(w http.ResponseWriter, r *http.Request, userID, siteID string) {
	summary, err := h.service.SettlementSummaryForSite(r.Context(), userID, siteID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, summary)
}

func (h *Handler) listLedger(w http.ResponseWriter, r *http.Request, userID, siteID string) {
	items, err := h.service.ListLedger(r.Context(), userID, siteID, 50)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listRechargeCandidates(w http.ResponseWriter, r *http.Request, userID, siteID string) {
	page := 1
	pageSize := 20
	if v := strings.TrimSpace(r.URL.Query().Get("page")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("page_size")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	} else if v := strings.TrimSpace(r.URL.Query().Get("pageSize")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	resp, err := h.service.ListRechargeCandidates(r.Context(), userID, siteID, page, pageSize)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) markRecharge(w http.ResponseWriter, r *http.Request, userID, siteID string) {
	var input MarkRechargeInput
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	item, err := h.service.MarkRecharge(r.Context(), userID, siteID, input)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, item)
}

// pathIDWithRest 解析 /prefix/{id}/{rest...}，rest 不含前导斜杠。
func pathIDWithRest(path string, prefix string) (id string, rest string, ok bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(path, prefix)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, "/", 2)
	id = parts[0]
	if id == "" || id == "sync-all" || id == "sync-stream" {
		return "", "", false
	}
	if len(parts) == 1 {
		return id, "", true
	}
	return id, parts[1], true
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	userID, ok := authctx.UserID(r.Context())
	if !ok {
		httpjson.WriteError(w, http.StatusUnauthorized, "auth.errors.unauthorized")
		return
	}
	if _, err := h.requireWorkspace(r.Context(), userID); err != nil {
		writeWorkspaceError(w, err)
		return
	}
	id, rest, ok := pathIDWithRest(r.URL.Path, "/api/upstream-sites/")
	if !ok {
		httpjson.WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	if rest == "" {
		if err := h.service.Remove(r.Context(), userID, id); err != nil {
			writeUpstreamError(w, err)
			return
		}
		httpjson.Write(w, http.StatusOK, map[string]bool{"success": true})
		return
	}
	if strings.HasPrefix(rest, "settlements/") {
		recordID := strings.TrimPrefix(rest, "settlements/")
		recordID = strings.Trim(recordID, "/")
		if recordID == "" || strings.Contains(recordID, "/") {
			httpjson.WriteError(w, http.StatusNotFound, "Not Found")
			return
		}
		h.deleteSettlement(w, r, userID, id, recordID)
		return
	}
	httpjson.WriteError(w, http.StatusNotFound, "Not Found")
}

func pathID(path string, prefix string, suffix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

func (h *Handler) requireWorkspace(ctx context.Context, userID string) (string, error) {
	if h.accounts == nil {
		return "", newRequestError("admin.adminAccounts.errors.noCurrentAccount", "")
	}
	return h.accounts.RequireCurrentID(ctx, userID)
}

func writeWorkspaceError(w http.ResponseWriter, err error) {
	if err != nil && strings.Contains(err.Error(), "adminAccounts") {
		httpjson.WriteError(w, http.StatusConflict, err.Error())
		return
	}
	httpjson.WriteError(w, http.StatusInternalServerError, "Failed to resolve admin account")
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	if err != nil && strings.Contains(err.Error(), "adminAccounts") {
		httpjson.WriteError(w, http.StatusConflict, err.Error())
		return
	}
	var requestErr *RequestError
	if errors.As(err, &requestErr) && requestErr.MessageKey == ErrorNotFound {
		httpjson.WriteError(w, http.StatusNotFound, requestErr.MessageKey)
		return
	}
	httpjson.WriteError(w, http.StatusInternalServerError, errorKey(err))
}
