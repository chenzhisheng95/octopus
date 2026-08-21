package relay

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

var requestIDs atomic.Uint64 // requestIDs 分配进程内严格递增的请求 ID。
var errNoActiveChannel = errors.New("no active channel") // errNoActiveChannel 表示分组尚未选择活动渠道。
var errUpstreamTimeout = errors.New("upstream request timed out") // errUpstreamTimeout 表示单次上游尝试超时。

const defaultGroupTimeout = 30 // defaultGroupTimeout 是分组未显式配置超时时的默认秒数。

// execution 保存单个客户端请求的全部可变执行状态。
type execution struct {
	ctx            *gin.Context       // 当前客户端请求上下文。
	protocol       *relayProtocol     // 客户端协议规则。
	request        relayRequest       // 原始请求和路由字段。
	log            LogRecord          // 持续更新的完整请求记录。
	requestMetrics model.StatsMetrics // requestMetrics 累积全部真实上游调用的用量和费用。
	attemptIndex   int                // 当前请求已经分配的最后尝试序号。
}

// HandleChatCompletions 处理 OpenAI Chat Completions 客户端请求。
func HandleChatCompletions(c *gin.Context) {
	(&execution{ctx: c, protocol: &relayProtocol{format: llm.APIFormatOpenAIChatCompletion, route: "/chat/completions", authType: httpclient.AuthTypeBearer, inbound: openai.NewInboundTransformer()}}).execute()
}

// HandleResponses 处理 OpenAI Responses 客户端请求。
func HandleResponses(c *gin.Context) {
	(&execution{ctx: c, protocol: &relayProtocol{format: llm.APIFormatOpenAIResponse, route: "/responses", authType: httpclient.AuthTypeBearer, inbound: responses.NewInboundTransformer()}}).execute()
}

// HandleMessages 处理 Anthropic Messages 客户端请求。
func HandleMessages(c *gin.Context) {
	(&execution{ctx: c, protocol: &relayProtocol{format: llm.APIFormatAnthropicMessage, route: "/messages", authType: httpclient.AuthTypeAPIKey, inbound: anthropic.NewInboundTransformer()}}).execute()
}

// execute 初始化请求，并在渠道未选择时等待、失败时重试，直至提交响应或客户端取消。
func (e *execution) execute() {
	e.log = LogRecord{LogOverview: LogOverview{ID: requestIDs.Add(1), State: RequestStateRunning, StartedAt: time.Now(), ClientProtocol: e.protocol.format}}
	ctx := e.ctx.Request.Context()
	raw, err := httpclient.ReadHTTPRequest(e.ctx.Request)
	if err != nil {
		e.emit(LogEventRequestStarted, nil)
		e.finish(RequestStateFailed, err, nil, nil)
		e.protocol.writeError(e.ctx, http.StatusBadRequest, err)
		return
	}
	e.log.RequestBody = string(raw.Body)
	parsed, err := e.protocol.inbound.TransformRequest(ctx, cloneRequest(raw, ctx))
	if err != nil {
		e.emit(LogEventRequestStarted, nil)
		e.finish(RequestStateFailed, err, nil, nil)
		e.protocol.writeError(e.ctx, http.StatusBadRequest, err)
		return
	}
	e.request = relayRequest{raw: raw, model: parsed.Model, stream: parsed.Stream != nil && *parsed.Stream}
	e.log.RequestModel = e.request.model
	e.log.Stream = e.request.stream
	e.emit(LogEventRequestStarted, nil)
	if supported := e.ctx.GetString("supported_models"); supported != "" {
		allowed := false
		for _, item := range strings.Split(supported, ",") {
			if strings.TrimSpace(item) == e.request.model {
				allowed = true
				break
			}
		}
		if !allowed {
			err = errors.New("model not supported")
			e.finish(RequestStateFailed, err, nil, nil)
			e.protocol.writeError(e.ctx, http.StatusBadRequest, err)
			return
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			e.finish(RequestStateCanceled, err, nil, nil)
			return
		}

		activeItemChanged := op.GroupActiveItemChangeSignal()
		candidates, retryInterval, timeout, resolveErr := e.resolveCandidates(ctx)
		if errors.Is(resolveErr, errNoActiveChannel) {
			if e.log.Error != "" {
				e.log.Error = ""
				e.emit(LogEventTargetWaiting, nil)
			}
			select {
			case <-ctx.Done():
				e.finish(RequestStateCanceled, ctx.Err(), nil, nil)
				return
			case <-activeItemChanged:
			}
			continue
		}
		if resolveErr != nil {
			e.recordUnavailableTarget(model.GroupItem{}, nil, resolveErr)
			select {
			case <-ctx.Done():
				e.finish(RequestStateCanceled, ctx.Err(), nil, nil)
				return
			case <-time.After(time.Duration(retryInterval) * time.Second):
			}
			continue
		}

		// 按优先级依次尝试组内渠道，任一渠道失败或超时立即切到下一个来源。
		for _, candidate := range candidates {
			if err := ctx.Err(); err != nil {
				e.finish(RequestStateCanceled, err, nil, nil)
				return
			}
			done, _ := e.executeAttempt(ctx, candidate.item, candidate.channel, time.Duration(timeout)*time.Second)
			if done {
				return
			}
		}

		// 本轮所有来源均失败，等待后从头重试，活动渠道恢复后自动切回。
		select {
		case <-ctx.Done():
			e.finish(RequestStateCanceled, ctx.Err(), nil, nil)
			return
		case <-time.After(time.Duration(retryInterval) * time.Second):
		}
	}
}

// candidate 保存组内一个可尝试的渠道及其分组项。
type candidate struct {
	item    model.GroupItem // 分组项。
	channel *model.Channel  // 对应的渠道配置。
}

// resolveCandidates 返回按优先级排序的可用渠道列表：活动项优先，其余按 Priority 升序。
func (e *execution) resolveCandidates(ctx context.Context) ([]candidate, int, int, error) {
	retryInterval := 1
	timeout := defaultGroupTimeout
	group, err := op.GroupGetByName(e.request.model, ctx)
	if err != nil {
		return nil, retryInterval, timeout, errors.New("model not found")
	}
	if group.RetryInterval >= 1 {
		retryInterval = group.RetryInterval
	}
	if group.Timeout >= 1 {
		timeout = group.Timeout
	}
	if group.ActiveItemID == 0 {
		return nil, retryInterval, timeout, errNoActiveChannel
	}

	var active model.GroupItem
	rest := make([]model.GroupItem, 0, len(group.Items))
	for _, item := range group.Items {
		if item.ID == group.ActiveItemID {
			active = item
		} else {
			rest = append(rest, item)
		}
	}
	if active.ID == 0 {
		return nil, retryInterval, timeout, errors.New("active channel not found")
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Priority < rest[j].Priority })

	candidates := make([]candidate, 0, len(group.Items))
	appendCandidate := func(item model.GroupItem) {
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil || !channel.Enabled || channel.Key == "" {
			return
		}
		candidates = append(candidates, candidate{item: item, channel: channel})
	}
	appendCandidate(active)
	for _, item := range rest {
		appendCandidate(item)
	}
	if len(candidates) == 0 {
		return nil, retryInterval, timeout, errors.New("no available channel")
	}
	return candidates, retryInterval, timeout, nil
}

// executeAttempt 执行当前渠道的一次上游尝试，提交前失败时交回外层继续重试。
// timeout 限定单次上游尝试（流式请求为取回首个事件前）的时长，超时会中止并返回 errUpstreamTimeout。
func (e *execution) executeAttempt(ctx context.Context, item model.GroupItem, channel *model.Channel, timeout time.Duration) (bool, error) {
	client, err := helper.ChannelHttpClient(channel)
	if err != nil {
		e.recordUnavailableTarget(item, channel, err)
		return false, err
	}
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	startedAt := time.Now()
	attempt := e.startAttempt(item, channel, cancelAttempt)

	type attemptOutcome struct {
		result upstreamResult
	}
	done := make(chan attemptOutcome, 1)
	go func() {
		done <- attemptOutcome{(&forwarder{protocol: e.protocol, request: &e.request, client: client}).executeUpstream(attemptCtx, item.ModelName, channel)}
	}()

	var result upstreamResult
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		select {
		case <-attemptCtx.Done():
			cancelAttempt()
			result = upstreamResult{err: attemptCtx.Err()}
			if outcome := <-done; outcome.result.response != nil {
				outcome.result.response.Close()
			}
		case <-timer.C:
			cancelAttempt()
			result = upstreamResult{err: errUpstreamTimeout}
			if outcome := <-done; outcome.result.response != nil {
				outcome.result.response.Close()
			}
		case outcome := <-done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			result = outcome.result
		}
	} else {
		result = (<-done).result
	}

	interrupted := !clearAttempt(e.log.ID, attempt.Index)
	if interrupted {
		result.err = errors.New("relay attempt interrupted")
	}
	if result.err != nil {
		return e.handleAttemptFailure(ctx, item, channel, attempt, result, time.Since(startedAt), interrupted)
	}

	e.commitAttempt(ctx, attemptCtx, item, channel, attempt, result, startedAt)
	return true, nil
}

// handleAttemptFailure 记录上游失败并决定当前请求是立即重试、等待重试还是结束。
func (e *execution) handleAttemptFailure(ctx context.Context, item model.GroupItem, channel *model.Channel, attempt *LogAttempt, result upstreamResult, duration time.Duration, interrupted bool) (bool, error) {
	if result.response != nil {
		result.response.Close()
	}
	attempt.Error = result.err.Error()
	if len(result.responseBody) > 0 {
		attempt.Error += ": " + string(result.responseBody)
	}
	if interrupted {
		e.log.Error = attempt.Error
		e.emit(LogEventAttemptFinished, attempt)
		return false, nil
	}
	if errors.Is(result.err, context.Canceled) || ctx.Err() != nil {
		e.log.Error = attempt.Error
		e.emit(LogEventAttemptFinished, attempt)
		cancelErr := result.err
		if ctx.Err() != nil {
			cancelErr = ctx.Err()
		}
		e.finish(RequestStateCanceled, cancelErr, nil, nil)
		return true, nil
	}
	if !errors.Is(result.err, errUnsupportedTarget) {
		metrics := usageMetrics(item.ModelName, result.usage)
		e.requestMetrics.Add(metrics)
		applyAttemptMetric(item, channel, metrics, duration, false)
	}
	e.log.Error = attempt.Error
	e.emit(LogEventAttemptFinished, attempt)
	return false, result.err
}

// commitAttempt 将已验证的上游响应提交给客户端，并完成本次尝试和请求统计。
func (e *execution) commitAttempt(ctx, attemptCtx context.Context, item model.GroupItem, channel *model.Channel, attempt *LogAttempt, result upstreamResult, startedAt time.Time) {
	e.log.State = RequestStateCommitted
	e.emit(LogEventResponseCommitted, attempt)

	commit := result.response.Commit(attemptCtx, e.ctx)
	result.response.Close()
	usage := commit.usage
	if usage == nil {
		usage = result.usage
	}
	metricDuration := time.Since(startedAt)
	if !commit.firstWriteAt.IsZero() {
		metricDuration = commit.firstWriteAt.Sub(startedAt)
	}
	metrics := usageMetrics(item.ModelName, usage)
	e.requestMetrics.Add(metrics)
	applyAttemptMetric(item, channel, metrics, metricDuration, commit.err == nil || errors.Is(commit.err, errClientWrite) || errors.Is(commit.err, context.Canceled))
	if commit.err == nil {
		e.finish(RequestStateSuccess, nil, commit.responseBody, usage)
	} else if errors.Is(commit.err, context.Canceled) || ctx.Err() != nil {
		e.finish(RequestStateCanceled, commit.err, commit.responseBody, usage)
	} else {
		e.finish(RequestStateFailed, commit.err, commit.responseBody, usage)
	}
}

// newAttempt 分配新的尝试序号并写入请求的最终路由字段。
func (e *execution) newAttempt(channelName, modelName string) *LogAttempt {
	e.attemptIndex++
	attempt := &LogAttempt{Index: e.attemptIndex, ChannelName: channelName, ModelName: modelName}
	e.log.ActualModel = modelName
	e.log.FinalChannelName = channelName
	return attempt
}

// recordUnavailableTarget 记录当前活动目标不可用且未发出上游请求的尝试。
func (e *execution) recordUnavailableTarget(item model.GroupItem, channel *model.Channel, err error) {
	channelName := "Octopus"
	modelName := item.ModelName
	if modelName == "" {
		modelName = e.request.model
	}
	if channel != nil {
		channelName = channel.Name
	}
	attempt := e.newAttempt(channelName, modelName)
	attempt.Error = err.Error()
	e.log.Error = attempt.Error
	e.emit(LogEventAttemptFinished, attempt)
}

// startAttempt 登记可中止句柄，并发布一次真实上游请求的运行状态。
func (e *execution) startAttempt(item model.GroupItem, channel *model.Channel, cancel context.CancelFunc) *LogAttempt {
	attempt := e.newAttempt(channel.Name, item.ModelName)
	e.log.Error = ""
	setAttempt(e.log.ID, attempt.Index, cancel)
	e.emit(LogEventAttemptStarted, attempt)
	return attempt
}

// emit 向日志状态提交当前请求和本次尝试的独立快照。
func (e *execution) emit(eventType LogEventType, current *LogAttempt) {
	var attempt *LogAttempt
	if current != nil {
		copyAttempt := *current
		attempt = &copyAttempt
	}
	applyLog(eventType, e.log, attempt)
}

// finish 通过唯一完成入口补全日志和统计。
func (e *execution) finish(state RequestState, err error, responseBody []byte, usage *llm.Usage) {
	e.log.State = state
	e.log.CompletedAt = time.Now()
	e.log.Duration = e.log.CompletedAt.Sub(e.log.StartedAt)
	e.requestMetrics.WaitTime = e.log.Duration.Milliseconds()
	if state == RequestStateSuccess {
		e.log.Error = ""
	} else if err != nil {
		e.log.Error = err.Error()
	}
	if len(responseBody) > 0 {
		e.log.ResponseBody = string(responseBody)
	}
	if usage != nil {
		e.log.InputTokens = usage.PromptTokens
		e.log.OutputTokens = usage.CompletionTokens
		if usage.PromptTokensDetails != nil {
			e.log.CacheReadTokens = usage.PromptTokensDetails.CachedTokens
			e.log.CacheWriteTokens = usage.PromptTokensDetails.WriteCachedTokens
		}
	}
	e.log.TotalCost = e.requestMetrics.InputCost + e.requestMetrics.OutputCost
	applyCompletion(e.ctx.Request.Context(), e.ctx.GetInt("api_key_id"), state, e.requestMetrics)
	e.emit(LogEventRequestFinished, nil)
}
