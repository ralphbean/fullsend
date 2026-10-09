output "provider_audience" {
  description = "Audience value to request in the projected Kubernetes token."
  value       = local.provider_audience
}

output "provider_resource_name" {
  description = "Canonical workload identity provider resource name."
  value       = google_iam_workload_identity_pool_provider.kind.name
}

output "inference_service_account_email" {
  description = "IAM service account impersonated by the authorized Kubernetes workload."
  value       = google_service_account.inference.email
}

output "kubernetes_subject" {
  description = "Exact Kubernetes subject authorized by the provider and IAM binding."
  value       = local.kubernetes_subject
}
