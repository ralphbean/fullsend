variable "project_id" {
  description = "Project that owns the WIF pool, provider, and inference service account."
  type        = string
}

variable "project_number" {
  description = "Numeric project number used in the provider audience and federated principal."
  type        = string

  validation {
    condition     = can(regex("^[0-9]+$", var.project_number))
    error_message = "project_number must contain only decimal digits."
  }
}

variable "pool_id" {
  description = "Dedicated workload identity pool ID."
  type        = string
  default     = "fullsend-kind-poc"
}

variable "provider_id" {
  description = "OIDC provider ID inside the pool."
  type        = string
  default     = "kind-local"
}

variable "issuer_uri" {
  description = "Issuer from the current kind API server OpenID configuration."
  type        = string

  validation {
    condition     = can(regex("^https://", var.issuer_uri))
    error_message = "issuer_uri must use HTTPS."
  }
}

variable "jwks_file" {
  description = "Path to the current public kind JWKS JSON file. Refresh after every cluster recreation."
  type        = string
}

variable "kubernetes_namespace" {
  description = "Namespace allowed to request inference credentials."
  type        = string
  default     = "fullsend-runner"
}

variable "kubernetes_service_account" {
  description = "Exact Kubernetes ServiceAccount allowed to request inference credentials."
  type        = string
  default     = "runner-inference"
}

variable "inference_service_account_id" {
  description = "IAM service account ID with scoped Vertex inference access."
  type        = string
  default     = "fullsend-kind-poc-inference"
}
