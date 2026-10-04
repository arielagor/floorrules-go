# Skeleton: validated with `terraform validate`, never planned or applied.

# --- Container registry ---
resource "google_artifact_registry_repository" "floorrules" {
  location      = var.region
  repository_id = "floorrules"
  format        = "DOCKER"
  description   = "floorsvc images, pushed by CI and referenced by digest."

  docker_config {
    immutable_tags = true
  }
}

# --- GKE Autopilot ---
resource "google_container_cluster" "main" {
  name             = "floorrules"
  location         = var.region
  enable_autopilot = true
  network          = var.network
  subnetwork       = var.subnetwork

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
  }

  release_channel {
    channel = "REGULAR"
  }

  # Only admit images that pass Binary Authorization policy (signed by CI).
  binary_authorization {
    evaluation_mode = "PROJECT_SINGLETON_POLICY_ENFORCE"
  }

  deletion_protection = true
}

# --- Cloud SQL for PostgreSQL, private IP only ---
resource "google_sql_database_instance" "main" {
  name             = "floorrules-pg"
  database_version = "POSTGRES_16"
  region           = var.region

  settings {
    tier              = "db-custom-1-3840"
    availability_type = "REGIONAL"

    ip_configuration {
      ipv4_enabled    = false
      private_network = var.network
      ssl_mode        = "ENCRYPTED_ONLY"
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
    }

    database_flags {
      name  = "log_min_duration_statement"
      value = "500"
    }
  }

  deletion_protection = true
}

resource "google_sql_database" "floorrules" {
  name     = "floorrules"
  instance = google_sql_database_instance.main.name
}

# --- Identity: KSA -> GSA via Workload Identity, no keys ---
resource "google_service_account" "floorsvc" {
  account_id   = "floorsvc"
  display_name = "floorsvc runtime"
}

resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.floorsvc.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.k8s_namespace}/${var.k8s_service_account}]"
}

resource "google_project_iam_member" "cloudsql_client" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.floorsvc.email}"
}

# Secrets live in Secret Manager; values are added out of band, never in state.
resource "google_secret_manager_secret" "auth_hmac" {
  secret_id = "floorsvc-auth-hmac-secret"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret" "database_url" {
  secret_id = "floorsvc-database-url"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_iam_member" "auth_hmac_access" {
  secret_id = google_secret_manager_secret.auth_hmac.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.floorsvc.email}"
}

resource "google_secret_manager_secret_iam_member" "database_url_access" {
  secret_id = google_secret_manager_secret.database_url.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.floorsvc.email}"
}
