package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/looplj/axonhub/llm/transformer"
)

type TestModelRequest struct {
	ChannelID int    `json:"channel_id"`
	Model     string `json:"model" binding:"required"`
	Prompt    string `json:"prompt"`
}

type TestModelResponse struct {
	Success   bool           `json:"success"`
	Response  string         `json:"response"`
	LatencyMs int64          `json:"latency_ms"`
	Model     string         `json:"model"`
	Usage     *ModelUsage    `json:"usage,omitempty"`
	Error     string         `json:"error,omitempty"`
}

type ModelUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func TestModel(ctx context.Context, req TestModelRequest) (*TestModelResponse, error) {
	if req.Prompt == "" {
		req.Prompt = "Hello!"
	}

	if req.ChannelID > 0 {
		return testModelByChannel(ctx, req)
	}
	return testModelByGroup(ctx, req)
}

func testModelByChannel(ctx context.Context, req TestModelRequest) (*TestModelResponse, error) {
	channel, err := op.ChannelGet(req.ChannelID, ctx)
	if err != nil {
		return &TestModelResponse{
			Success: false,
			Error:   fmt.Sprintf("channel not found: %v", err),
		}, nil
	}

	return doTestModel(ctx, channel, req.Model, req.Prompt)
}

func testModelByGroup(ctx context.Context, req TestModelRequest) (*TestModelResponse, error) {
	group, err := op.GroupGetEnabledMap(req.Model, ctx)
	if err != nil {
		return &TestModelResponse{
			Success: false,
			Error:   fmt.Sprintf("model not found in any group: %v", err),
		}, nil
	}

	if len(group.Items) == 0 {
		return &TestModelResponse{
			Success: false,
			Error:   "no available channel for this model",
		}, nil
	}

	for _, item := range group.Items {
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil {
			continue
		}
		if !channel.Enabled {
			continue
		}
		result, err := doTestModel(ctx, channel, item.ModelName, req.Prompt)
		if err == nil && result.Success {
			return result, nil
		}
	}

	return &TestModelResponse{
		Success: false,
		Error:   "all channels failed for this model",
	}, nil
}

func doTestModel(ctx context.Context, channel *model.Channel, modelName string, prompt string) (*TestModelResponse, error) {
	baseURL := channel.GetBaseUrl()
	if baseURL == "" {
		return &TestModelResponse{
			Success: false,
			Error:   "no base url configured",
		}, nil
	}

	usedKey := channel.GetChannelKey()
	if usedKey.ChannelKey == "" {
		return &TestModelResponse{
			Success: false,
			Error:   "no available key",
		}, nil
	}

	apiURL := transformer.NormalizeBaseURL(baseURL, "v1") + "/chat/completions"
	if channel.Type == model.ChannelTypeDoubao {
		apiURL = transformer.NormalizeBaseURL(baseURL, "v3") + "/chat/completions"
	}

	requestBody := map[string]any{
		"model": modelName,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens": 100,
	}

	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return &TestModelResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to marshal request: %v", err),
		}, nil
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return &TestModelResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+usedKey.ChannelKey)
	applyCustomHeaders(httpReq, *channel)

	httpClient, err := ChannelHttpClient(channel)
	if err != nil {
		return &TestModelResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to get http client: %v", err),
		}, nil
	}

	start := time.Now()
	resp, err := httpClient.Do(httpReq)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return &TestModelResponse{
			Success:   false,
			LatencyMs: latency,
			Error:     fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return &TestModelResponse{
			Success:   false,
			LatencyMs: latency,
			Error:     fmt.Sprintf("failed to read response: %v", err),
		}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return &TestModelResponse{
			Success:   false,
			LatencyMs: latency,
			Model:     modelName,
			Error:     fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody)),
		}, nil
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return &TestModelResponse{
			Success:   false,
			LatencyMs: latency,
			Model:     modelName,
			Error:     fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	content := ""
	if len(chatResp.Choices) > 0 {
		content = chatResp.Choices[0].Message.Content
	}

	return &TestModelResponse{
		Success:   true,
		Response:  content,
		LatencyMs: latency,
		Model:     modelName,
		Usage: &ModelUsage{
			InputTokens:  chatResp.Usage.PromptTokens,
			OutputTokens: chatResp.Usage.CompletionTokens,
		},
	}, nil
}
