terraform {
  required_version = ">= 1.6"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }

  # Skeleton only. A real setup would use a GCS backend, e.g.
  # backend "gcs" { bucket = "PROJECT_ID-tfstate" prefix = "floorrules" }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
