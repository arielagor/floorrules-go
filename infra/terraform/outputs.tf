output "registry" {
  description = "Image path prefix for CI pushes."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.floorrules.repository_id}"
}

output "gsa_email" {
  description = "Goes into the KSA annotation iam.gke.io/gcp-service-account."
  value       = google_service_account.floorsvc.email
}

output "cloudsql_connection_name" {
  description = "Cloud SQL instance connection name."
  value       = google_sql_database_instance.main.connection_name
}
