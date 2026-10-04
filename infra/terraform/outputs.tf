output "registry" {
  description = "Image path prefix for CI pushes."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.floorrules.repository_id}"
}

output "binauthz_attestor" {
  description = "Attestor CI signs each pushed digest for (gcloud container binauthz attestations sign-and-create --attestor)."
  value       = google_binary_authorization_attestor.ci.id
}

output "binauthz_signing_key_version" {
  description = "KMS key version CI signs attestations with."
  value       = data.google_kms_crypto_key_version.attestor.id
}

output "cloudsql_private_ip" {
  description = "Host for the DSNs stored in Secret Manager (sslmode=verify-ca)."
  value       = google_sql_database_instance.main.private_ip_address
}
