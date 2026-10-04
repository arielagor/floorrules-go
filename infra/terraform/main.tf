# Skeleton: validated with `terraform validate`, never planned or applied.

# Secret access is granted to Workload Identity Federation principals for
# Kubernetes ServiceAccounts: the Secret Manager CSI add-on authenticates as
# the pod's KSA, so no Google service account is involved.
data "google_project" "this" {}

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

  # The endpoint stays reachable for CI and operators, but only from the
  # listed range; Google Cloud public addresses are not admitted by default.
  master_authorized_networks_config {
    gcp_public_cidrs_access_enabled = false
    cidr_blocks {
      cidr_block   = var.master_authorized_cidr
      display_name = "operators"
    }
  }

  release_channel {
    channel = "REGULAR"
  }

  # Enforces google_binary_authorization_policy.this below. Without that
  # resource the project's default policy admits every image.
  binary_authorization {
    evaluation_mode = "PROJECT_SINGLETON_POLICY_ENFORCE"
  }

  # Secret Manager add-on: pods mount secrets with the
  # secrets-store-gke.csi.k8s.io driver (deploy/k8s/secretproviderclass.yaml).
  secret_manager_config {
    enabled = true
  }

  deletion_protection = true
}

# --- Binary Authorization: only images CI attested may run ---
# CI signs the pushed digest with the KMS key below
# (gcloud container binauthz attestations sign-and-create). Granting CI
# roles/cloudkms.signerVerifier and roles/containeranalysis.notes.attacher
# belongs with its Workload Identity Federation setup, which this sample
# does not include.
resource "google_kms_key_ring" "binauthz" {
  name     = "floorrules-binauthz"
  location = var.region
}

resource "google_kms_crypto_key" "attestor" {
  name     = "ci-attestor"
  key_ring = google_kms_key_ring.binauthz.id
  purpose  = "ASYMMETRIC_SIGN"

  version_template {
    algorithm = "RSA_SIGN_PKCS1_4096_SHA512"
  }

  lifecycle {
    prevent_destroy = true
  }
}

data "google_kms_crypto_key_version" "attestor" {
  crypto_key = google_kms_crypto_key.attestor.id
}

resource "google_container_analysis_note" "ci" {
  name = "floorrules-ci-attestor-note"
  attestation_authority {
    hint {
      human_readable_name = "floorrules CI built and tested this digest"
    }
  }
}

resource "google_binary_authorization_attestor" "ci" {
  name = "floorrules-ci"
  attestation_authority_note {
    note_reference = google_container_analysis_note.ci.name
    public_keys {
      id = data.google_kms_crypto_key_version.attestor.id
      pkix_public_key {
        public_key_pem      = data.google_kms_crypto_key_version.attestor.public_key[0].pem
        signature_algorithm = data.google_kms_crypto_key_version.attestor.public_key[0].algorithm
      }
    }
  }
}

resource "google_binary_authorization_policy" "this" {
  # Google-maintained system images (GKE add-ons) stay admitted.
  global_policy_evaluation_mode = "ENABLE"

  default_admission_rule {
    evaluation_mode         = "REQUIRE_ATTESTATION"
    enforcement_mode        = "ENFORCED_BLOCK_AND_AUDIT_LOG"
    require_attestations_by = [google_binary_authorization_attestor.ci.name]
  }
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

# --- Secrets ---
# Values for the DSNs and the HMAC key are added out of band, never in
# state. Each DSN uses sslmode=verify-ca with
# sslrootcert=/var/run/secrets/floorrules/server-ca.pem; the service refuses
# any DSN that does not verify the server (internal/config).
#
# The pods connect to the private IP with a password DSN, so no identity
# needs the Cloud SQL Client role, and they call no Google API themselves.
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

resource "google_secret_manager_secret" "migrate_database_url" {
  secret_id = "floorsvc-migrate-database-url"
  replication {
    auto {}
  }
}

# The Cloud SQL server CA is public; it is a secret only so it arrives
# through the same CSI mount as the DSN that references it.
resource "google_secret_manager_secret" "db_server_ca" {
  secret_id = "floorsvc-db-server-ca"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "db_server_ca" {
  secret      = google_secret_manager_secret.db_server_ca.id
  secret_data = google_sql_database_instance.main.server_ca_cert[0].cert
}

# --- Secret access, per Kubernetes ServiceAccount ---
# floorsvc (the service) reads its HMAC key, its DSN and the server CA.
# floorsvc-migrate (the migrate Job) reads the DSN of floorsvc_migrator, the
# role that owns the schema (deploy/sql/roles.sql), and the server CA.
resource "google_secret_manager_secret_iam_member" "auth_hmac_access" {
  secret_id = google_secret_manager_secret.auth_hmac.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "principal://iam.googleapis.com/projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/${var.k8s_namespace}/sa/${var.k8s_service_account}"
}

resource "google_secret_manager_secret_iam_member" "database_url_access" {
  secret_id = google_secret_manager_secret.database_url.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "principal://iam.googleapis.com/projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/${var.k8s_namespace}/sa/${var.k8s_service_account}"
}

resource "google_secret_manager_secret_iam_member" "db_server_ca_access" {
  secret_id = google_secret_manager_secret.db_server_ca.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "principal://iam.googleapis.com/projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/${var.k8s_namespace}/sa/${var.k8s_service_account}"
}

resource "google_secret_manager_secret_iam_member" "migrate_database_url_access" {
  secret_id = google_secret_manager_secret.migrate_database_url.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "principal://iam.googleapis.com/projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/${var.k8s_namespace}/sa/${var.k8s_migrate_service_account}"
}

resource "google_secret_manager_secret_iam_member" "migrate_db_server_ca_access" {
  secret_id = google_secret_manager_secret.db_server_ca.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "principal://iam.googleapis.com/projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/${var.k8s_namespace}/sa/${var.k8s_migrate_service_account}"
}
