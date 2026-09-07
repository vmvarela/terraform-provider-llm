// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

var (
	_ resource.ResourceWithModifyPlan = &generationResource{}
)

var usageAttrTypes = map[string]attr.Type{
	"prompt_tokens":     types.Int64Type,
	"completion_tokens": types.Int64Type,
	"total_tokens":      types.Int64Type,
}

// generationResource is the logical llm_generation resource. It generates
// content during apply and stores a snapshot in state; there is no import, no
// remote lookup, and no remote deletion.
type generationResource struct {
	p *llmProvider
}

type generationResourceModel struct {
	Model               types.String  `tfsdk:"model"`
	Messages            types.List    `tfsdk:"messages"`
	ResponseFormat      types.String  `tfsdk:"response_format"`
	Temperature         types.Float64 `tfsdk:"temperature"`
	TopP                types.Float64 `tfsdk:"top_p"`
	MaxTokens           types.Int64   `tfsdk:"max_tokens"`
	MaxCompletionTokens types.Int64   `tfsdk:"max_completion_tokens"`
	GenerationKey       types.String  `tfsdk:"generation_key"`

	ID           types.String `tfsdk:"id"`
	Content      types.String `tfsdk:"content"`
	ResponseID   types.String `tfsdk:"response_id"`
	ModelUsed    types.String `tfsdk:"model_used"`
	FinishReason types.String `tfsdk:"finish_reason"`
	Usage        types.Object `tfsdk:"usage"`
}

type messageModel struct {
	Role    types.String `tfsdk:"role"`
	Content types.String `tfsdk:"content"`
}

type usageModel struct {
	PromptTokens     types.Int64 `tfsdk:"prompt_tokens"`
	CompletionTokens types.Int64 `tfsdk:"completion_tokens"`
	TotalTokens      types.Int64 `tfsdk:"total_tokens"`
}

func (r *generationResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_generation"
}

func (r *generationResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Generates content through an OpenAI-compatible Chat Completions " +
			"API during apply and stores a logical snapshot of the response in Terraform state.\n\n" +
			"This is a logical resource: there is **no import**, no remote lookup, and no remote " +
			"deletion. `terraform destroy` only removes the snapshot from state; refresh never " +
			"contacts the API.\n\n" +
			"Prompts and generated content are persisted in the Terraform state. Sensitive " +
			"attributes are redacted from CLI output but are **not encrypted**. Regeneration " +
			"happens during apply whenever `model`, `messages`, generation parameters, or " +
			"`generation_key` change; provider configuration changes alone do not trigger " +
			"regeneration — change `generation_key` explicitly to regenerate after a backend " +
			"change.",
		Attributes: map[string]schema.Attribute{
			"model": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Model identifier passed to the API (for example " +
					"`gpt-4o-mini`). Must be a non-empty string. This configured value is separate " +
					"from `model_used`, which reports the model the API actually used.",
			},
			"messages": schema.ListNestedAttribute{
				Required:  true,
				Sensitive: true,
				MarkdownDescription: "Conversation messages in order. Content is plain text and is " +
					"persisted in state; the list is marked sensitive (redacted from display, not " +
					"encrypted). Whitespace-only content is sent and stored as-is, without rewriting.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"role": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "Message role: `system`, `developer`, `user`, or " +
								"`assistant`.",
						},
						"content": schema.StringAttribute{
							Required:  true,
							Sensitive: true,
							MarkdownDescription: "Plain-text message content, sent and stored " +
								"verbatim. Sensitive.",
						},
					},
				},
			},
			"response_format": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Output format requested from the API. Only sent to the API " +
					"when explicitly configured; when omitted the API default (plain text) applies. " +
					"Supported modes: `text` (plain text) and `json_object` (a JSON object, " +
					"validated locally before the snapshot is saved).",
			},
			"temperature": schema.Float64Attribute{
				Optional: true,
				MarkdownDescription: "Sampling temperature between `0` and `2`. Omitted from the " +
					"API request when unset.",
			},
			"top_p": schema.Float64Attribute{
				Optional: true,
				MarkdownDescription: "Nucleus sampling value between `0` and `1`. Omitted from the " +
					"API request when unset.",
			},
			"max_tokens": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Maximum number of tokens to generate. Must be positive and " +
					"mutually exclusive with `max_completion_tokens`.",
			},
			"max_completion_tokens": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Maximum number of completion tokens (newer API field). Must " +
					"be positive and mutually exclusive with `max_tokens`.",
			},
			"generation_key": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Optional marker that forces regeneration when its value " +
					"changes. Never sent to the API. Use it to explicitly regenerate after a " +
					"backend (base URL, credentials, model deployment) change; credential rotation " +
					"alone does not regenerate content.",
			},
			"id": schema.StringAttribute{
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
				MarkdownDescription: "Stable local resource ID generated by the provider, " +
					"independent of any API response ID. Not importable.",
			},
			"content": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Generated text content from the first assistant message. " +
					"Sensitive; persisted in state.",
			},
			"response_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Response ID reported by the API, kept as optional metadata. " +
					"Null when the API omits it. The local `id` remains the resource identifier.",
			},
			"model_used": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Model reported by the API in the response. Never overwrites " +
					"the configured `model`. Null when the API omits it.",
			},
			"finish_reason": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Finish reason reported by the API (for example `stop`). " +
					"Null when the API omits it.",
			},
			"usage": schema.SingleNestedAttribute{
				Computed: true,
				MarkdownDescription: "Token usage reported by the API. Null when the API omits it; " +
					"individual counters are null when unreported.",
				Attributes: map[string]schema.Attribute{
					"prompt_tokens": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Prompt tokens reported by the API.",
					},
					"completion_tokens": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Completion tokens reported by the API.",
					},
					"total_tokens": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Total tokens reported by the API.",
					},
				},
			},
		},
	}
}

// ValidateConfig is unknown-safe: planned unknowns never produce errors, and
// invalid known inputs are rejected with attribute diagnostics that never echo
// sensitive input. The same validation runs before every Create/Update.
func (r *generationResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var plan generationResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validateGeneration(ctx, plan, false)...)
}

func (r *generationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan generationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	c, diags := r.apiClient()
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	result, diags := r.generate(ctx, c, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Snapshot is written once, only on success.
	setSnapshot(ctx, &plan, result, true, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *generationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Logical resource: preserve state exactly as-is, no generation, no remote
	// lookup, no dependency on the configured client.
}

func (r *generationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state generationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Keep the previous local ID and all old state until generation succeeds.
	if plan.ID.IsUnknown() {
		plan.ID = state.ID
	}

	c, diags := r.apiClient()
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	result, diags := r.generate(ctx, c, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	setSnapshot(ctx, &plan, result, false, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *generationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Logical resource: remove the snapshot from state, no remote request.
	resp.State.RemoveResource(ctx)
}

// ModifyPlan marks generated outputs unknown in the plan whenever regeneration
// is required (any generation input changed), and only for that case. The
// local ID keeps UseStateForUnknown.
func (r *generationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, state generationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !generationInputsChanged(plan, state) {
		return
	}
	plan.Content = types.StringUnknown()
	plan.ResponseID = types.StringUnknown()
	plan.ModelUsed = types.StringUnknown()
	plan.FinishReason = types.StringUnknown()
	plan.Usage = types.ObjectUnknown(usageAttrTypes)
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func generationInputsChanged(plan, state generationResourceModel) bool {
	return !plan.Model.Equal(state.Model) ||
		!plan.Messages.Equal(state.Messages) ||
		!plan.ResponseFormat.Equal(state.ResponseFormat) ||
		!plan.Temperature.Equal(state.Temperature) ||
		!plan.TopP.Equal(state.TopP) ||
		!plan.MaxTokens.Equal(state.MaxTokens) ||
		!plan.MaxCompletionTokens.Equal(state.MaxCompletionTokens) ||
		!plan.GenerationKey.Equal(state.GenerationKey)
}

// apiClient builds the client from provider configuration. Never called from
// Read/Delete, so refresh and destroy work without a configured endpoint.
func (r *generationResource) apiClient() (*client, diag.Diagnostics) {
	var diags diag.Diagnostics
	if r.p == nil {
		diags.AddError("Provider not configured",
			"The llm provider was not configured; this is a bug in the provider.")
		return nil, diags
	}
	return r.p.configuredClient(context.Background())
}

// generate validates the plan (apply mode: unresolved inputs are rejected),
// builds the API request, and calls the client. On any error no snapshot is
// produced. Errors from the client are already sanitized; the diagnostic adds
// only that a failed request may still have incurred charges.
func (r *generationResource) generate(ctx context.Context, c *client, plan generationResourceModel) (chatResult, diag.Diagnostics) {
	var diags diag.Diagnostics
	diags.Append(validateGeneration(ctx, plan, true)...)
	if diags.HasError() {
		return chatResult{}, diags
	}

	req := chatRequest{Model: plan.Model.ValueString()}
	if !plan.Messages.IsNull() && !plan.Messages.IsUnknown() {
		for _, el := range plan.Messages.Elements() {
			obj, ok := el.(types.Object)
			if !ok {
				continue
			}
			var m messageModel
			if d := obj.As(ctx, &m, basetypes.ObjectAsOptions{}); d.HasError() {
				diags.Append(d...)
				return chatResult{}, diags
			}
			req.Messages = append(req.Messages, chatMessage{
				Role:    m.Role.ValueString(),
				Content: m.Content.ValueString(),
			})
		}
	}
	if !plan.ResponseFormat.IsNull() && !plan.ResponseFormat.IsUnknown() {
		req.ResponseFormat = &chatResponseFormat{Type: plan.ResponseFormat.ValueString()}
	}
	if !plan.Temperature.IsNull() && !plan.Temperature.IsUnknown() {
		v := plan.Temperature.ValueFloat64()
		req.Temperature = &v
	}
	if !plan.TopP.IsNull() && !plan.TopP.IsUnknown() {
		v := plan.TopP.ValueFloat64()
		req.TopP = &v
	}
	if !plan.MaxTokens.IsNull() && !plan.MaxTokens.IsUnknown() {
		v := plan.MaxTokens.ValueInt64()
		req.MaxTokens = &v
	}
	if !plan.MaxCompletionTokens.IsNull() && !plan.MaxCompletionTokens.IsUnknown() {
		v := plan.MaxCompletionTokens.ValueInt64()
		req.MaxCompletionTokens = &v
	}
	// generation_key is intentionally not sent to the API.

	result, err := c.generate(ctx, req)
	if err != nil {
		// Client errors already carry the charge notice for request-side
		// failures; response-side validation failures are deterministic.
		diags.AddError("Generation failed", err.Error())
		return chatResult{}, diags
	}
	return result, diags
}

// setSnapshot fills computed attributes from a successful response. The
// response overwrites all computed metadata, including nulls when the API
// omits values. newID generates a fresh local resource ID.
func setSnapshot(ctx context.Context, plan *generationResourceModel, result chatResult, newID bool, diags *diag.Diagnostics) {
	if newID {
		plan.ID = types.StringValue(newResourceID())
	}
	plan.Content = stringOrNull(result.Content)
	plan.ResponseID = stringOrNull(result.ResponseID)
	plan.ModelUsed = stringOrNull(result.ModelUsed)
	plan.FinishReason = stringOrNull(result.FinishReason)
	if result.Usage == nil {
		plan.Usage = types.ObjectNull(usageAttrTypes)
		return
	}
	u := usageModel{
		PromptTokens:     int64OrNull(result.Usage.PromptTokens),
		CompletionTokens: int64OrNull(result.Usage.CompletionTokens),
		TotalTokens:      int64OrNull(result.Usage.TotalTokens),
	}
	obj, d := types.ObjectValueFrom(ctx, usageAttrTypes, u)
	diags.Append(d...)
	if d.HasError() {
		return
	}
	plan.Usage = obj
}

func stringOrNull(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

func int64OrNull(v *int64) types.Int64 {
	if v == nil {
		return types.Int64Null()
	}
	return types.Int64Value(*v)
}

// newResourceID returns a stable local ID from crypto/rand, independent of any
// API response ID. rand.Text never returns an error.
func newResourceID() string {
	return rand.Text()
}

// validateGeneration validates all generation inputs. In plan mode (forApply
// false) unknowns are tolerated; in apply mode (forApply true) unresolved
// inputs are rejected. Diagnostics never echo sensitive input (message
// content, prompts, credentials).
func validateGeneration(ctx context.Context, plan generationResourceModel, forApply bool) diag.Diagnostics {
	var diags diag.Diagnostics
	diags.Append(validateNonEmptyString("model", plan.Model, forApply)...)
	diags.Append(validateResponseFormat(plan.ResponseFormat, forApply)...)
	diags.Append(validateFloat64Range("temperature", plan.Temperature, 0, 2, forApply)...)
	diags.Append(validateFloat64Range("top_p", plan.TopP, 0, 1, forApply)...)
	diags.Append(validatePositiveInt64("max_tokens", plan.MaxTokens, forApply)...)
	diags.Append(validatePositiveInt64("max_completion_tokens", plan.MaxCompletionTokens, forApply)...)
	diags.Append(validateExclusiveInt64("max_tokens", plan.MaxTokens, "max_completion_tokens", plan.MaxCompletionTokens, forApply)...)
	diags.Append(validateMessages(ctx, plan.Messages, forApply)...)
	return diags
}

func unresolvedMsg(name string) string {
	return name + " is unknown at apply time; generation requires all inputs to be resolved."
}

func validateNonEmptyString(name string, v types.String, forApply bool) diag.Diagnostics {
	var diags diag.Diagnostics
	if v.IsNull() {
		return diags
	}
	if v.IsUnknown() {
		if forApply {
			diags.AddAttributeError(path.Root(name), "Unresolved input", unresolvedMsg(name))
		}
		return diags
	}
	// Whitespace-only values are invalid but never rewritten.
	if strings.TrimSpace(v.ValueString()) == "" {
		diags.AddAttributeError(path.Root(name), "Invalid "+name,
			name+" must be a non-empty string.")
	}
	return diags
}

func validateFloat64Range(name string, v types.Float64, min, max float64, forApply bool) diag.Diagnostics {
	var diags diag.Diagnostics
	if v.IsNull() {
		return diags
	}
	if v.IsUnknown() {
		if forApply {
			diags.AddAttributeError(path.Root(name), "Unresolved input", unresolvedMsg(name))
		}
		return diags
	}
	f := v.ValueFloat64()
	if f < min || f > max {
		diags.AddAttributeError(path.Root(name), "Invalid "+name,
			fmt.Sprintf("%s must be between %g and %g.", name, min, max))
	}
	return diags
}

func validatePositiveInt64(name string, v types.Int64, forApply bool) diag.Diagnostics {
	var diags diag.Diagnostics
	if v.IsNull() {
		return diags
	}
	if v.IsUnknown() {
		if forApply {
			diags.AddAttributeError(path.Root(name), "Unresolved input", unresolvedMsg(name))
		}
		return diags
	}
	if v.ValueInt64() <= 0 {
		diags.AddAttributeError(path.Root(name), "Invalid "+name,
			name+" must be a positive integer.")
	}
	return diags
}

func validateExclusiveInt64(nameA string, a types.Int64, nameB string, b types.Int64, forApply bool) diag.Diagnostics {
	var diags diag.Diagnostics
	if forApply && (a.IsUnknown() || b.IsUnknown()) {
		// Unknown single values are reported by the positive validators; only
		// flag exclusivity when both are known.
		return diags
	}
	if a.IsNull() || b.IsNull() || a.IsUnknown() || b.IsUnknown() {
		return diags
	}
	diags.AddAttributeError(path.Root(nameA), "Conflicting attributes",
		nameA+" and "+nameB+" are mutually exclusive; set only one.")
	return diags
}

func validateResponseFormat(v types.String, forApply bool) diag.Diagnostics {
	var diags diag.Diagnostics
	if v.IsNull() {
		return diags
	}
	if v.IsUnknown() {
		if forApply {
			diags.AddAttributeError(path.Root("response_format"), "Unresolved input",
				unresolvedMsg("response_format"))
		}
		return diags
	}
	switch v.ValueString() {
	case "text", "json_object":
	default:
		diags.AddAttributeError(path.Root("response_format"), "Invalid response_format",
			`response_format must be either "text" or "json_object".`)
	}
	return diags
}

// validateMessages checks nested messages. Unknown elements or fields are
// tolerated during planning and rejected at apply time. Message content is
// never included in diagnostics.
func validateMessages(ctx context.Context, list types.List, forApply bool) diag.Diagnostics {
	var diags diag.Diagnostics
	if list.IsNull() || list.IsUnknown() {
		return diags
	}
	for i, el := range list.Elements() {
		obj, ok := el.(types.Object)
		if !ok {
			continue
		}
		elemPath := path.Root("messages").AtListIndex(i)
		if obj.IsUnknown() {
			if forApply {
				diags.AddAttributeError(elemPath, "Unresolved message",
					fmt.Sprintf("messages[%d] is unknown at apply time and cannot be generated.", i))
			}
			continue
		}
		var m messageModel
		if d := obj.As(ctx, &m, basetypes.ObjectAsOptions{}); d.HasError() {
			diags.Append(d...)
			continue
		}
		if m.Role.IsNull() {
			diags.AddAttributeError(elemPath.AtName("role"), "Missing role",
				fmt.Sprintf("messages[%d].role is required.", i))
		}
		if m.Content.IsNull() {
			diags.AddAttributeError(elemPath.AtName("content"), "Missing content",
				fmt.Sprintf("messages[%d].content is required.", i))
		}
		roleUnknown := m.Role.IsUnknown()
		contentUnknown := m.Content.IsUnknown()
		if roleUnknown || contentUnknown {
			if forApply {
				diags.AddAttributeError(elemPath, "Unresolved message",
					fmt.Sprintf("messages[%d] contains unknown values at apply time and cannot be generated.", i))
			}
			continue
		}
		if !m.Role.IsNull() {
			switch m.Role.ValueString() {
			case "system", "developer", "user", "assistant":
			default:
				diags.AddAttributeError(elemPath.AtName("role"), "Unsupported role",
					fmt.Sprintf("messages[%d].role must be one of: system, developer, user, assistant.", i))
			}
		}
	}
	return diags
}
