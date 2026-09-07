# Changelog

All notable changes to this project will be documented in this file.

## 0.1.0 (Unreleased)

### Features

- `llm_generation` resource: a logical snapshot of generated content, created
  during apply and preserved through refresh without remote requests. Destroy
  removes the snapshot from Terraform state without a remote request.
  Regeneration happens on `apply` when messages, model, generation parameters,
  or the explicit `generation_key` change.
- OpenAI-compatible Chat Completions HTTP client built on `net/http` and
  `encoding/json` (no vendor SDK), with configurable base URL, optional bearer
  authentication, bounded response reads, and explicit handling of malformed,
  refused, or truncated responses.
- Output support for plain text messages and JSON objects; JSON-object output
  is validated locally before the snapshot is committed to state.

### Testing and Documentation

- Unit tests backed by `httptest` endpoints and acceptance tests covering the
  Terraform lifecycle (generation, empty plan, refresh/destroy request counts,
  regeneration, and state preservation on failed updates).
- Registry documentation in `docs/` generated with `terraform-plugin-docs` and
  executable examples in `examples/`.

### Notes

- The provider is not yet published to the Terraform Registry, and
  compatibility with live endpoints is untested.
