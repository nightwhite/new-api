package controller

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

type asyncRelayTaskSpec struct {
	Action     string
	TargetPath string
	Format     types.RelayFormat
}

type asyncRelayTaskContext struct {
	UserId             int
	UserName           string
	TokenId            int
	TokenKey           string
	TokenName          string
	TokenUnlimited     bool
	TokenQuota         int
	TokenGroup         string
	TokenCrossGroup    bool
	TokenModelLimitOn  bool
	TokenModelLimitMap map[string]bool

	UsingGroup string
	UserGroup  string
	UserQuota  int
	UserEmail  string
	UserSetting dto.UserSetting

	ChannelId            int
	ChannelType          int
	ChannelName          string
	ChannelCreateTime    int64
	ChannelBaseUrl       string
	ChannelKey           string
	ChannelOrganization  string
	ChannelSetting       dto.ChannelSettings
	ChannelOtherSetting  dto.ChannelOtherSettings
	ChannelParamOverride map[string]any
	ChannelHeaderOverride map[string]any
	ChannelAutoBan       bool
	ChannelModelMapping  string
	StatusCodeMapping    string
	ChannelIsMultiKey    bool
	ChannelMultiKeyIndex int
	ApiVersion           string
	Region               string
	Plugin               string
	BotId                string

	OriginalModel  string
	RequestHeaders http.Header
	RequestBody    []byte

	// For stream-only upstream fallback (currently only used by chat.completions).
	StreamFallbackBody []byte
}

func AsyncRelayChatCompletions(c *gin.Context) {
	createAsyncRelayTask(c, asyncRelayTaskSpec{
		Action:     "chat.completions",
		TargetPath: "/v1/chat/completions",
		Format:     types.RelayFormatOpenAI,
	})
}

func AsyncRelayImagesGenerations(c *gin.Context) {
	createAsyncRelayTask(c, asyncRelayTaskSpec{
		Action:     "images.generations",
		TargetPath: "/v1/images/generations",
		Format:     types.RelayFormatOpenAIImage,
	})
}

func AsyncRelayImagesEdits(c *gin.Context) {
	createAsyncRelayTask(c, asyncRelayTaskSpec{
		Action:     "images.edits",
		TargetPath: "/v1/images/edits",
		Format:     types.RelayFormatOpenAIImage,
	})
}

func createAsyncRelayTask(c *gin.Context, spec asyncRelayTaskSpec) {
	var forceStreamFalse bool
	if spec.Format == types.RelayFormatOpenAI && spec.TargetPath == "/v1/chat/completions" {
		body, err := common.GetRequestBody(c)
		if err != nil {
			abortAsyncRelayWithOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "read_request_body_failed")
			return
		}
		normalized, streamBody, changed, err := normalizeAsyncChatBody(body)
		if err != nil {
			abortAsyncRelayWithOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid_json_body")
			return
		}
		if changed {
			forceStreamFalse = true
			c.Set(common.KeyRequestBody, normalized)
		}
		if len(streamBody) != 0 {
			c.Set("async_stream_fallback_body", streamBody)
		}
	}

	requestBody, err := common.GetRequestBody(c)
	if err != nil {
		abortAsyncRelayWithOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "read_request_body_failed")
		return
	}

	userId := c.GetInt("id")
	if userId == 0 {
		abortAsyncRelayWithOpenAIError(c, http.StatusUnauthorized, "invalid_request_error", "unauthorized")
		return
	}

	channelId := c.GetInt("channel_id")
	if channelId == 0 {
		abortAsyncRelayWithOpenAIError(c, http.StatusServiceUnavailable, "upstream_error", "no_channel_selected")
		return
	}

	originalModel := c.GetString("original_model")
	if originalModel == "" {
		abortAsyncRelayWithOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if usingGroup == "" {
		usingGroup = c.GetString("group")
	}

	var userSetting dto.UserSetting
	if v, ok := common.GetContextKeyType[dto.UserSetting](c, constant.ContextKeyUserSetting); ok {
		userSetting = v
	}

	taskId := common.GetUUID()
	now := time.Now().Unix()

	task := &model.Task{
		CreatedAt:  now,
		UpdatedAt:  now,
		TaskID:     taskId,
		Platform:   constant.TaskPlatformAsyncRelay,
		UserId:     userId,
		Group:      usingGroup,
		ChannelId:  channelId,
		Action:     spec.Action,
		Status:     model.TaskStatusQueued,
		SubmitTime: now,
		Progress:   "0%",
	}

	if err := task.Insert(); err != nil {
		abortAsyncRelayWithOpenAIError(c, http.StatusInternalServerError, "upstream_error", "create_task_failed")
		return
	}

	ctxSnapshot := asyncRelayTaskContext{
		UserId:              userId,
		UserName:            common.GetContextKeyString(c, constant.ContextKeyUserName),
		TokenId:             c.GetInt("token_id"),
		TokenKey:            c.GetString("token_key"),
		TokenName:           c.GetString("token_name"),
		TokenUnlimited:      c.GetBool("token_unlimited_quota"),
		TokenQuota:          c.GetInt("token_quota"),
		TokenGroup:          common.GetContextKeyString(c, constant.ContextKeyTokenGroup),
		TokenCrossGroup:     common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry),
		TokenModelLimitOn:   c.GetBool("token_model_limit_enabled"),
		TokenModelLimitMap:  getTokenModelLimitMap(c),
		UsingGroup:          usingGroup,
		UserGroup:           common.GetContextKeyString(c, constant.ContextKeyUserGroup),
		UserQuota:           common.GetContextKeyInt(c, constant.ContextKeyUserQuota),
		UserEmail:           common.GetContextKeyString(c, constant.ContextKeyUserEmail),
		UserSetting:         userSetting,
		ChannelId:           channelId,
		ChannelType:         common.GetContextKeyInt(c, constant.ContextKeyChannelType),
		ChannelName:         common.GetContextKeyString(c, constant.ContextKeyChannelName),
		ChannelCreateTime:   c.GetInt64(string(constant.ContextKeyChannelCreateTime)),
		ChannelBaseUrl:      common.GetContextKeyString(c, constant.ContextKeyChannelBaseUrl),
		ChannelKey:          common.GetContextKeyString(c, constant.ContextKeyChannelKey),
		ChannelOrganization: common.GetContextKeyString(c, constant.ContextKeyChannelOrganization),
		ChannelParamOverride: common.GetContextKeyStringMap(c, constant.ContextKeyChannelParamOverride),
		ChannelHeaderOverride: common.GetContextKeyStringMap(c, constant.ContextKeyChannelHeaderOverride),
		ChannelAutoBan:        common.GetContextKeyBool(c, constant.ContextKeyChannelAutoBan),
		ChannelModelMapping:   common.GetContextKeyString(c, constant.ContextKeyChannelModelMapping),
		StatusCodeMapping:     common.GetContextKeyString(c, constant.ContextKeyChannelStatusCodeMapping),
		ChannelIsMultiKey:     common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey),
		ChannelMultiKeyIndex:  common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex),
		ApiVersion:            c.GetString("api_version"),
		Region:                c.GetString("region"),
		Plugin:                c.GetString("plugin"),
		BotId:                 c.GetString("bot_id"),
		OriginalModel:         originalModel,
		RequestHeaders:        cloneHeader(c.Request.Header),
		RequestBody:           append([]byte(nil), requestBody...),
	}
	if forceStreamFalse {
		// Make it explicit that we are expecting non-stream JSON response for async wrapper.
		ctxSnapshot.RequestHeaders.Set("Accept", "application/json")
	}
	if v, ok := c.Get("async_stream_fallback_body"); ok {
		if b, ok := v.([]byte); ok && len(b) > 0 {
			ctxSnapshot.StreamFallbackBody = append([]byte(nil), b...)
		}
	}

	if v, ok := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting); ok {
		ctxSnapshot.ChannelSetting = v
	}
	if v, ok := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting); ok {
		ctxSnapshot.ChannelOtherSetting = v
	}

	gopool.Go(func() {
		executeAsyncRelayTask(task, spec, ctxSnapshot)
	})

	c.JSON(http.StatusOK, gin.H{
		"task_id": taskId,
		"status":  "queued",
	})
}

func executeAsyncRelayTask(task *model.Task, spec asyncRelayTaskSpec, snap asyncRelayTaskContext) {
	defer func() {
		if r := recover(); r != nil {
			failAsyncRelayTask(task, http.StatusInternalServerError, fmt.Sprintf("panic:%v", r))
		}
	}()

	now := time.Now().Unix()
	task.Status = model.TaskStatusInProgress
	task.StartTime = now
	task.UpdatedAt = now
	_ = task.Update()

	respStatus, respBody, respContentType := runRelayAndCapture(spec, snap, snap.RequestBody, snap.RequestHeaders)

	// Stream-only upstream fallback for chat.completions:
	// - If upstream responds with SSE when we requested non-stream
	// - Or if upstream indicates "stream required" by returning an error containing stream-only hints
	if spec.Format == types.RelayFormatOpenAI && spec.TargetPath == "/v1/chat/completions" && len(snap.StreamFallbackBody) > 0 {
		if isEventStreamContentType(respContentType) || shouldFallbackToStream(respStatus, respBody) {
			streamHeaders := cloneHeader(snap.RequestHeaders)
			// Some upstreams require this Accept to return SSE.
			streamHeaders.Set("Accept", "text/event-stream")
			s2, b2, ct2 := runRelayAndCapture(spec, snap, snap.StreamFallbackBody, streamHeaders)
			if isEventStreamContentType(ct2) || looksLikeSSE(b2) {
				if finalJSON, ok := sseChatCompletionsToFinalJSON(b2, snap.OriginalModel); ok {
					respStatus = http.StatusOK
					respBody = finalJSON
				} else {
					// If we can't parse SSE, store raw SSE for debugging.
					respStatus = s2
					respBody = b2
				}
			} else {
				respStatus = s2
				respBody = b2
			}
		}
	}

	maxBytes := int64(constant.AsyncTaskMaxResponseMB) << 20
	if maxBytes > 0 && int64(len(respBody)) > maxBytes {
		respStatus = http.StatusInternalServerError
		respBody = []byte(`{"error":{"message":"async_response_too_large","type":"upstream_error"}}`)
	}

	finishTime := time.Now().Unix()
	task.UpdatedAt = finishTime
	task.FinishTime = finishTime
	task.ResponseStatusCode = respStatus
	task.Data = append([]byte(nil), respBody...)
	task.Progress = "100%"

	if respStatus >= 200 && respStatus < 300 {
		task.Status = model.TaskStatusSuccess
		task.FailReason = ""
	} else {
		task.Status = model.TaskStatusFailure
		task.FailReason = extractOpenAIErrorMessage(respBody)
		if task.FailReason == "" {
			task.FailReason = http.StatusText(respStatus)
		}
		logger.LogWarn(context.Background(), fmt.Sprintf("async relay task failed, task_id=%s, status=%d, reason=%s", task.TaskID, respStatus, task.FailReason))
	}

	_ = task.Update()
}

func AsyncRelayTaskResult(c *gin.Context) {
	taskId := c.Param("task_id")
	if taskId == "" {
		abortAsyncRelayWithOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "task_id is required")
		return
	}

	userId := c.GetInt("id")
	task, exist, err := model.GetByTaskId(userId, taskId)
	if err != nil {
		abortAsyncRelayWithOpenAIError(c, http.StatusInternalServerError, "upstream_error", "get_task_failed")
		return
	}
	if !exist || task == nil || task.Platform != constant.TaskPlatformAsyncRelay {
		abortAsyncRelayWithOpenAIError(c, http.StatusNotFound, "invalid_request_error", "task_not_found")
		return
	}

	if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
		status := task.ResponseStatusCode
		if status == 0 {
			if task.Status == model.TaskStatusSuccess {
				status = http.StatusOK
			} else {
				status = http.StatusInternalServerError
			}
		}
		c.Data(status, "application/json", task.Data)
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"task_id":   task.TaskID,
		"status":    string(task.Status),
		"progress":  task.Progress,
		"submitted": task.SubmitTime,
	})
}

func abortAsyncRelayWithOpenAIError(c *gin.Context, status int, errType string, msg string) {
	c.JSON(status, gin.H{
		"error": types.OpenAIError{
			Message: msg,
			Type:    errType,
		},
	})
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	out := make(http.Header, len(h))
	for k, v := range h {
		vv := make([]string, len(v))
		copy(vv, v)
		out[k] = vv
	}
	return out
}

func extractOpenAIErrorMessage(body []byte) string {
	var wrapped struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := common.Unmarshal(body, &wrapped); err != nil {
		return ""
	}
	return wrapped.Error.Message
}

func normalizeAsyncChatBody(body []byte) (nonStream []byte, stream []byte, changed bool, err error) {
	var req dto.GeneralOpenAIRequest
	if err := common.Unmarshal(body, &req); err != nil {
		return nil, nil, false, err
	}
	// Prepare stream body (always available for fallback, even if original stream=false).
	reqStream := req
	reqStream.Stream = true
	stream, err = common.Marshal(reqStream)
	if err != nil {
		return nil, nil, false, err
	}

	if !req.Stream {
		return body, stream, false, nil
	}
	req.Stream = false
	normalized, err := common.Marshal(req)
	if err != nil {
		return nil, nil, false, err
	}
	return normalized, stream, true, nil
}

func getTokenModelLimitMap(c *gin.Context) map[string]bool {
	raw, ok := c.Get("token_model_limit")
	if !ok || raw == nil {
		return nil
	}
	if m, ok := raw.(map[string]bool); ok {
		return m
	}
	return nil
}

func failAsyncRelayTask(task *model.Task, statusCode int, reason string) {
	now := time.Now().Unix()
	task.UpdatedAt = now
	task.FinishTime = now
	task.Status = model.TaskStatusFailure
	task.Progress = "100%"
	task.ResponseStatusCode = statusCode
	task.FailReason = reason
	task.Data = []byte(`{"error":{"message":"` + reason + `","type":"upstream_error"}}`)
	_ = task.Update()
}

func runRelayAndCapture(spec asyncRelayTaskSpec, snap asyncRelayTaskContext, body []byte, headers http.Header) (status int, respBody []byte, contentType string) {
	rec := httptest.NewRecorder()
	bg, _ := gin.CreateTestContext(rec)

	timeoutSeconds := constant.AsyncTaskTimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 600
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, spec.TargetPath, bytes.NewReader(body)).WithContext(ctx)
	req.Header = cloneHeader(headers)
	bg.Request = req

	common.SetContextKey(bg, constant.ContextKeyRequestStartTime, time.Now())
	bg.Set("id", snap.UserId)
	if snap.UserName != "" {
		// RecordConsumeLog uses c.GetString("username")
		bg.Set("username", snap.UserName)
	}

	bg.Set("token_id", snap.TokenId)
	bg.Set("token_key", snap.TokenKey)
	bg.Set("token_name", snap.TokenName)
	bg.Set("token_unlimited_quota", snap.TokenUnlimited)
	if !snap.TokenUnlimited {
		bg.Set("token_quota", snap.TokenQuota)
	}
	bg.Set("token_model_limit_enabled", snap.TokenModelLimitOn)
	if snap.TokenModelLimitOn {
		bg.Set("token_model_limit", snap.TokenModelLimitMap)
	}
	common.SetContextKey(bg, constant.ContextKeyTokenGroup, snap.TokenGroup)
	common.SetContextKey(bg, constant.ContextKeyTokenCrossGroupRetry, snap.TokenCrossGroup)

	common.SetContextKey(bg, constant.ContextKeyUsingGroup, snap.UsingGroup)
	common.SetContextKey(bg, constant.ContextKeyUserGroup, snap.UserGroup)
	common.SetContextKey(bg, constant.ContextKeyUserQuota, snap.UserQuota)
	common.SetContextKey(bg, constant.ContextKeyUserEmail, snap.UserEmail)
	common.SetContextKey(bg, constant.ContextKeyUserSetting, snap.UserSetting)

	if snap.ChannelId == 0 || snap.ChannelKey == "" {
		return http.StatusServiceUnavailable, []byte(`{"error":{"message":"invalid_channel_context","type":"upstream_error"}}`), "application/json"
	}

	common.SetContextKey(bg, constant.ContextKeyOriginalModel, snap.OriginalModel)
	common.SetContextKey(bg, constant.ContextKeyChannelId, snap.ChannelId)
	common.SetContextKey(bg, constant.ContextKeyChannelName, snap.ChannelName)
	common.SetContextKey(bg, constant.ContextKeyChannelType, snap.ChannelType)
	common.SetContextKey(bg, constant.ContextKeyChannelCreateTime, snap.ChannelCreateTime)
	common.SetContextKey(bg, constant.ContextKeyChannelBaseUrl, snap.ChannelBaseUrl)
	common.SetContextKey(bg, constant.ContextKeyChannelKey, snap.ChannelKey)
	common.SetContextKey(bg, constant.ContextKeyChannelOrganization, snap.ChannelOrganization)
	common.SetContextKey(bg, constant.ContextKeyChannelSetting, snap.ChannelSetting)
	common.SetContextKey(bg, constant.ContextKeyChannelOtherSetting, snap.ChannelOtherSetting)
	common.SetContextKey(bg, constant.ContextKeyChannelParamOverride, snap.ChannelParamOverride)
	common.SetContextKey(bg, constant.ContextKeyChannelHeaderOverride, snap.ChannelHeaderOverride)
	common.SetContextKey(bg, constant.ContextKeyChannelAutoBan, snap.ChannelAutoBan)
	common.SetContextKey(bg, constant.ContextKeyChannelModelMapping, snap.ChannelModelMapping)
	common.SetContextKey(bg, constant.ContextKeyChannelStatusCodeMapping, snap.StatusCodeMapping)
	common.SetContextKey(bg, constant.ContextKeyChannelIsMultiKey, snap.ChannelIsMultiKey)
	common.SetContextKey(bg, constant.ContextKeyChannelMultiKeyIndex, snap.ChannelMultiKeyIndex)
	common.SetContextKey(bg, constant.ContextKeySystemPromptOverride, false)

	if snap.ApiVersion != "" {
		bg.Set("api_version", snap.ApiVersion)
	}
	if snap.Region != "" {
		bg.Set("region", snap.Region)
	}
	if snap.Plugin != "" {
		bg.Set("plugin", snap.Plugin)
	}
	if snap.BotId != "" {
		bg.Set("bot_id", snap.BotId)
	}

	bg.Set(common.KeyRequestBody, body)
	Relay(bg, spec.Format)

	if ctx.Err() != nil {
		// Timeout or cancel at async task layer (best-effort mapping).
		return http.StatusGatewayTimeout, []byte(`{"error":{"message":"async_task_timeout","type":"upstream_error"}}`), "application/json"
	}

	contentType = rec.Header().Get("Content-Type")
	return rec.Code, rec.Body.Bytes(), contentType
}

func isEventStreamContentType(ct string) bool {
	return strings.HasPrefix(ct, "text/event-stream")
}

func looksLikeSSE(body []byte) bool {
	b := bytes.TrimSpace(body)
	return bytes.HasPrefix(b, []byte("data:")) || bytes.Contains(b, []byte("\ndata:"))
}

func shouldFallbackToStream(status int, body []byte) bool {
	if status >= 200 && status < 300 {
		return false
	}
	msg := strings.ToLower(extractOpenAIErrorMessage(body))
	if msg == "" {
		msg = strings.ToLower(string(body))
	}
	return strings.Contains(msg, "stream") && (strings.Contains(msg, "only") || strings.Contains(msg, "required") || strings.Contains(msg, "must"))
}

func sseChatCompletionsToFinalJSON(sse []byte, fallbackModel string) ([]byte, bool) {
	maxBytes := int64(constant.AsyncTaskMaxResponseMB) << 20
	if maxBytes > 0 && int64(len(sse)) > maxBytes {
		return nil, false
	}
	// Minimal OpenAI-compatible SSE aggregator.
	// Supports: data: {ChatCompletionsStreamResponse}, data: [DONE]
	var (
		contentBuilder strings.Builder
		role           = "assistant"
		finishReason   string
		lastID         string
		lastModel      string
		created        int64
		usage          *dto.Usage
	)

	toolCalls := map[int]*dto.ToolCallResponse{}

	lines := strings.Split(string(sse), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}

		var chunk dto.ChatCompletionsStreamResponse
		if err := common.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Id != "" {
			lastID = chunk.Id
		}
		if chunk.Model != "" {
			lastModel = chunk.Model
		}
		if chunk.Created != 0 {
			created = chunk.Created
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Role != "" {
			role = delta.Role
		}
		if s := delta.GetContentString(); s != "" {
			contentBuilder.WriteString(s)
		}
		if len(delta.ToolCalls) > 0 {
			for _, tc := range delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				existing, ok := toolCalls[idx]
				if !ok {
					copyTC := tc
					copyTC.Index = nil
					toolCalls[idx] = &copyTC
					existing = &copyTC
				}
				if existing.ID == "" && tc.ID != "" {
					existing.ID = tc.ID
				}
				if existing.Type == nil && tc.Type != nil {
					existing.Type = tc.Type
				}
				if existing.Function.Name == "" && tc.Function.Name != "" {
					existing.Function.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					existing.Function.Arguments += tc.Function.Arguments
				}
			}
		}
		if chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason != "" {
			finishReason = *chunk.Choices[0].FinishReason
		}
	}

	modelName := lastModel
	if modelName == "" {
		modelName = fallbackModel
	}

	var toolCallsOut []any
	if len(toolCalls) > 0 {
		for i := 0; i < len(toolCalls); i++ {
			if tc, ok := toolCalls[i]; ok && tc != nil {
				toolCallsOut = append(toolCallsOut, map[string]any{
					"id":   tc.ID,
					"type": tc.Type,
					"function": map[string]any{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				})
			}
		}
	}

	message := map[string]any{
		"role":    role,
		"content": contentBuilder.String(),
	}
	if len(toolCallsOut) > 0 {
		message["tool_calls"] = toolCallsOut
	}

	resp := map[string]any{
		"id":      lastID,
		"object":  "chat.completion",
		"created": created,
		"model":   modelName,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}

	b, err := common.Marshal(resp)
	if err != nil {
		return nil, false
	}
	return b, true
}
