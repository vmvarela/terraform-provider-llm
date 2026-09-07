// Acceptance tests for the llm_generation resource.
//
// All requests hit in-process httptest backends; no live endpoints. Request
// counters are atomic so the handler and test steps agree on request counts.
package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const sentinelAPIKey = "secret-sentinel-key-DO-NOT-LEAK"

// genServer is a fake OpenAI-compatible chat completions backend.
type genServer struct {
	srv   *httptest.Server
	calls atomic.Int64

	// mode controls how the next request is answered: 0 = success,
	// 1 = HTTP 500, 2 = malformed JSON body, 3 = valid HTTP/JSON envelope
	// whose message content is not valid JSON (exercises json_object
	// response validation). Test steps set it in PreConfig.
	mode atomic.Int32

	mu      sync.Mutex
	content string // content returned in successful responses
	bodies  []map[string]json.RawMessage
	auths   []string
}

func newGenServer(t *testing.T) *genServer {
	t.Helper()
	s := &genServer{content: "snapshot-1"}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *genServer) setContent(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content = content
}

// captureID stores the resource's stable local ID so later steps can assert
// it never changes across regenerations.
func captureID(dst *string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		rs, ok := state.RootModule().Resources["llm_generation.test"]
		if !ok {
			return fmt.Errorf("resource llm_generation.test not found in state")
		}
		if rs.Primary.ID == "" {
			return fmt.Errorf("llm_generation.test has an empty ID")
		}
		*dst = rs.Primary.ID
		return nil
	}
}

func (s *genServer) handle(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(body, &raw)
	s.mu.Lock()
	s.bodies = append(s.bodies, raw)
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.mu.Unlock()

	switch s.mode.Load() {
	case 1:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":{"message":"backend exploded"}}`)
	case 2:
		_, _ = fmt.Fprint(w, `not-json-at-all`)
	case 3:
		// Valid envelope whose content is not JSON: rejected by
		// json_object validation without another request.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"remote-1","model":"returned-model","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
			strconv.Quote("plain text, not json"))
	default:
		content := s.contentLocked()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"remote-1","model":"returned-model","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
			strconv.Quote(content))
	}
}

func (s *genServer) contentLocked() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.content
}

func (s *genServer) lastBody() (map[string]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return nil, fmt.Errorf("no requests recorded")
	}
	last := s.bodies[len(s.bodies)-1]
	out := make(map[string]json.RawMessage, len(last))
	for k, v := range last {
		out[k] = v
	}
	return out, nil
}

func (s *genServer) lastAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auths) == 0 {
		return ""
	}
	return s.auths[len(s.auths)-1]
}

func protoV6Factory() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"llm": providerserver.NewProtocol6WithError(New("test")()),
	}
}

func llmProviderConfig(baseURL, apiKey string) string {
	if apiKey == "" {
		return fmt.Sprintf(`provider "llm" {
  base_url = %q
}`, baseURL)
	}
	return fmt.Sprintf(`provider "llm" {
  base_url = %q
  api_key  = %q
}`, baseURL, apiKey)
}

func genResourceConfig(model, message string, extraLines ...string) string {
	extra := ""
	if len(extraLines) > 0 {
		extra = "\n  " + strings.Join(extraLines, "\n  ")
	}
	return fmt.Sprintf(`resource "llm_generation" "test" {
  model    = %q
  messages = [{ role = "user", content = %q }]%s
}`, model, message, extra)
}

func fullConfig(baseURL, apiKey, model, message string, extraLines ...string) string {
	return llmProviderConfig(baseURL, apiKey) + "\n\n" + genResourceConfig(model, message, extraLines...)
}

func checkCalls(s *genServer, want int64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := s.calls.Load(); got != want {
			return fmt.Errorf("expected %d generation requests, got %d", want, got)
		}
		return nil
	}
}

func checkRequestModel(s *genServer, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		body, err := s.lastBody()
		if err != nil {
			return err
		}
		var got string
		if err := json.Unmarshal(body["model"], &got); err != nil {
			return fmt.Errorf("request model: %w", err)
		}
		if got != want {
			return fmt.Errorf("request model = %q, want %q", got, want)
		}
		return nil
	}
}

func checkRequestMessageContent(s *genServer, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		body, err := s.lastBody()
		if err != nil {
			return err
		}
		var msgs []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body["messages"], &msgs); err != nil {
			return fmt.Errorf("request messages: %w", err)
		}
		if len(msgs) != 1 || msgs[0].Content != want || msgs[0].Role != "user" {
			return fmt.Errorf("request messages = %+v, want one user message %q", msgs, want)
		}
		return nil
	}
}

func checkRequestMissing(s *genServer, keys ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		body, err := s.lastBody()
		if err != nil {
			return err
		}
		for _, k := range keys {
			if _, ok := body[k]; ok {
				return fmt.Errorf("request body unexpectedly contains %q", k)
			}
		}
		return nil
	}
}

func checkRequestResponseFormat(s *genServer, wantType string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		body, err := s.lastBody()
		if err != nil {
			return err
		}
		var rf struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body["response_format"], &rf); err != nil {
			return fmt.Errorf("request response_format: %w", err)
		}
		if rf.Type != wantType {
			return fmt.Errorf("request response_format.type = %q, want %q", rf.Type, wantType)
		}
		return nil
	}
}

func checkRequestTemperature(s *genServer, want float64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		body, err := s.lastBody()
		if err != nil {
			return err
		}
		var got float64
		if err := json.Unmarshal(body["temperature"], &got); err != nil {
			return fmt.Errorf("request temperature: %w", err)
		}
		if got != want {
			return fmt.Errorf("request temperature = %v, want %v", got, want)
		}
		return nil
	}
}

func checkLastAuth(s *genServer, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := s.lastAuth(); got != want {
			return fmt.Errorf("last Authorization header = %q, want %q", got, want)
		}
		return nil
	}
}

// checkNoCredsInResourceState walks every flattened attribute of the resource
// instance and fails if the credential sentinel appears anywhere. It does not
// assert that prompts or outputs are absent from state.
func checkNoCredsInResourceState(sentinel string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		rs, ok := state.RootModule().Resources["llm_generation.test"]
		if !ok {
			return fmt.Errorf("resource llm_generation.test not found in state")
		}
		for k, v := range rs.Primary.Attributes {
			if strings.Contains(v, sentinel) {
				return fmt.Errorf("credential leaked into resource state attribute %q", k)
			}
		}
		return nil
	}
}

// clearEnv removes ambient LLM_* env vars so tests only see explicit config.
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_API_KEY", "")
}

// TestAccGeneration_CreateReadDestroy covers first generation, a subsequent
// empty plan (refresh), and refresh/destroy making zero requests.
func TestAccGeneration_CreateReadDestroy(t *testing.T) {
	clearEnv(t)
	srv := newGenServer(t)
	var resID string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factory(),
		// Destroy (and any refresh before it) must not hit the backend.
		CheckDestroy: checkCalls(srv, 1),
		Steps: []resource.TestStep{
			{
				Config: fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "model", "configured-model"),
					resource.TestCheckResourceAttr("llm_generation.test", "content", "snapshot-1"),
					resource.TestCheckResourceAttr("llm_generation.test", "response_id", "remote-1"),
					resource.TestCheckResourceAttr("llm_generation.test", "model_used", "returned-model"),
					resource.TestCheckResourceAttr("llm_generation.test", "finish_reason", "stop"),
					resource.TestCheckResourceAttr("llm_generation.test", "usage.prompt_tokens", "1"),
					resource.TestCheckResourceAttr("llm_generation.test", "usage.completion_tokens", "2"),
					resource.TestCheckResourceAttr("llm_generation.test", "usage.total_tokens", "3"),
					captureID(&resID),
					checkCalls(srv, 1),
					checkRequestModel(srv, "configured-model"),
					checkRequestMessageContent(srv, "hello"),
					checkRequestMissing(srv, "generation_key", "response_format"),
					checkLastAuth(srv, "Bearer "+sentinelAPIKey),
					checkNoCredsInResourceState(sentinelAPIKey),
				),
			},
			{
				// Same config: refresh finds no drift, plan is empty, no requests.
				Config:           fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCalls(srv, 1),
					resource.TestCheckResourceAttrPtr("llm_generation.test", "id", &resID),
				),
			},
		},
	})
}

// TestAccGeneration_UpdateRegenerates covers message, model, parameter, and
// generation_key updates: each regenerates, preserves the local ID, and the
// API-returned model never overwrites the configured model.
func TestAccGeneration_UpdateRegenerates(t *testing.T) {
	clearEnv(t)
	srv := newGenServer(t)
	var resID string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factory(),
		Steps: []resource.TestStep{
			{
				Config: fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureID(&resID),
					checkCalls(srv, 1),
				),
			},
			{
				Config: fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello v2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "content", "snapshot-1"),
					resource.TestCheckResourceAttrPtr("llm_generation.test", "id", &resID),
					checkCalls(srv, 2),
					checkRequestMessageContent(srv, "hello v2"),
				),
			},
			{
				// API returns "returned-model"; the configured model changes and stays.
				Config: fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model-2", "hello v2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "model", "configured-model-2"),
					resource.TestCheckResourceAttr("llm_generation.test", "model_used", "returned-model"),
					resource.TestCheckResourceAttrPtr("llm_generation.test", "id", &resID),
					checkCalls(srv, 3),
					checkRequestModel(srv, "configured-model-2"),
				),
			},
			{
				Config: fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model-2", "hello v2", "temperature = 0.5"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr("llm_generation.test", "id", &resID),
					checkCalls(srv, 4),
					checkRequestTemperature(srv, 0.5),
				),
			},
			{
				// generation_key forces regeneration but is never sent on the wire.
				Config: fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model-2", "hello v2",
					"temperature = 0.5", `generation_key = "gen-key-2"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr("llm_generation.test", "id", &resID),
					checkCalls(srv, 5),
					checkRequestMissing(srv, "generation_key"),
				),
			},
			{
				// max_tokens and max_completion_tokens are mutually exclusive.
				Config:      fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model-2", "hello v2", "max_tokens = 10", "max_completion_tokens = 20"),
				ExpectError: regexp.MustCompile("mutually exclusive"),
			},
			{
				// A valid final config restores state so post-test destroy
				// succeeds; nothing regenerates because nothing changed.
				Config:           fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model-2", "hello v2", "temperature = 0.5", `generation_key = "gen-key-2"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check:            checkCalls(srv, 5),
			},
		},
	})
}

// TestAccGeneration_JSONOutput covers json_object output on a valid response
// and an invalid-JSON update that fails while preserving old good state.
func TestAccGeneration_JSONOutput(t *testing.T) {
	clearEnv(t)
	srv := newGenServer(t)
	srv.setContent(`{"answer":"ok"}`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factory(),
		Steps: []resource.TestStep{
			{
				Config: fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "give me json",
					`response_format = "json_object"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "content", `{"answer":"ok"}`),
					checkCalls(srv, 1),
					checkRequestResponseFormat(srv, "json_object"),
				),
			},
			{
				// The failing request returns content that is not valid JSON.
				PreConfig:   func() { srv.mode.Store(3) },
				Config:      fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "give me json v2", `response_format = "json_object"`),
				ExpectError: regexp.MustCompile("not valid JSON as required by the json_object output format"),
			}, {
				// State still holds the last good snapshot; no retry happened.
				Config:           fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "give me json", `response_format = "json_object"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "content", `{"answer":"ok"}`),
					checkCalls(srv, 2), // failed update charged one request, nothing since
				),
			},
		},
	})
}

// TestAccGeneration_FailedUpdatePreservesState covers a failed Update (HTTP
// 500, then a malformed body): the previous ID, content, and config stay in
// state, no requests follow, and a retry with the new config succeeds while
// keeping the same local ID. Steps switch the backend mode in PreConfig.
func TestAccGeneration_FailedUpdatePreservesState(t *testing.T) {
	clearEnv(t)
	srv := newGenServer(t)
	var resID string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factory(),
		Steps: []resource.TestStep{
			{
				Config: fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureID(&resID),
					checkCalls(srv, 1),
				),
			},
			{
				// HTTP 500 during regenerate.
				PreConfig:   func() { srv.mode.Store(1) },
				Config:      fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello v2"),
				ExpectError: regexp.MustCompile("HTTP status 500"),
			},
			{
				// Old snapshot untouched; refresh/plan made no requests.
				Config:           fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "content", "snapshot-1"),
					resource.TestCheckResourceAttrPtr("llm_generation.test", "id", &resID),
					checkCalls(srv, 2),
				),
			},
			{
				Config:      fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello v2"),
				PreConfig:   func() { srv.mode.Store(2) },
				ExpectError: regexp.MustCompile("malformed JSON"),
			},
			{
				Config:           fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "content", "snapshot-1"),
					checkCalls(srv, 3),
				),
			},
			{
				// Backend healthy again; retry succeeds with the same local ID.
				PreConfig: func() { srv.mode.Store(0) },
				Config:    fullConfig(srv.srv.URL, sentinelAPIKey, "configured-model", "hello v2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "content", "snapshot-1"),
					resource.TestCheckResourceAttrPtr("llm_generation.test", "id", &resID),
					checkCalls(srv, 4),
				),
			},
		},
	})
}

// TestAccGeneration_ProviderChangesDoNotRegenerate covers credential rotation
// and base_url changes producing no diff and no requests, then an explicit
// generation_key change regenerating against the new endpoint.
func TestAccGeneration_ProviderChangesDoNotRegenerate(t *testing.T) {
	clearEnv(t)
	srvA := newGenServer(t)
	srvB := newGenServer(t)
	srvB.setContent("snapshot-from-b")
	var resID string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factory(),
		Steps: []resource.TestStep{
			{
				Config: fullConfig(srvA.srv.URL, sentinelAPIKey, "configured-model", "hello"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureID(&resID),
					checkCalls(srvA, 1),
					checkCalls(srvB, 0),
					checkNoCredsInResourceState(sentinelAPIKey),
					checkNoCredsInResourceState("rotated-secret-key"),
				),
			},
			{
				// Credential rotation alone must not regenerate content.
				Config:           fullConfig(srvA.srv.URL, "rotated-secret-key", "configured-model", "hello"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCalls(srvA, 1),
					checkCalls(srvB, 0),
				),
			},
			{
				// base_url change with identical resource inputs: no diff, no requests.
				Config:           fullConfig(srvB.srv.URL, "rotated-secret-key", "configured-model", "hello"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCalls(srvA, 1),
					checkCalls(srvB, 0),
				),
			},
			{
				// Explicit generation_key regenerates against endpoint B.
				Config: fullConfig(srvB.srv.URL, "rotated-secret-key", "configured-model", "hello", `generation_key = "backend-v2"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "content", "snapshot-from-b"),
					resource.TestCheckResourceAttrPtr("llm_generation.test", "id", &resID),
					checkCalls(srvA, 1),
					checkCalls(srvB, 1),
				),
			},
		},
	})
}

// TestAccGeneration_UnknownInputsNoRequestAtPlan uses the built-in
// terraform_data resource (no registry download) to feed unknown values into
// messages and model. Outputs must be unknown at plan and no request may fire
// until apply.
func TestAccGeneration_UnknownInputsNoRequestAtPlan(t *testing.T) {
	clearEnv(t)
	srv := newGenServer(t)

	genWithUnknownMessages := llmProviderConfig(srv.srv.URL, sentinelAPIKey) + `

resource "terraform_data" "seed" {
  input = "seed"
}

resource "llm_generation" "test" {
  model    = "configured-model"
  messages = [{ role = "user", content = terraform_data.seed.id }]
}
`

	genWithUnknownModel := llmProviderConfig(srv.srv.URL, sentinelAPIKey) + `

resource "terraform_data" "seed" {
  input = "seed"
}

resource "llm_generation" "test" {
  model    = terraform_data.seed.id
  messages = [{ role = "user", content = "hello" }]
}
`

	unknownOutputs := resource.ConfigPlanChecks{
		// PlanOnly steps run no PreApply plan; the saved non-refresh plan is
		// checked in the PostApplyPreRefresh slot instead.
		PostApplyPreRefresh: []plancheck.PlanCheck{
			plancheck.ExpectUnknownValue("llm_generation.test", tfjsonpath.New("content")),
			plancheck.ExpectUnknownValue("llm_generation.test", tfjsonpath.New("response_id")),
			plancheck.ExpectUnknownValue("llm_generation.test", tfjsonpath.New("model_used")),
		},
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factory(),
		Steps: []resource.TestStep{
			{
				Config:             genWithUnknownMessages,
				PlanOnly:           true,
				ConfigPlanChecks:   unknownOutputs,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:             genWithUnknownModel,
				PlanOnly:           true,
				ConfigPlanChecks:   unknownOutputs,
				ExpectNonEmptyPlan: true,
			},
			{
				// Apply with the unknown input resolved: the two PlanOnly
				// plans above made zero requests, so exactly one request.
				Config: genWithUnknownMessages,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "content", "snapshot-1"),
					checkCalls(srv, 1),
				),
			},
		},
	})
}

// TestAccGeneration_NoCredentials covers a local-style endpoint with no
// api_key configured: generation succeeds and no Authorization header is sent.
func TestAccGeneration_NoCredentials(t *testing.T) {
	clearEnv(t)
	srv := newGenServer(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factory(),
		Steps: []resource.TestStep{
			{
				Config: fullConfig(srv.srv.URL, "", "configured-model", "hello"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("llm_generation.test", "content", "snapshot-1"),
					checkCalls(srv, 1),
					checkLastAuth(srv, ""),
				),
			},
		},
	})
}
