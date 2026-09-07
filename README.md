# Terraform Provider LLM

A vendor-neutral Terraform provider for generating and persisting text and JSON
with OpenAI-compatible LLM APIs.

**Status: implemented, not published.** The provider builds, its lifecycle is
covered by acceptance tests against local mock endpoints on stable Terraform
(1.13/1.14), and documentation is generated. It is not released to the
Terraform Registry; the `vmvarela/llm` address is the intended namespace for
future publication. Until then, local use requires a
[development override](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-development)
or a filesystem mirror pointing at the built binary.

## Purpose

Generate content during `terraform apply`, store the result in Terraform state,
and reuse it until its inputs change. Other resources can consume the snapshot
through normal Terraform references.

Best suited to small, non-critical artifacts: application bootstrap content,
demo data, service descriptions, and documentation managed alongside resources.
For human review, batch processing, model evaluation, or reproducible artifacts,
generate and version the content outside Terraform instead.

Do not use unreviewed generated output to decide permissions, security policies,
network access, or destructive infrastructure changes.

## Usage

The provider is `llm`, with one resource: `llm_generation`.

```hcl
terraform {
  required_providers {
    llm = {
      source = "vmvarela/llm"
    }
  }
}

provider "llm" {
  base_url = var.llm_base_url # or LLM_BASE_URL
  # api_key = var.llm_api_key # or LLM_API_KEY
}

resource "llm_generation" "description" {
  model = var.llm_model

  messages = [
    {
      role    = "user"
      content = "Write a short description of an internal service catalog."
    }
  ]

  # Optional: temperature, top_p, max_tokens, max_completion_tokens,
  # response_format ("text" or "json_object"), generation_key.
}

output "description" {
  value     = llm_generation.description.content
  sensitive = true
}
```

`base_url` is required and accepts any OpenAI-compatible Chat Completions
endpoint, including unauthenticated local ones; there is no vendor default.
`api_key` is optional. Credentials belong in provider configuration and never
appear in resource state. Prompts and outputs are persisted in state and marked
sensitive (redacted from display, not encrypted).

## Behavior

| Operation | Behavior |
| --- | --- |
| First apply | Generate, validate, and store the response. |
| Unchanged plan or apply | Reuse the stored response without another request. |
| Changed generation inputs | Generate during an in-place update. |
| Failed update | Return an error and retain the previous snapshot. |
| Refresh | Preserve the snapshot without a remote request. |
| Destroy | Remove the snapshot from state; do not delete remote data. |

Changing messages, model, or generation parameters regenerates content. The
optional `generation_key` forces regeneration without editing the prompt, which
is the explicit trigger after a backend change; changing only provider
configuration does not regenerate existing snapshots.

Output formats are plain text and JSON objects. JSON-object output is validated
locally (a top-level JSON object) before a snapshot is saved and can be
consumed with Terraform's `jsondecode`. JSON Schema, streaming, tool execution,
and native vendor-specific APIs are out of scope.

## Important Limits

- New content is unknown until apply, so a plan cannot preview it or use it as
  newly generated `for_each` keys.
- A persisted snapshot is stable, but regeneration is not reproducible. The same
  prompt and model can produce different output, even with zero temperature.
- API compatibility varies by endpoint and model; an OpenAI-compatible label
  does not guarantee support for every generation parameter or JSON mode.
- Failed or interrupted requests may still be billed. Automatic retries cannot
  guarantee exactly-once generation across arbitrary compatible endpoints.
- Prompts and outputs are stored in Terraform state and may appear in saved
  plans. `sensitive` only redacts display. Protect state, plans, and backups.
- Destroy does not erase provider-side retention or recover request charges.
- Portable import is not planned: many endpoints cannot retrieve a prior
  generation by ID. This is a logical resource, not a remotely managed object.

## Chained Generation

The [three-stage example](examples/chained-generation/main.tf) turns a service
description into a draft, edits that draft, and packages the edited text as JSON
with `title`, `summary`, and `body` fields:

```text
service_description -> draft -> edit -> document
```

Each stage references the previous resource's `content`, so Terraform infers
the ordering without `depends_on`. The endpoint, credentials, model, and source
description are all configurable inputs.

An unchanged configuration reuses the snapshots. Changing the source can plan
regeneration through the chain; downstream content remains unknown until apply.
Each stage is saved independently, so a later failure does not roll back earlier
stages. The final postcondition checks for the expected JSON keys, not factual
accuracy or full JSON Schema conformance. All three outputs are sensitive.

## Examples

- [Provider configuration](examples/provider/provider.tf): `terraform` block,
  provider inputs, and variable conventions.
- [Single generation](examples/resources/llm_generation/resource.tf): basic
  `llm_generation` usage with optional parameters.
- [JSON-object output](examples/json-object/main.tf): validated JSON output
  consumed with `jsondecode`.
- [Chained generation](examples/chained-generation/main.tf): multi-stage
  pipeline through dependent resources.
