// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes bounds how much of an API response (success or error) is
// read into memory.
const maxResponseBytes = 4 << 20 // 4 MiB

// chatCompletionsPath is appended to the configured base URL path.
const chatCompletionsPath = "chat/completions"

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponseFormat struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model               string              `json:"model"`
	Messages            []chatMessage       `json:"messages"`
	Temperature         *float64            `json:"temperature,omitempty"`
	TopP                *float64            `json:"top_p,omitempty"`
	MaxTokens           *int64              `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64              `json:"max_completion_tokens,omitempty"`
	ResponseFormat      *chatResponseFormat `json:"response_format,omitempty"`
}

type chatUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
}

type chatResult struct {
	Content      string
	ResponseID   string
	ModelUsed    string
	FinishReason string
	Usage        *chatUsage
}

// chatResponse models the subset of the OpenAI-compatible chat completions
// response the provider needs. Unknown metadata fields are tolerated.
type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
}

type chatChoice struct {
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

const chargeNotice = "the generation may have incurred charges and requests are not retried automatically"

type client struct {
	httpClient *http.Client
	apiKey     string
	timeout    time.Duration
	endpoint   string
}

// newClient validates the API base URL, credentials, and timeout. baseURL must
// be an absolute http(s) URL without userinfo, query parameters, or fragment;
// its path is preserved as a prefix for the chat completions endpoint.
func newClient(baseURL, apiKey string, timeout time.Duration) (*client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, errors.New("invalid API base URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("API base URL must use the http or https scheme")
	case u.Host == "":
		return nil, errors.New("API base URL must include a host")
	case u.User != nil:
		return nil, errors.New("API base URL must not contain credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return nil, errors.New("API base URL must not contain query parameters or a fragment")
	case apiKey != "" && hasUnsafeHeaderValue(apiKey):
		return nil, errors.New("API key contains characters that cannot be used as an HTTP header value")
	case timeout <= 0:
		return nil, errors.New("API request timeout must be positive")
	}

	// Preserve the base path prefix and its escaping; a trailing slash on the
	// base URL must not produce a double slash.
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	endpoint := u.Scheme + "://" + u.Host + path + "/" + chatCompletionsPath

	return &client{
		httpClient: &http.Client{
			// Reject every redirect before any follow-up request is made so
			// credentials are never forwarded to another destination.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		apiKey:   apiKey,
		timeout:  timeout,
		endpoint: endpoint,
	}, nil
}

// hasUnsafeHeaderValue reports whether v contains bytes that are not allowed
// (or not meaningful) in an HTTP header value, preventing header injection.
func hasUnsafeHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if b := v[i]; b <= 0x20 || b >= 0x7f {
			return true
		}
	}
	return false
}

// generate performs a chat completions request and validates the response.
// Error diagnostics are intentionally generic: they never include the raw
// URL, request or response bodies, credentials, prompts, or unsanitized
// server-provided strings such as request IDs.
func (c *client) generate(ctx context.Context, req chatRequest) (chatResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	payload, err := json.Marshal(req)
	if err != nil {
		return chatResult{}, fmt.Errorf("encoding chat completions request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return chatResult{}, errors.New("building API request failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return chatResult{}, errors.New("API request timed out or was canceled; " + chargeNotice)
		}
		return chatResult{}, errors.New("API request failed; " + chargeNotice)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Bound the read for both success and error bodies.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return chatResult{}, errors.New("reading API response failed; " + chargeNotice)
	}
	if len(body) > maxResponseBytes {
		return chatResult{}, errors.New("API response exceeds the maximum supported size")
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return chatResult{}, errors.New("API returned an unexpected redirect response; redirects are not followed and " + chargeNotice)
		}
		return chatResult{}, fmt.Errorf("API request failed with HTTP status %d; %s", resp.StatusCode, chargeNotice)
	}

	var apiResp chatResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return chatResult{}, errors.New("API returned a malformed JSON response")
	}

	if len(apiResp.Choices) == 0 {
		return chatResult{}, errors.New("API response contains no choices")
	}
	if len(apiResp.Choices) > 1 {
		return chatResult{}, errors.New("API response contains multiple choices; only a single choice is supported")
	}
	choice := apiResp.Choices[0]
	if choice.Message.Role != "assistant" {
		return chatResult{}, errors.New("API response message role is not assistant")
	}
	if choice.Message.Content == "" {
		return chatResult{}, errors.New("API response contains empty content")
	}
	switch choice.FinishReason {
	case "stop":
	case "length":
		return chatResult{}, errors.New("generation was truncated (finish_reason length); " + chargeNotice)
	case "content_filter":
		return chatResult{}, errors.New("generation was filtered by the API (finish_reason content_filter)")
	case "tool_calls":
		return chatResult{}, errors.New("API returned tool-call output, which is not supported")
	case "":
		return chatResult{}, errors.New("API response is missing finish_reason")
	default:
		return chatResult{}, errors.New("API returned an unsupported finish_reason")
	}

	if err := validateUsage(apiResp.Usage); err != nil {
		return chatResult{}, err
	}

	content := choice.Message.Content
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		if err := validateJSONObject(content); err != nil {
			return chatResult{}, err
		}
	}

	return chatResult{
		Content:      content,
		ResponseID:   apiResp.ID,
		ModelUsed:    apiResp.Model,
		FinishReason: choice.FinishReason,
		Usage:        apiResp.Usage,
	}, nil
}

func validateUsage(usage *chatUsage) error {
	if usage == nil {
		return nil
	}
	if usage.PromptTokens != nil && *usage.PromptTokens < 0 {
		return errors.New("API response contains a negative prompt token count")
	}
	if usage.CompletionTokens != nil && *usage.CompletionTokens < 0 {
		return errors.New("API response contains a negative completion token count")
	}
	if usage.TotalTokens != nil && *usage.TotalTokens < 0 {
		return errors.New("API response contains a negative total token count")
	}
	return nil
}

// validateJSONObject checks that content parses as JSON whose top-level value
// is a non-null object. json.Unmarshal rejects malformed input and trailing
// content. The original content is never repaired or rewritten.
func validateJSONObject(content string) error {
	var root any
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		return errors.New("API response is not valid JSON as required by the json_object output format")
	}
	if _, ok := root.(map[string]any); !ok {
		return errors.New("API response is not a JSON object as required by the json_object output format")
	}
	return nil
}
