variable "project_id" {
  description = "GCP project to deploy into."
  type        = string
}

variable "region" {
  description = "Region for the cluster, registry and database."
  type        = string
  default     = "us-central1"
}

variable "network" {
  description = "Self link of an existing VPC with Private Service Access configured for Cloud SQL."
  type        = string
}

variable "subnetwork" {
  description = "Self link of the subnetwork for the GKE cluster."
  type        = string
}

variable "master_authorized_cidr" {
  description = "The only range allowed to reach the GKE control plane endpoint (operators' VPN or CI egress), e.g. 203.0.113.0/28."
  type        = string
}

variable "k8s_namespace" {
  description = "Namespace the service runs in; must match deploy/k8s."
  type        = string
  default     = "floorrules"
}

variable "k8s_service_account" {
  description = "Kubernetes ServiceAccount name; must match deploy/k8s."
  type        = string
  default     = "floorsvc"
}

variable "k8s_migrate_service_account" {
  description = "Kubernetes ServiceAccount of the migrate Job; must match deploy/k8s."
  type        = string
  default     = "floorsvc-migrate"
}
