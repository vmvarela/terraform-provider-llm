// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const defaultTimeoutSeconds = int64(300)

// llmProvider implements the llm Terraform provider against an
// OpenAI-compatible Chat Completions HTTP API.
type llmProvider struct {
	version string
	conf    providerConfigModel
}

// providerConfigModel is the provider configuration block. Values are stored
// as-is (including unknowns); environment fallbacks and validation happen
// lazily when a client is actually needed for generation.
type providerConfigModel struct {
	BaseURL        types.String `tfsdk:"base_url"`
	APIKey         types.String `tfsdk:"api_key"`
	TimeoutSeconds types.Int64  `tfsdk:"timeout_seconds"`
}

// New returns the provider factory expected by providerserver.Serve.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &llmProvider{version: version}
	}
}

func (p *llmProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "llm"
	resp.Version = p.version
}

func (p *llmProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Terraform provider for generating content through any " +
			"OpenAI-compatible Chat Completions HTTP endpoint. Credentials live only in " +
			"provider configuration and are never stored in resource state.",
		Attributes: map[string]schema.Attribute{
			"base_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Root URL of an OpenAI-compatible Chat Completions API " +
					"(the provider appends `/chat/completions`). Falls back to the `LLM_BASE_URL` " +
					"environment variable when unset. There is no vendor default; a base URL must " +
					"be configured here or via the environment before any `llm_generation` resource " +
					"can be created or updated. Refreshing or destroying existing resources never " +
					"contacts the API and works without it.",
			},
			"api_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Bearer token sent in the `Authorization` header. Falls back " +
					"to the `LLM_API_KEY` environment variable when unset. Setting an explicit " +
					"empty string disables the environment fallback and sends no credentials, for " +
					"local or unauthenticated endpoints. Marked sensitive and never stored in " +
					"resource state.",
			},
			"timeout_seconds": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Per-request HTTP timeout in seconds for generation calls. " +
					"Defaults to `300`. Must be between `1` and `86400`.",
			},
		},
	}
}

func (p *llmProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	// Store configuration as-is. Unknown values are tolerated here; they are
	// rejected at generation time. No environment fallback and no network I/O.
	resp.Diagnostics.Append(req.Config.Get(ctx, &p.conf)...)
}

func (p *llmProvider) ValidateConfig(ctx context.Context, req provider.ValidateConfigRequest, resp *provider.ValidateConfigResponse) {
	var conf providerConfigModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &conf)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Unknown-safe: only known values are range-checked.
	resp.Diagnostics.Append(validateInt64Range("timeout_seconds", conf.TimeoutSeconds, 1, 86400)...)
}

func (p *llmProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		func() resource.Resource { return &generationResource{p: p} },
	}
}

func (p *llmProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return nil
}

// configuredClient resolves provider configuration and environment variables
// into a client for generation. It is only called from resource Create/Update:
// unknown configured values are rejected instead of silently falling back to
// the environment, and missing base URL is an actionable, sanitized error.
func (p *llmProvider) configuredClient(ctx context.Context) (*client, diag.Diagnostics) {
	var diags diag.Diagnostics

	baseURL := p.conf.BaseURL
	if baseURL.IsUnknown() {
		diags.AddError("Provider base_url is unknown at apply time",
			"Generation requires a known base_url. Ensure the value is resolvable during apply.")
		return nil, diags
	}
	url := ""
	if baseURL.IsNull() {
		url = os.Getenv("LLM_BASE_URL")
		if url == "" {
			diags.AddError("Missing API base URL",
				"Set base_url in the provider configuration or the LLM_BASE_URL environment "+
					"variable before creating or updating llm_generation resources.")
			return nil, diags
		}
	} else {
		url = baseURL.ValueString()
	}

	apiKey := ""
	key := p.conf.APIKey
	switch {
	case key.IsUnknown():
		diags.AddError("Provider api_key is unknown at apply time",
			"Generation requires a known api_key, or an unset/empty api_key to send no credentials.")
		return nil, diags
	case key.IsNull():
		apiKey = os.Getenv("LLM_API_KEY")
	default:
		// An explicit empty string intentionally disables the env fallback.
		apiKey = key.ValueString()
	}

	timeout := defaultTimeoutSeconds
	t := p.conf.TimeoutSeconds
	if t.IsUnknown() {
		diags.AddError("Provider timeout_seconds is unknown at apply time",
			"Generation requires a known timeout_seconds.")
		return nil, diags
	}
	if !t.IsNull() {
		timeout = t.ValueInt64()
	}

	c, err := newClient(url, apiKey, time.Duration(timeout)*time.Second)
	if err != nil {
		diags.AddError("Unable to configure API client", err.Error())
		return nil, diags
	}
	return c, diags
}

// validateInt64Range checks a known integer against an inclusive range.
// Unknown and null values are skipped; this is only used at plan time.
func validateInt64Range(name string, v types.Int64, min, max int64) diag.Diagnostics {
	var diags diag.Diagnostics
	if v.IsNull() || v.IsUnknown() {
		return diags
	}
	n := v.ValueInt64()
	if n < min || n > max {
		diags.AddAttributeError(path.Root(name), "Invalid "+name,
			fmt.Sprintf("%s must be between %d and %d.", name, min, max))
	}
	return diags
}
