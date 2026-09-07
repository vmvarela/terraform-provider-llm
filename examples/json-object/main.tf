# Example of JSON-object output. The generated content is parsed with
# jsondecode, so the API must return a valid JSON object (validated by the
# provider before the snapshot is saved).

terraform {
  required_providers {
    llm = {
      source = "vmvarela/llm"
    }
  }
}

provider "llm" {
  base_url = var.base_url
  api_key  = var.api_key
}

variable "base_url" {
  description = "Root URL of an OpenAI-compatible Chat Completions API."
  type        = string
}

variable "api_key" {
  type      = string
  default   = null
  sensitive = true
}

variable "model" {
  description = "Model identifier accepted by the configured endpoint."
  type        = string
}

resource "llm_generation" "example" {
  model           = var.model
  response_format = "json_object"

  messages = [
    {
      role    = "user"
      content = "Return a JSON object with the keys \"greeting\" and \"target\". Reply with the JSON object only."
    },
  ]

  generation_key = "example-v1"

  # Regeneration replaces the snapshot. A failed postcondition blocks
  # dependents but does not roll back content already written to state.
}

locals {
  parsed = jsondecode(llm_generation.example.content)
}

output "greeting" {
  value = local.parsed.greeting
}
