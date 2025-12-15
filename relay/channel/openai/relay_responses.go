package openai

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func mapResponsesStopReasonToClaude(reason string) string {
	switch reason {
	case "", "stop":
		return "end"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return reason
	}
}

func responsesOutputText(outputs []dto.ResponsesOutput) string {
	var sb strings.Builder
	for _, out := range outputs {
		for _, c := range out.Content {
			if c.Text != "" {
				sb.WriteString(c.Text)
			}
		}
	}
	return sb.String()
}

func responsesUsageToClaude(u *dto.Usage) *dto.ClaudeUsage {
	if u == nil {
		return nil
	}
	usage := &dto.ClaudeUsage{
		InputTokens:                 u.InputTokens,
		CacheReadInputTokens:        u.PromptTokensDetails.CachedTokens,
		CacheCreationInputTokens:    u.ClaudeCacheCreation1hTokens,
		OutputTokens:                u.OutputTokens,
		ClaudeCacheCreation5mTokens: u.ClaudeCacheCreation5mTokens,
		ClaudeCacheCreation1hTokens: u.ClaudeCacheCreation1hTokens,
	}
	if usage.InputTokens == 0 {
		usage.InputTokens = u.PromptTokens
	}
	if usage.OutputTokens == 0 {
		usage.OutputTokens = u.CompletionTokens
	}
	if u.ClaudeCacheCreation5mTokens > 0 || u.ClaudeCacheCreation1hTokens > 0 {
		usage.CacheCreation = &dto.ClaudeCacheCreationUsage{
			Ephemeral5mInputTokens: u.ClaudeCacheCreation5mTokens,
			Ephemeral1hInputTokens: u.ClaudeCacheCreation1hTokens,
		}
	}
	return usage
}

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	// 部分上游（即使 stream=false）也可能返回 SSE 文本（以 "event:" 开头）。
	// 若检测到 SSE，优先尝试解析出最终响应，并按非流式 JSON 兼容返回。
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") ||
		bytes.HasPrefix(bytes.TrimSpace(responseBody), []byte("event:")) ||
		bytes.HasPrefix(bytes.TrimSpace(responseBody), []byte("data:")) {
		if info != nil && info.IsStream {
			clone := *resp
			clone.Body = io.NopCloser(bytes.NewReader(responseBody))
			return OaiResponsesStreamHandler(c, info, &clone)
		}
		finalResp, usageFromSSE, parseErr := parseResponsesSSE(responseBody)
		if parseErr != nil {
			preview := string(responseBody)
			if len(preview) > 2000 {
				preview = preview[:2000] + "...(truncated)"
			}
			logger.LogError(c, fmt.Sprintf("responses parse failed, status=%d, body=%s, err=%v", resp.StatusCode, preview, parseErr))
			return nil, types.NewOpenAIError(parseErr, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		c.Writer.Header().Set("Content-Type", "application/json")
		jsonBody, err := common.Marshal(finalResp)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		if _, err = c.Writer.Write(jsonBody); err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		return usageFromSSE, nil
	}

	// 非 2xx 直接包装为 OpenAI 风格错误
	if resp.StatusCode >= http.StatusBadRequest {
		message := string(responseBody)
		code := ""
		var bodyObj map[string]any
		if err := common.Unmarshal(responseBody, &bodyObj); err == nil {
			if detail, ok := bodyObj["detail"]; ok {
				switch v := detail.(type) {
				case string:
					message = v
				case map[string]any:
					if msg, ok := v["message"].(string); ok {
						message = msg
					}
					if cStr, ok := v["code"].(string); ok {
						code = cStr
					}
				}
			}
		}
		openaiErr := types.OpenAIError{
			Type:    "invalid_request_error",
			Message: message,
			Code:    code,
		}
		return nil, types.WithOpenAIError(openaiErr, resp.StatusCode)
	}

	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		preview := string(responseBody)
		if len(preview) > 2000 {
			preview = preview[:2000] + "...(truncated)"
		}
		logger.LogError(c, fmt.Sprintf("responses parse failed, status=%d, body=%s", resp.StatusCode, preview))
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	if responsesResponse.HasImageGenerationCall() {
		c.Set("image_generation_call", true)
		c.Set("image_generation_call_quality", responsesResponse.GetQuality())
		c.Set("image_generation_call_size", responsesResponse.GetSize())
	}

	usage := dto.Usage{}
	if responsesResponse.Usage != nil {
		usage.PromptTokens = responsesResponse.Usage.InputTokens
		usage.CompletionTokens = responsesResponse.Usage.OutputTokens
		usage.TotalTokens = responsesResponse.Usage.TotalTokens
		if responsesResponse.Usage.InputTokensDetails != nil {
			usage.PromptTokensDetails.CachedTokens = responsesResponse.Usage.InputTokensDetails.CachedTokens
		}
	}

	originalPath := c.GetString(string(constant.ContextKeyOriginalPath))
	if originalPath == "" && info != nil {
		originalPath = info.RequestURLPath
	}
	shouldConvertToClaude := info != nil &&
		info.ChannelType == constant.ChannelTypeCodex &&
		strings.HasPrefix(originalPath, "/v1/messages")

	if shouldConvertToClaude {
		text := responsesOutputText(responsesResponse.Output)
		stopReason := mapResponsesStopReasonToClaude("stop")
		claudeResp := dto.ClaudeResponse{
			Id:         responsesResponse.ID,
			Type:       "message",
			Role:       "assistant",
			Model:      info.UpstreamModelName,
			StopReason: stopReason,
			Content: []dto.ClaudeMediaMessage{
				{Type: "text", Text: common.GetPointer(text)},
			},
			Usage: responsesUsageToClaude(&usage),
		}
		c.Writer.Header().Set("Content-Type", "application/json")
		c.JSON(http.StatusOK, claudeResp)
		return &usage, nil
	}

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	if info == nil || info.ResponsesUsageInfo == nil || info.ResponsesUsageInfo.BuiltInTools == nil {
		return &usage, nil
	}
	// 解析 Tools 用量
	for _, tool := range responsesResponse.Tools {
		buildToolinfo, ok := info.ResponsesUsageInfo.BuiltInTools[common.Interface2String(tool["type"])]
		if !ok || buildToolinfo == nil {
			logger.LogError(c, fmt.Sprintf("BuiltInTools not found for tool type: %v", tool["type"]))
			continue
		}
		buildToolinfo.CallCount++
	}
	return &usage, nil
}

// parseResponsesSSE 将 text/event-stream 解析为最终的 Responses 响应（用于非流式客户端兜底）。
func parseResponsesSSE(body []byte) (*dto.OpenAIResponsesResponse, *dto.Usage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	var dataLines []string

	handleEvent := func(data string) (*dto.OpenAIResponsesResponse, *dto.Usage, error) {
		var streamResp dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResp); err != nil {
			return nil, nil, err
		}
		if streamResp.Type == dto.ResponsesOutputTypeItemDone {
			return nil, nil, nil
		}
		if streamResp.Response == nil {
			return nil, nil, nil
		}
		usage := &dto.Usage{}
		if streamResp.Response.Usage != nil {
			usage.PromptTokens = streamResp.Response.Usage.InputTokens
			usage.CompletionTokens = streamResp.Response.Usage.OutputTokens
			usage.TotalTokens = streamResp.Response.Usage.TotalTokens
			if streamResp.Response.Usage.InputTokensDetails != nil {
				usage.PromptTokensDetails.CachedTokens = streamResp.Response.Usage.InputTokensDetails.CachedTokens
			}
		}
		return streamResp.Response, usage, nil
	}

	var lastResp *dto.OpenAIResponsesResponse
	var lastUsage *dto.Usage

	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		resp, usage, err := handleEvent(data)
		if err != nil {
			return err
		}
		if resp != nil {
			lastResp = resp
		}
		if usage != nil {
			lastUsage = usage
		}
		dataLines = dataLines[:0]
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
		if line == "" {
			if err := flush(); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	if err := flush(); err != nil {
		return nil, nil, err
	}

	if lastResp == nil {
		return nil, nil, fmt.Errorf("no response found in SSE")
	}
	return lastResp, lastUsage, nil
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	// 如果 upstream 返回错误状态码，直接读取并包装为 OpenAI 风格错误
	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		message := string(bodyBytes)
		code := ""
		// 尝试解析 detail 字段
		var bodyObj map[string]any
		if err := common.Unmarshal(bodyBytes, &bodyObj); err == nil {
			if detail, ok := bodyObj["detail"]; ok {
				switch v := detail.(type) {
				case string:
					message = v
				case map[string]any:
					if msg, ok := v["message"].(string); ok {
						message = msg
					}
					if cStr, ok := v["code"].(string); ok {
						code = cStr
					}
				}
			}
		}
		openaiErr := types.OpenAIError{
			Type:    "invalid_request_error",
			Message: message,
			Code:    code,
		}
		return nil, types.WithOpenAIError(openaiErr, resp.StatusCode)
	}

	var usage = &dto.Usage{}
	var responseTextBuilder strings.Builder
	originalPath := c.GetString(string(constant.ContextKeyOriginalPath))
	if originalPath == "" {
		originalPath = info.RequestURLPath
	}
	shouldConvertToClaude := info != nil &&
		info.ChannelType == constant.ChannelTypeCodex &&
		strings.HasPrefix(originalPath, "/v1/messages")
	shouldConvertToChat := !shouldConvertToClaude && strings.HasPrefix(originalPath, "/v1/chat/completions")
	responseID := helper.GetResponseID(c)
	createdAt := time.Now().Unix()
	sentRoleChunk := false
	doneSent := false
	messageStarted := false
	contentStarted := false

	// ensure SSE headers
	helper.SetEventStreamHeaders(c)

	helper.StreamScannerHandler(c, resp, info, func(data string) bool {

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err == nil {
			if shouldConvertToClaude {
				switch streamResponse.Type {
				case "response.output_text.delta":
					if !messageStarted {
						start := dto.ClaudeResponse{
							Type: "message_start",
							Message: &dto.ClaudeMediaMessage{
								Id:    responseID,
								Role:  "assistant",
								Model: info.UpstreamModelName,
							},
						}
						helper.ClaudeData(c, start)
						messageStarted = true
					}
					if !contentStarted {
						start := dto.ClaudeResponse{
							Type: "content_block_start",
							ContentBlock: &dto.ClaudeMediaMessage{
								Type: "text",
							},
						}
						start.SetIndex(0)
						helper.ClaudeData(c, start)
						contentStarted = true
					}
					if streamResponse.Delta != "" {
						deltaText := streamResponse.Delta
						delta := dto.ClaudeResponse{
							Type: "content_block_delta",
							Delta: &dto.ClaudeMediaMessage{
								Type: "text_delta",
								Text: &deltaText,
							},
						}
						delta.SetIndex(0)
						helper.ClaudeData(c, delta)
					}
				case "response.completed":
					if !messageStarted {
						start := dto.ClaudeResponse{
							Type: "message_start",
							Message: &dto.ClaudeMediaMessage{
								Id:    responseID,
								Role:  "assistant",
								Model: info.UpstreamModelName,
							},
						}
						helper.ClaudeData(c, start)
						messageStarted = true
					}
					if contentStarted {
						stop := dto.ClaudeResponse{Type: "content_block_stop"}
						stop.SetIndex(0)
						helper.ClaudeData(c, stop)
					}
					stopReason := mapResponsesStopReasonToClaude("stop")
					if streamResponse.Response != nil && streamResponse.Response.IncompleteDetails != nil {
						stopReason = mapResponsesStopReasonToClaude(streamResponse.Response.IncompleteDetails.Reasoning)
					}
					var usageForEvent *dto.ClaudeUsage
					if streamResponse.Response != nil {
						usageForEvent = responsesUsageToClaude(streamResponse.Response.Usage)
					}
					messageDelta := dto.ClaudeResponse{
						Type: "message_delta",
						Delta: &dto.ClaudeMediaMessage{
							StopReason: &stopReason,
						},
						Usage: usageForEvent,
					}
					helper.ClaudeData(c, messageDelta)
					helper.ClaudeData(c, dto.ClaudeResponse{Type: "message_stop"})
				}
			} else if shouldConvertToChat {
				// First chunk with role
				if !sentRoleChunk {
					startChunk := helper.GenerateStartEmptyResponse(responseID, createdAt, info.UpstreamModelName, nil)
					_ = helper.ObjectData(c, startChunk)
					sentRoleChunk = true
				}
				switch streamResponse.Type {
				case "response.output_text.delta":
					// 文本增量
					chunk := dto.ChatCompletionsStreamResponse{
						Id:                responseID,
						Object:            "chat.completion.chunk",
						Created:           createdAt,
						Model:             info.UpstreamModelName,
						SystemFingerprint: nil,
						Choices: []dto.ChatCompletionsStreamResponseChoice{
							{
								Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
									Content: common.GetPointer(streamResponse.Delta),
								},
							},
						},
					}
					_ = helper.ObjectData(c, chunk)
				case "response.completed":
					// 最终结束块，附带 usage
					finishReason := "stop"
					stopChunk := helper.GenerateStopResponse(responseID, createdAt, info.UpstreamModelName, finishReason)
					if streamResponse.Response != nil && streamResponse.Response.Usage != nil {
						if streamResponse.Response.Usage.InputTokens != 0 {
							usage.PromptTokens = streamResponse.Response.Usage.InputTokens
						}
						if streamResponse.Response.Usage.OutputTokens != 0 {
							usage.CompletionTokens = streamResponse.Response.Usage.OutputTokens
						}
						if streamResponse.Response.Usage.TotalTokens != 0 {
							usage.TotalTokens = streamResponse.Response.Usage.TotalTokens
						}
						if streamResponse.Response.Usage.InputTokensDetails != nil {
							usage.PromptTokensDetails.CachedTokens = streamResponse.Response.Usage.InputTokensDetails.CachedTokens
						}
						stopChunk.Usage = usage
					}
					_ = helper.ObjectData(c, stopChunk)
					helper.Done(c)
					doneSent = true
				}
			} else {
				sendResponsesStreamData(c, streamResponse, data)
			}
			switch streamResponse.Type {
			case "response.completed":
				if streamResponse.Response != nil {
					if streamResponse.Response.Usage != nil {
						if streamResponse.Response.Usage.InputTokens != 0 {
							usage.PromptTokens = streamResponse.Response.Usage.InputTokens
						}
						if streamResponse.Response.Usage.OutputTokens != 0 {
							usage.CompletionTokens = streamResponse.Response.Usage.OutputTokens
						}
						if streamResponse.Response.Usage.TotalTokens != 0 {
							usage.TotalTokens = streamResponse.Response.Usage.TotalTokens
						}
						if streamResponse.Response.Usage.InputTokensDetails != nil {
							usage.PromptTokensDetails.CachedTokens = streamResponse.Response.Usage.InputTokensDetails.CachedTokens
						}
					}
					if streamResponse.Response.HasImageGenerationCall() {
						c.Set("image_generation_call", true)
						c.Set("image_generation_call_quality", streamResponse.Response.GetQuality())
						c.Set("image_generation_call_size", streamResponse.Response.GetSize())
					}
				}
			case "response.output_text.delta":
				// 处理输出文本
				responseTextBuilder.WriteString(streamResponse.Delta)
			case dto.ResponsesOutputTypeItemDone:
				// 函数调用处理
				if streamResponse.Item != nil {
					switch streamResponse.Item.Type {
					case dto.BuildInCallWebSearchCall:
						if info != nil && info.ResponsesUsageInfo != nil && info.ResponsesUsageInfo.BuiltInTools != nil {
							if webSearchTool, exists := info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview]; exists && webSearchTool != nil {
								webSearchTool.CallCount++
							}
						}
					}
				}
			}
		} else {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
		}
		return true
	})

	// 如果 upstream 提前结束但未收到 response.completed，也补一个 [DONE]
	if shouldConvertToChat && !doneSent {
		helper.Done(c)
	}

	if usage.CompletionTokens == 0 {
		// 计算输出文本的 token 数量
		tempStr := responseTextBuilder.String()
		if len(tempStr) > 0 {
			// 非正常结束，使用输出文本的 token 数量
			completionTokens := service.CountTextToken(tempStr, info.UpstreamModelName)
			usage.CompletionTokens = completionTokens
		}
	}

	if usage.PromptTokens == 0 && usage.CompletionTokens != 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	return usage, nil
}
