terraform {
  required_version = "= 1.8.8"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "= 7.22.0"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = "global"
}

locals {
  provider_audience = "//iam.googleapis.com/projects/${var.project_number}/locations/global/workloadIdentityPools/${var.pool_id}/providers/${var.provider_id}"
  kubernetes_subject = "system:serviceaccount:${var.kubernetes_namespace}:${var.kubernetes_service_account}"
  workload_identity_principal = "principal://iam.googleapis.com/projects/${var.project_number}/locations/global/workloadIdentityPools/${var.pool_id}/subject/${local.kubernetes_subject}"
}

resource "google_project_service" "required" {
  for_each = toset([
    "aiplatform.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "serviceusage.googleapis.com",
    "sts.googleapis.com",
  ])

  project            = var.project_id
  service            = each.key
  disable_on_destroy = false
}

resource "google_iam_workload_identity_pool" "kind" {
  workload_identity_pool_id = var.pool_id
  display_name              = "Fullsend kind inference"
  description               = "Local kind identity for the Fullsend inference PoC"
  project                   = var.project_id
  disabled                  = false

  lifecycle {
    prevent_destroy = true
  }

  depends_on = [google_project_service.required]
}

resource "google_iam_workload_identity_pool_provider" "kind" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.kind.workload_identity_pool_id
  workload_identity_pool_provider_id = var.provider_id
  display_name                       = "Local kind"
  description                        = "Accepts inference tokens only from the configured runner ServiceAccount"
  project                            = var.project_id
  attribute_mapping = {
    "google.subject"        = "assertion.sub"
    "attribute.namespace"   = "assertion['kubernetes.io']['namespace']"
    "attribute.serviceaccount" = "assertion['kubernetes.io']['serviceaccount']['name']"
  }
  attribute_condition = "assertion['kubernetes.io']['namespace'] == \"${var.kubernetes_namespace}\" && assertion.sub == \"${local.kubernetes_subject}\""

  oidc {
    issuer_uri        = var.issuer_uri
    allowed_audiences = [local.provider_audience]
    jwks_json         = file(var.jwks_file)
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_service_account" "inference" {
  project      = var.project_id
  account_id   = var.inference_service_account_id
  display_name = "Fullsend kind inference"
  description  = "Scoped Vertex identity impersonated by the kind runner"

  lifecycle {
    prevent_destroy = true
  }

  depends_on = [google_project_service.required]
}

resource "google_service_account_iam_member" "kubernetes_workload" {
  service_account_id = google_service_account.inference.name
  role               = "roles/iam.workloadIdentityUser"
  member             = local.workload_identity_principal

  depends_on = [google_iam_workload_identity_pool_provider.kind]
}

resource "google_project_iam_member" "vertex_inference" {
  project = var.project_id
  role    = "roles/aiplatform.user"
  member  = "serviceAccount:${google_service_account.inference.email}"
}

resource "google_project_iam_member" "service_usage" {
  project = var.project_id
  role    = "roles/serviceusage.serviceUsageConsumer"
  member  = "serviceAccount:${google_service_account.inference.email}"
}
