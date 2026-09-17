resource "google_project_service" "compute" {
  project            = var.project_id
  service            = "compute.googleapis.com"
  disable_on_destroy = false
}

resource "google_compute_network" "evaluation" {
  name                    = "${var.name}-network"
  auto_create_subnetworks = false

  depends_on = [google_project_service.compute]
}

resource "google_compute_subnetwork" "evaluation" {
  name          = "${var.name}-subnet"
  ip_cidr_range = "10.91.0.0/24"
  network       = google_compute_network.evaluation.id
  region        = var.region
}

resource "google_compute_firewall" "ssh" {
  name          = "${var.name}-ssh"
  network       = google_compute_network.evaluation.name
  direction     = "INGRESS"
  source_ranges = var.ssh_source_ranges
  target_tags   = ["brezel-evaluation"]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

resource "google_compute_instance" "evaluation" {
  name                      = var.name
  machine_type              = var.machine_type
  zone                      = var.zone
  allow_stopping_for_update = true
  tags                      = ["brezel-evaluation"]

  boot_disk {
    auto_delete = true

    initialize_params {
      image = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"
      size  = var.boot_disk_size_gb
      type  = "pd-ssd"
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.evaluation.id

    access_config {
      network_tier = "PREMIUM"
    }
  }

  advanced_machine_features {
    enable_nested_virtualization = true
  }

  shielded_instance_config {
    enable_integrity_monitoring = true
    enable_secure_boot          = true
    enable_vtpm                 = true
  }

  metadata = {
    block-project-ssh-keys = "TRUE"
    enable-oslogin         = "TRUE"
  }

  scheduling {
    automatic_restart   = true
    on_host_maintenance = "MIGRATE"
    provisioning_model  = "STANDARD"
  }

  # Deliberately omit a service_account block. Qualification does not need an
  # ambient Google identity on the host or inside a nested guest.

  depends_on = [google_project_service.compute]
}
