variable "project_id" {
  description = "GCP project that owns the disposable Brezel evaluation host."
  type        = string
}

variable "region" {
  description = "GCP region for the isolated evaluation network."
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = "GCP zone with N2 capacity and nested virtualization support."
  type        = string
  default     = "us-central1-a"
}

variable "name" {
  description = "Name prefix for evaluation resources."
  type        = string
  default     = "brezel-evaluation"

  validation {
    condition     = can(regex("^[a-z]([-a-z0-9]*[a-z0-9])?$", var.name)) && length(var.name) <= 40
    error_message = "name must be a lowercase GCP resource prefix of at most 40 characters."
  }
}

variable "machine_type" {
  description = "Nested-virtualization-capable machine type. n2-standard-8 fits the restricted trial CPU ceiling."
  type        = string
  default     = "n2-standard-8"
}

variable "boot_disk_size_gb" {
  description = "SSD boot disk used for source, VM artifacts, snapshots, and disposable workspaces."
  type        = number
  default     = 200

  validation {
    condition     = var.boot_disk_size_gb >= 100
    error_message = "boot_disk_size_gb must be at least 100 GiB."
  }
}

variable "ssh_source_ranges" {
  description = "Operator CIDRs permitted to reach SSH. Public-anywhere SSH is rejected."
  type        = list(string)

  validation {
    condition = (
      length(var.ssh_source_ranges) > 0 &&
      !contains(var.ssh_source_ranges, "0.0.0.0/0") &&
      !contains(var.ssh_source_ranges, "::/0") &&
      alltrue([for value in var.ssh_source_ranges : can(cidrhost(value, 0))])
    )
    error_message = "ssh_source_ranges must contain valid restricted CIDRs and cannot allow the whole internet."
  }
}
