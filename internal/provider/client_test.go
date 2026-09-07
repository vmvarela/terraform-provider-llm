// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testChatResponse(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	resp := map[string]any{
		"id": "resp-123",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "hello world",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	}
	if mutate != nil {
		mutate(resp)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal test response: %v", err)
	}
	return string(body)
}

func chatClient(t *testing.T, baseURL, apiKey string) *client {
	t.Helper()
	c, err := newClient(baseURL, apiKey, 10*time.Second)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

func TestNewClientValidation(t *testing.T) {
	validTimeout := 30 * time.Second
	tests := []struct {
		name    string
		baseURL string
		apiKey  string
		timeout time.Duration
		wantErr bool
	}{
		{name: "valid https", baseURL: "https://api.example.com/v1", timeout: validTimeout},
		{name: "valid http", baseURL: "http://localhost:8080", timeout: validTimeout},
		{name: "valid with trailing slash", baseURL: "https://api.example.com/v1/", timeout: validTimeout},
		{name: "empty base URL", baseURL: "", timeout: validTimeout, wantErr: true},
		{name: "unsupported scheme", baseURL: "ftp://api.example.com", timeout: validTimeout, wantErr: true},
		{name: "https without host", baseURL: "https:///v1", timeout: validTimeout, wantErr: true},
		{name: "relative URL", baseURL: "/v1", timeout: validTimeout, wantErr: true},
		{name: "userinfo", baseURL: "https://user:pass@api.example.com", timeout: validTimeout, wantErr: true},
		{name: "query", baseURL: "https://api.example.com/v1?x=1", timeout: validTimeout, wantErr: true},
		{name: "bare query marker", baseURL: "https://api.example.com/v1?", timeout: validTimeout, wantErr: true},
		{name: "fragment", baseURL: "https://api.example.com/v1#frag", timeout: validTimeout, wantErr: true},
		{name: "opaque URL", baseURL: "https:opaque", timeout: validTimeout, wantErr: true},
		{name: "zero timeout", baseURL: "https://api.example.com", timeout: 0, wantErr: true},
		{name: "negative timeout", baseURL: "https://api.example.com", timeout: -time.Second, wantErr: true},
		{name: "key with newline", baseURL: "https://api.example.com", apiKey: "abc\n", timeout: validTimeout, wantErr: true},
		{name: "key with CRLF injection", baseURL: "https://api.example.com", apiKey: "abc\r\nX-Evil: 1", timeout: validTimeout, wantErr: true},
		{name: "key with space", baseURL: "https://api.example.com", apiKey: "abc def", timeout: validTimeout, wantErr: true},
		{name: "key with non-ascii", baseURL: "https://api.example.com", apiKey: "clé", timeout: validTimeout, wantErr: true},
		{name: "empty key allowed", baseURL: "https://api.example.com", timeout: validTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := newClient(tt.baseURL, tt.apiKey, tt.timeout)
			if (err != nil) != tt.wantErr {
				t.Fatalf("newClient() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && c != nil {
				t.Fatalf("newClient() returned client %v alongside error", c)
			}
			if !tt.wantErr && c == nil {
				t.Fatal("newClient() returned nil client without error")
			}
		})
	}
}

func TestClientEndpointURL(t *testing.T) {
	tests := []struct {
		name      string
		baseURL   string
		wantPath  string
		wantQuery string
	}{
		{name: "no path", baseURL: "PLACEHOLDER", wantPath: "/chat/completions"},
		{name: "v1 path", baseURL: "PLACEHOLDER/v1", wantPath: "/v1/chat/completions"},
		{name: "v1 trailing slash", baseURL: "PLACEHOLDER/v1/", wantPath: "/v1/chat/completions"},
		{name: "root trailing slash", baseURL: "PLACEHOLDER/", wantPath: "/chat/completions"},
		{name: "nested path", baseURL: "PLACEHOLDER/openai/v1", wantPath: "/openai/v1/chat/completions"},
		{name: "escaped path preserved", baseURL: "PLACEHOLDER/v1%20space", wantPath: "/v1%20space/chat/completions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.EscapedPath()
				gotQuery = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, testChatResponse(t, nil))
			}))
			defer srv.Close()

			base := strings.Replace(tt.baseURL, "PLACEHOLDER", srv.URL, 1)
			c := chatClient(t, base, "")
			_, err := c.generate(context.Background(), chatRequest{
				Model:    "m",
				Messages: []chatMessage{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if gotPath != tt.wantPath {
				t.Errorf("request path = %q, want %q", gotPath, tt.wantPath)
			}
			if gotQuery != tt.wantQuery {
				t.Errorf("request query = %q, want %q", gotQuery, tt.wantQuery)
			}
		})
	}
}

func TestClientRequestConstruction(t *testing.T) {
	tests := []struct {
		name string
		req  chatRequest
		want map[string]any // expected decoded JSON body
	}{
		{
			name: "required fields only",
			req: chatRequest{
				Model:    "gpt-x",
				Messages: []chatMessage{{Role: "user", Content: "hi"}},
			},
			want: map[string]any{
				"model": "gpt-x",
				"messages": []any{map[string]any{
					"role":    "user",
					"content": "hi",
				}},
			},
		},
		{
			name: "zero-valued optionals included via pointer",
			req: chatRequest{
				Model:               "gpt-x",
				Messages:            []chatMessage{{Role: "user", Content: "hi"}},
				Temperature:         ptr(0.0),
				TopP:                ptr(0.0),
				MaxTokens:           ptr(int64(0)),
				MaxCompletionTokens: ptr(int64(0)),
			},
			want: map[string]any{
				"model":                 "gpt-x",
				"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
				"temperature":           0.0,
				"top_p":                 0.0,
				"max_tokens":            0.0,
				"max_completion_tokens": 0.0,
			},
		},
		{
			name: "all optionals set",
			req: chatRequest{
				Model:               "gpt-x",
				Messages:            []chatMessage{{Role: "user", Content: "hi"}},
				Temperature:         ptr(0.7),
				TopP:                ptr(0.9),
				MaxTokens:           ptr(int64(100)),
				MaxCompletionTokens: ptr(int64(50)),
				ResponseFormat:      &chatResponseFormat{Type: "text"},
			},
			want: map[string]any{
				"model":                 "gpt-x",
				"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
				"temperature":           0.7,
				"top_p":                 0.9,
				"max_tokens":            100.0,
				"max_completion_tokens": 50.0,
				"response_format":       map[string]any{"type": "text"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				gotMethod    string
				gotAuth      string
				gotCT        string
				gotBody      map[string]any
				requestCount int64
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt64(&requestCount, 1)
				gotMethod = r.Method
				gotAuth = r.Header.Get("Authorization")
				gotCT = r.Header.Get("Content-Type")
				var m map[string]any
				if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				gotBody = m
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, testChatResponse(t, func(r map[string]any) {
					r["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] = `{"ok": true}`
				}))
			}))
			defer srv.Close()

			c := chatClient(t, srv.URL, "secret-key")
			if _, err := c.generate(context.Background(), tt.req); err != nil {
				t.Fatalf("generate: %v", err)
			}
			if n := atomic.LoadInt64(&requestCount); n != 1 {
				t.Errorf("request count = %d, want 1", n)
			}
			if gotMethod != http.MethodPost {
				t.Errorf("method = %q, want POST", gotMethod)
			}
			if gotCT != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", gotCT)
			}
			if gotAuth != "Bearer secret-key" {
				t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-key")
			}
			gotJSON, err := json.Marshal(gotBody)
			if err != nil {
				t.Fatalf("marshal captured body: %v", err)
			}
			wantJSON, err := json.Marshal(tt.want)
			if err != nil {
				t.Fatalf("marshal want body: %v", err)
			}
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("request body:\n got %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestClientNoAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, testChatResponse(t, nil))
	}))
	defer srv.Close()

	c := chatClient(t, srv.URL, "")
	if _, err := c.generate(context.Background(), chatRequest{
		Model:    "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty", gotAuth)
	}
}

func TestClientRejectsRedirects(t *testing.T) {
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect destination must never be called")
	}))
	defer dest.Close()

	var redirects int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&redirects, 1)
		http.Redirect(w, r, dest.URL+"/chat/completions", http.StatusFound)
	}))
	defer srv.Close()

	c := chatClient(t, srv.URL, "secret-key")
	_, err := c.generate(context.Background(), chatRequest{
		Model:    "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("generate() error = nil, want redirect rejection")
	}
	if n := atomic.LoadInt64(&redirects); n != 1 {
		t.Errorf("origin request count = %d, want 1", n)
	}
	if strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), dest.URL) {
		t.Errorf("redirect error leaks URLs: %q", err.Error())
	}
}

func TestClientTimeoutAndCancel(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	t.Run("timeout", func(t *testing.T) {
		c, err := newClient(srv.URL, "", 10*time.Millisecond)
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		_, err = c.generate(context.Background(), chatRequest{
			Model:    "m",
			Messages: []chatMessage{{Role: "user", Content: "hi"}},
		})
		if err == nil {
			t.Fatal("generate() error = nil, want timeout")
		}
	})

	t.Run("context cancel", func(t *testing.T) {
		c := chatClient(t, srv.URL, "")
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			cancel()
		}()
		_, err := c.generate(ctx, chatRequest{
			Model:    "m",
			Messages: []chatMessage{{Role: "user", Content: "hi"}},
		})
		if err == nil {
			t.Fatal("generate() error = nil, want cancellation error")
		}
	})
}

func TestClientOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("a", 64*1024)
		for written := 0; written <= maxResponseBytes+1; written += len(chunk) {
			_, _ = fmt.Fprint(w, chunk)
		}
	}))
	defer srv.Close()

	c := chatClient(t, srv.URL, "")
	_, err := c.generate(context.Background(), chatRequest{
		Model:    "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("generate() error = nil, want oversized-body error")
	}
	if !strings.Contains(err.Error(), "maximum supported size") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClientErrorResponseRedaction(t *testing.T) {
	const (
		key    = "sentinel-api-key"
		prompt = "top-secret prompt"
	)
	leakBody := fmt.Sprintf(`{"error":{"message":"bad request for prompt %s","type":"invalid_request_error","code":"x","request_id":"req_abc123"},"url":"%s"}`, prompt, "https://evil.example/leak")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, leakBody)
	}))
	defer srv.Close()

	c := chatClient(t, srv.URL, key)
	_, err := c.generate(context.Background(), chatRequest{
		Model:    "m",
		Messages: []chatMessage{{Role: "user", Content: prompt}},
	})
	if err == nil {
		t.Fatal("generate() error = nil, want HTTP error")
	}
	for _, leak := range []string{key, prompt, "req_abc123", "evil.example", "invalid_request_error", "bad request for prompt"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error message leaks %q: %q", leak, err.Error())
		}
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error message missing status code: %q", err.Error())
	}
}

func TestClientRawTransportErrorRedaction(t *testing.T) {
	// Close the connection without a response so the transport layer fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	c := chatClient(t, srv.URL, "secret-key")
	_, err := c.generate(context.Background(), chatRequest{
		Model:    "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("generate() error = nil, want transport error")
	}
	if strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("transport error leaks URL or credentials: %q", err.Error())
	}
}

func TestGenerateResponseValidation(t *testing.T) {
	tests := []struct {
		name       string
		body       func(t *testing.T) string
		wantErr    string // substring of expected error
		wantResult *chatResult
	}{
		{
			name: "success full metadata",
			body: func(t *testing.T) string { return testChatResponse(t, nil) },
			wantResult: &chatResult{
				Content:      "hello world",
				ResponseID:   "resp-123",
				ModelUsed:    "",
				FinishReason: "stop",
				Usage: &chatUsage{
					PromptTokens:     ptr(int64(10)),
					CompletionTokens: ptr(int64(5)),
					TotalTokens:      ptr(int64(15)),
				},
			},
		},
		{
			name: "model in response",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) { r["model"] = "gpt-x" })
			},
			wantResult: &chatResult{
				Content:      "hello world",
				ResponseID:   "resp-123",
				ModelUsed:    "gpt-x",
				FinishReason: "stop",
				Usage: &chatUsage{
					PromptTokens:     ptr(int64(10)),
					CompletionTokens: ptr(int64(5)),
					TotalTokens:      ptr(int64(15)),
				},
			},
		},
		{
			name: "optional metadata missing",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					delete(r, "id")
					delete(r, "usage")
				})
			},
			wantResult: &chatResult{
				Content:      "hello world",
				ResponseID:   "",
				FinishReason: "stop",
				Usage:        nil,
			},
		},
		{
			name: "usage null treated as missing",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) { r["usage"] = nil })
			},
			wantResult: &chatResult{
				Content:      "hello world",
				ResponseID:   "resp-123",
				FinishReason: "stop",
				Usage:        nil,
			},
		},
		{
			name: "usage zero counts preserved",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["usage"] = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
				})
			},
			wantResult: &chatResult{
				Content:      "hello world",
				ResponseID:   "resp-123",
				FinishReason: "stop",
				Usage: &chatUsage{
					PromptTokens:     ptr(int64(0)),
					CompletionTokens: ptr(int64(0)),
					TotalTokens:      ptr(int64(0)),
				},
			},
		},
		{
			name: "usage partially missing",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["usage"] = map[string]any{"prompt_tokens": 3}
				})
			},
			wantResult: &chatResult{
				Content:      "hello world",
				ResponseID:   "resp-123",
				FinishReason: "stop",
				Usage:        &chatUsage{PromptTokens: ptr(int64(3))},
			},
		},
		{
			name: "unknown metadata tolerated",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["created"] = 123
					r["system_fingerprint"] = "fp"
					r["object"] = "chat.completion"
				})
			},
			wantResult: &chatResult{
				Content:      "hello world",
				ResponseID:   "resp-123",
				FinishReason: "stop",
				Usage: &chatUsage{
					PromptTokens:     ptr(int64(10)),
					CompletionTokens: ptr(int64(5)),
					TotalTokens:      ptr(int64(15)),
				},
			},
		},
		{
			name: "negative prompt tokens",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["usage"] = map[string]any{"prompt_tokens": -1}
				})
			},
			wantErr: "negative prompt token count",
		},
		{
			name: "negative completion tokens",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["usage"] = map[string]any{"completion_tokens": -1}
				})
			},
			wantErr: "negative completion token count",
		},
		{
			name: "negative total tokens",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["usage"] = map[string]any{"total_tokens": -5}
				})
			},
			wantErr: "negative total token count",
		},
		{
			name: "no choices",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) { r["choices"] = []any{} })
			},
			wantErr: "no choices",
		},
		{
			name: "multiple choices",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["choices"] = []any{r["choices"].([]any)[0], r["choices"].([]any)[0]}
				})
			},
			wantErr: "multiple choices",
		},
		{
			name: "missing choices",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) { delete(r, "choices") })
			},
			wantErr: "no choices",
		},
		{
			name: "wrong role",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["role"] = "user"
				})
			},
			wantErr: "role is not assistant",
		},
		{
			name: "empty content",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] = ""
				})
			},
			wantErr: "empty content",
		},
		{
			name: "missing content",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					delete(r["choices"].([]any)[0].(map[string]any)["message"].(map[string]any), "content")
				})
			},
			wantErr: "empty content",
		},
		{
			name: "finish_reason length is truncation",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["choices"].([]any)[0].(map[string]any)["finish_reason"] = "length"
				})
			},
			wantErr: "truncated",
		},
		{
			name: "finish_reason content_filter",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["choices"].([]any)[0].(map[string]any)["finish_reason"] = "content_filter"
				})
			},
			wantErr: "filtered",
		},
		{
			name: "finish_reason tool_calls rejected",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["choices"].([]any)[0].(map[string]any)["finish_reason"] = "tool_calls"
				})
			},
			wantErr: "tool-call",
		},
		{
			name: "unsupported finish_reason",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					r["choices"].([]any)[0].(map[string]any)["finish_reason"] = "function_call"
				})
			},
			wantErr: "unsupported finish_reason",
		},
		{
			name: "missing finish_reason",
			body: func(t *testing.T) string {
				return testChatResponse(t, func(r map[string]any) {
					delete(r["choices"].([]any)[0].(map[string]any), "finish_reason")
				})
			},
			wantErr: "missing finish_reason",
		},
		{
			name:    "malformed JSON",
			body:    func(t *testing.T) string { return `{"choices":` },
			wantErr: "malformed JSON",
		},
		{
			name:    "empty body",
			body:    func(t *testing.T) string { return "" },
			wantErr: "malformed JSON",
		},
		{
			name:    "not a JSON object",
			body:    func(t *testing.T) string { return `["choice"]` },
			wantErr: "malformed JSON",
		},
		{
			name:    "truncated response stream",
			body:    func(t *testing.T) string { return `{"id":"resp-123","choices":[{"inde` },
			wantErr: "malformed JSON",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tt.body(t))
			}))
			defer srv.Close()

			c := chatClient(t, srv.URL, "")
			got, err := c.generate(context.Background(), chatRequest{
				Model:    "m",
				Messages: []chatMessage{{Role: "user", Content: "hi"}},
			})
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("generate() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("generate() error = %q, want containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if got.Content != tt.wantResult.Content ||
				got.ResponseID != tt.wantResult.ResponseID ||
				got.ModelUsed != tt.wantResult.ModelUsed ||
				got.FinishReason != tt.wantResult.FinishReason {
				t.Errorf("generate() = %+v, want %+v", got, *tt.wantResult)
			}
			if (got.Usage == nil) != (tt.wantResult.Usage == nil) {
				t.Fatalf("generate() usage = %+v, want %+v", got.Usage, tt.wantResult.Usage)
			}
			if got.Usage != nil {
				gotU, _ := json.Marshal(got.Usage)
				wantU, _ := json.Marshal(tt.wantResult.Usage)
				if string(gotU) != string(wantU) {
					t.Errorf("usage:\n got %s\nwant %s", gotU, wantU)
				}
			}
		})
	}
}

func TestGenerateJSONObjectValidation(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "valid object", content: `{"key": "value"}`},
		{name: "empty object", content: `{}`},
		{name: "nested object", content: `{"outer": {"inner": [1, 2]}}`},
		{name: "unicode", content: `{"é": "ü"}`},
		{name: "array rejected", content: `[1, 2, 3]`, wantErr: "not a JSON object"},
		{name: "string rejected", content: `"text"`, wantErr: "not a JSON object"},
		{name: "number rejected", content: `42`, wantErr: "not a JSON object"},
		{name: "boolean rejected", content: `true`, wantErr: "not a JSON object"},
		{name: "null rejected", content: `null`, wantErr: "not a JSON object"},
		{name: "trailing JSON rejected", content: `{"a":1}{"b":2}`, wantErr: "not valid JSON"},
		{name: "trailing garbage rejected", content: `{"a":1} extra`, wantErr: "not valid JSON"},
		{name: "code fence rejected", content: "```json\n{\"a\":1}\n```", wantErr: "not valid JSON"},
		{name: "empty rejected", content: "", wantErr: "empty content"},
		{name: "malformed rejected", content: `{"a":`, wantErr: "not valid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, testChatResponse(t, func(r map[string]any) {
					r["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] = tt.content
				}))
			}))
			defer srv.Close()

			c := chatClient(t, srv.URL, "")
			got, err := c.generate(context.Background(), chatRequest{
				Model:          "m",
				Messages:       []chatMessage{{Role: "user", Content: "hi"}},
				ResponseFormat: &chatResponseFormat{Type: "json_object"},
			})
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("generate() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("generate() error = %q, want containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if got.Content != tt.content {
				t.Errorf("content = %q, want original %q preserved", got.Content, tt.content)
			}
		})
	}
}

func TestGeneratePlainTextWithoutFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, testChatResponse(t, func(r map[string]any) {
			r["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] = "not json at all"
		}))
	}))
	defer srv.Close()

	c := chatClient(t, srv.URL, "")
	got, err := c.generate(context.Background(), chatRequest{
		Model:    "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got.Content != "not json at all" {
		t.Errorf("content = %q, want %q", got.Content, "not json at all")
	}
}

func ptr[T any](v T) *T { return &v }
