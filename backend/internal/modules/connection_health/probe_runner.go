package connection_health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProbeTimeout 是单次真实探活请求的超时时间，任务书要求默认 10s。
const ProbeTimeout = 10 * time.Second

const defaultProbePrompt = "hi"

// ProbeRequest 是发起一次真实轻量探活所需的全部参数。UpstreamKey 只用于构造请求凭据，
// 探活结果（ProbeOutcome）绝不回填明文 key。
type ProbeRequest struct {
	BaseURL        string
	UpstreamKey    string
	ProviderFamily string
	ModelName      string
	MaxTokens      int
	ProbePrompt    string
}

// RealProbeRunner 按 provider family 构造最小请求，对上游 AI 端点发起一次性轻量调用。
// 不经过任何现有请求转发路径，独立的 http.Client，超时 10s。
type RealProbeRunner struct {
	client *http.Client
}

func NewRealProbeRunner() *RealProbeRunner {
	return &RealProbeRunner{client: &http.Client{Timeout: ProbeTimeout}}
}

// Probe 发起一次真实轻量探活，返回分类后的结果。err 只用于调用方感知调用本身是否被 ctx 取消，
// 正常的上游错误都归类进 ProbeOutcome.Result，不通过 error 返回。
func (r *RealProbeRunner) Probe(ctx context.Context, req ProbeRequest) ProbeOutcome {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1
	}
	prompt := strings.TrimSpace(req.ProbePrompt)
	if prompt == "" {
		prompt = defaultProbePrompt
	}

	httpReq, buildErr := buildProbeRequest(ctx, req, prompt, maxTokens)
	if buildErr != nil {
		return ProbeOutcome{Result: ResultInvalidResponse, Detail: redact(buildErr.Error(), req.UpstreamKey)}
	}

	started := time.Now()
	resp, err := r.client.Do(httpReq)
	latencyMs := int(time.Since(started).Milliseconds())
	if err != nil {
		return ProbeOutcome{Result: classifyTransportError(err), LatencyMs: latencyMs, Detail: redact(err.Error(), req.UpstreamKey)}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return classifyHTTPResponse(resp.StatusCode, body, req.UpstreamKey, latencyMs)
}

// buildProbeRequest 统一走 OpenAI 兼容的 /v1/chat/completions 网关端点。
//
// 背景（对应整改任务书第 4 项）：real_connections.upstream_key 和
// upstream_site_id -> upstream_sites.base_url 代表的是已对接的 new-api / sub2api 网关凭据
// 和网关地址（一个可转发 Gemini/Anthropic/OpenAI 等多种模型的中转站点），不是 provider
// 官方凭据。new-api 和 sub2api 都以 OpenAI 兼容协议对外暴露 /v1/chat/completions，
// 内部按 model 名称路由到实际 provider。如果对这些网关直接打 Gemini
// generateContent / Anthropic messages 原生端点，网关大概率不认识这些路径，会导致
// Gemini/Anthropic 模型被系统性误判为失败，进而错误触发自动降级。
// providerFamily 目前只用于在 ModelName 为空时选择一个合理的默认模型名做探活，
// 不再影响实际请求的 endpoint/鉴权方式。
func buildProbeRequest(ctx context.Context, req ProbeRequest, prompt string, maxTokens int) (*http.Request, error) {
	baseURL := strings.TrimRight(req.BaseURL, "/")
	model := req.ModelName
	if model == "" {
		model = defaultModelForProvider(req.ProviderFamily)
	}

	endpoint := baseURL + "/v1/chat/completions"
	payload := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   []map[string]any{{"role": "user", "content": prompt}},
	}
	headers := map[string]string{"Authorization": "Bearer " + req.UpstreamKey}
	return newJSONRequest(ctx, http.MethodPost, endpoint, payload, headers)
}

func defaultModelForProvider(providerFamily string) string {
	switch providerFamily {
	case ProviderGemini:
		return "gemini-1.5-flash"
	case ProviderAnthropic:
		return "claude-3-haiku-20240307"
	default:
		return "gpt-4o-mini"
	}
}

func newJSONRequest(ctx context.Context, method string, endpoint string, payload any, headers map[string]string) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	return httpReq, nil
}

// classifyTransportError 归类连接层面的错误（超时、DNS、连接被拒等）为网络波动。
func classifyTransportError(err error) ResultKey {
	if errors.Is(err, context.DeadlineExceeded) {
		return ResultNetworkFluctuation
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return ResultNetworkFluctuation
	}
	return ResultNetworkFluctuation
}

// classifyHTTPResponse 按状态码和响应体归类为 7 种错误分类之一，或 ok。
func classifyHTTPResponse(status int, body []byte, upstreamKey string, latencyMs int) ProbeOutcome {
	detail := redact(truncate(string(body), 500), upstreamKey)
	promptTokens, completionTokens, totalTokens, actualCostUSD := parseProbeUsage(body)

	withUsage := func(result ResultKey, detailText string) ProbeOutcome {
		return ProbeOutcome{
			Result: result, LatencyMs: latencyMs, Detail: detailText,
			PromptTokens: promptTokens, CompletionTokens: completionTokens, TotalTokens: totalTokens,
			ActualCostUSD: actualCostUSD,
		}
	}

	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		if !json.Valid(body) {
			return withUsage(ResultInvalidResponse, detail)
		}
		return withUsage(ResultOK, "")

	case status == http.StatusTooManyRequests:
		return withUsage(ResultRateLimited, detail)

	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return withUsage(ResultAuth, detail)

	case status == http.StatusNotFound:
		return withUsage(ResultModelNotFound, detail)

	case status >= 500:
		return withUsage(ResultServerError, detail)

	default:
		// 其余 4xx：优先从响应体识别「模型不存在/不支持」，避免被当成 soft invalid_response
		// 而长期降级、无法触发模型限制摘除（例如 Codex ChatGPT 账号返回
		// "model is not supported" 的 400 invalid_request_error）。
		if status >= 400 && status < 500 && isModelUnavailableErrorBody(body) {
			return withUsage(ResultModelNotFound, detail)
		}
		// 其它参数错误无法安全归类为上游不可用，按响应无法解析处理。
		return withUsage(ResultInvalidResponse, detail)
	}
}

// parseProbeUsage 从 OpenAI 兼容 chat/completions 响应中解析 usage tokens 与真实费用。
// 兼容 usage.total_tokens / prompt_tokens / completion_tokens，以及部分网关的 camelCase。
// actualCostUSD 优先读 usage 内 actual_cost/total_cost/cost，其次顶层同名字段。
func parseProbeUsage(body []byte) (prompt, completion, total int, actualCostUSD float64) {
	if len(body) == 0 || !json.Valid(body) {
		return 0, 0, 0, 0
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0, 0, 0
	}
	usage, _ := payload["usage"].(map[string]any)
	if usage == nil {
		if data, ok := payload["data"].(map[string]any); ok {
			usage, _ = data["usage"].(map[string]any)
		}
	}
	if usage != nil {
		prompt = jsonNumberAsInt(usage["prompt_tokens"])
		if prompt == 0 {
			prompt = jsonNumberAsInt(usage["promptTokens"])
		}
		if prompt == 0 {
			prompt = jsonNumberAsInt(usage["input_tokens"])
		}
		completion = jsonNumberAsInt(usage["completion_tokens"])
		if completion == 0 {
			completion = jsonNumberAsInt(usage["completionTokens"])
		}
		if completion == 0 {
			completion = jsonNumberAsInt(usage["output_tokens"])
		}
		total = jsonNumberAsInt(usage["total_tokens"])
		if total == 0 {
			total = jsonNumberAsInt(usage["totalTokens"])
		}
		if total == 0 {
			total = prompt + completion
		}
		actualCostUSD = jsonNumberAsFloat(usage["actual_cost"])
		if actualCostUSD <= 0 {
			actualCostUSD = jsonNumberAsFloat(usage["actualCost"])
		}
		if actualCostUSD <= 0 {
			actualCostUSD = jsonNumberAsFloat(usage["total_cost"])
		}
		if actualCostUSD <= 0 {
			actualCostUSD = jsonNumberAsFloat(usage["totalCost"])
		}
		if actualCostUSD <= 0 {
			actualCostUSD = jsonNumberAsFloat(usage["cost"])
		}
	}
	if actualCostUSD <= 0 {
		actualCostUSD = jsonNumberAsFloat(payload["actual_cost"])
	}
	if actualCostUSD <= 0 {
		actualCostUSD = jsonNumberAsFloat(payload["total_cost"])
	}
	if actualCostUSD <= 0 {
		actualCostUSD = jsonNumberAsFloat(payload["cost"])
	}
	return prompt, completion, total, actualCostUSD
}

// parseProbeUsageTokens 保留给旧测试与兼容调用。
func parseProbeUsageTokens(body []byte) (prompt, completion, total int) {
	p, c, t, _ := parseProbeUsage(body)
	return p, c, t
}

func jsonNumberAsFloat(value any) float64 {
	switch v := value.(type) {
	case float64:
		if v > 0 {
			return v
		}
	case float32:
		if v > 0 {
			return float64(v)
		}
	case int:
		if v > 0 {
			return float64(v)
		}
	case int64:
		if v > 0 {
			return float64(v)
		}
	case json.Number:
		if n, err := v.Float64(); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func jsonNumberAsInt(value any) int {
	switch v := value.(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return int(n)
		}
	}
	return 0
}

// isModelUnavailableErrorBody 识别上游明确表示模型不可用/不支持的错误正文。
// 这些应归类为 model_not_found（硬失败），而不是 invalid_response（软失败）。
func isModelUnavailableErrorBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	// 常见 OpenAI / 网关文案
	phrases := []string{
		"model_not_found",
		"model not found",
		"model_not_available",
		"model not available",
		"unknown model",
		"invalid model",
		"no such model",
		"does not exist",
		"do not exist",
		"not supported",
		"is not supported",
		"not support",
		"unsupported model",
		"model is not",
		"cannot find model",
		"could not find model",
		"has no access to model",
		"you do not have access to model",
	}
	for _, phrase := range phrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	// invalid_request_error 且正文提到 model，多数是模型名/权限问题
	if strings.Contains(lower, "invalid_request_error") && strings.Contains(lower, "model") {
		return true
	}
	return false
}

// redact 把探活凭据从错误信息/响应体中裁剪掉，事件和日志绝不落地明文 key。
func redact(s string, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "***")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
