variable "model" {
  description = "Model identifier accepted by the configured endpoint."
  type        = string
}

resource "llm_generation" "example" {
  model = var.model

  messages = [
    {
      role    = "user"
      content = "In one sentence, explain what a Terraform provider does."
    },
  ]

  # Optional generation parameters (all omitted by default):
  # temperature           = 0.2
  # top_p                 = 0.9
  # max_tokens            = 256
  # max_completion_tokens = 256 # mutually exclusive with max_tokens
  # response_format       = "text" # or "json_object"

  # Changing generation_key explicitly regenerates the snapshot; provider
  # configuration changes alone do not.
  generation_key = "example-v1"
}

# The snapshot is a logical resource: Read never regenerates, and a failed
# regeneration preserves the previous snapshot. Validate content with
# postconditions to block dependents; they do not roll back state.
output "content" {
  description = "Generated content."
  value       = llm_generation.example.content
  sensitive   = true
}
