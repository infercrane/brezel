# GCP evaluation host

This module provisions one disposable Ubuntu 24.04 x86-64 host with nested KVM,
an SSD boot disk, an isolated VPC, and SSH restricted to operator CIDRs. It does
not install Brezel, expose the API, create an availability claim, or mark the
host qualified.

```console
gcloud auth application-default login
gcloud auth application-default set-quota-project "$GOOGLE_CLOUD_PROJECT"

terraform init
terraform apply \
  -var="project_id=$GOOGLE_CLOUD_PROJECT" \
  -var='ssh_source_ranges=["203.0.113.8/32"]'

terraform output -raw ssh_command
```

Copy the printed SSH command into a new shell. Do not evaluate Terraform output
as shell code.

On the host, clone the exact revision under evaluation and follow
[`docs/GO-LIVE.md`](../../../docs/GO-LIVE.md). A successful Terraform apply is
not a successful `make qualify-single-host`.

No service account is attached, and no firewall rule exposes the Brezel API.
Destroy the host when the evidence has been copied out:

```console
terraform destroy \
  -var="project_id=$GOOGLE_CLOUD_PROJECT" \
  -var='ssh_source_ranges=["203.0.113.8/32"]'
```
