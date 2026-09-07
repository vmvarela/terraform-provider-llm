# Chained generation example: a service description becomes a draft, then an
# edit, then a JSON document. Each stage stores its own snapshot. A downstream
# failure does not roll back a successful upstream generation. No explicit
# depends_on is needed.

terraform {
  required_providers {
    llm = {
      source = "vmvarela/llm"
    }
  }
}

variable "base_url" {
  description = "Root URL of an OpenAI-compatible Chat Completions API."
  type        = string
}

variable "api_key" {
  description = "Optional bearer token for the endpoint."
  type        = string
  default     = null
  sensitive   = true
}

variable "model" {
  description = "Model identifier accepted by the configured endpoint."
  type        = string
}

variable "service_description" {
  description = "Source material for the chain. Changes propagate through the dependent generations."
  type        = string
  default     = "Atlas is an internal developer portal that helps engineers discover service owners and onboarding guides."
}

provider "llm" {
  base_url = var.base_url
  api_key  = var.api_key
}

# Stage 1: turn the source description into a short draft.
resource "llm_generation" "draft" {
  model = var.model
  messages = [
    {
      role    = "system"
      content = "Write a short service introduction under 80 words. Preserve the service name and supplied facts. Do not invent features. Return only the draft."
    },
    {
      role    = "user"
      content = var.service_description
    },
  ]
}

# Stage 2: consume the draft, not the original description.
resource "llm_generation" "edit" {
  model = var.model
  messages = [
    {
      role    = "system"
      content = "Edit this service introduction for clarity and remove repetition. Keep its name and facts, add no claims, and stay under 80 words. Return only the revised text."
    },
    {
      role    = "user"
      content = llm_generation.draft.content
    },
  ]
}

# Stage 3: package the edited text as JSON for another resource to consume.
resource "llm_generation" "document" {
  model           = var.model
  response_format = "json_object"
  messages = [
    {
      role    = "system"
      content = "Return only a JSON object with three string fields: title, summary, and body. Use the service name as title, summarize in one sentence, and preserve the provided text as body."
    },
    {
      role    = "user"
      content = llm_generation.edit.content
    },
  ]

  # JSON mode validates the object locally. This extra check requires the
  # expected keys; failure blocks dependents but does not undo the snapshot.
  lifecycle {
    postcondition {
      condition = (
        can(jsondecode(self.content).title) &&
        can(jsondecode(self.content).summary) &&
        can(jsondecode(self.content).body)
      )
      error_message = "The generated document must contain title, summary, and body."
    }
  }
}

output "draft" {
  description = "First-stage draft. Sensitive values still persist in state."
  value       = llm_generation.draft.content
  sensitive   = true
}

output "edited" {
  description = "Second-stage revised text."
  value       = llm_generation.edit.content
  sensitive   = true
}

output "document" {
  description = "Final JSON object, ready to reference from another resource."
  value       = jsondecode(llm_generation.document.content)
  sensitive   = true
}
