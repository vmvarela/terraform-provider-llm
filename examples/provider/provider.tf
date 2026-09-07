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
  description = "Optional bearer token for the endpoint. Leave null for unauthenticated local endpoints."
  type        = string
  default     = null
  sensitive   = true
}
