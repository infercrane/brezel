output "instance_name" {
  description = "Name of the disposable evaluation host."
  value       = google_compute_instance.evaluation.name
}

output "external_ip" {
  description = "Ephemeral operator address. No Brezel API firewall rule is created."
  value       = google_compute_instance.evaluation.network_interface[0].access_config[0].nat_ip
}

output "ssh_command" {
  description = "OS Login command for the operator running qualification."
  value       = "gcloud compute ssh ${google_compute_instance.evaluation.name} --project ${var.project_id} --zone ${var.zone}"
}
