# Infrastructure (Google Cloud)

Terraform for the production deployment: Cloud Run, a private-IP Cloud SQL
Postgres instance, Secret Manager, and Artifact Registry. Every resource here
is provider-specific to `google` — see "Portability" below.

## What this creates

- **`network.tf`** — a custom-mode VPC, a per-region subnet for Cloud Run's
  Direct VPC egress, and the one-time private-services peering that lets a
  private-IP Cloud SQL instance live inside the VPC.
- **`sql.tf`** — the Cloud SQL instance (private IP only, `ssl_mode =
  ENCRYPTED_ONLY`), the `gostore` database, and the `gostore` application
  user. Two passwords are generated and kept separate on purpose: the
  instance's root/superuser password (provisioning only, never read by the
  app) and the app user's password (what `DATABASE_URL` actually uses).
  `prevent_destroy` is set — an accidental `terraform destroy` must not be
  able to take out the database.
- **`secrets.tf`** — `DATABASE_URL` is assembled from the values above and
  written to Secret Manager automatically, and so is `SETUP_TOKEN`: the
  one-time token that claims the first administrator account at
  `/admin/setup`. Read it after `apply` with
  ```sh
  gcloud secrets versions access latest --secret=gostore-setup-token
  ```
  There is no admin password or session secret to supply — accounts live in
  the database and a session is a row in `admin_sessions`. The PayFast
  credentials and `SMTP_PASSWORD` get empty secret *containers* only:
  Terraform owns the infrastructure, not values that come from a human
  decision (PayFast's dashboard, the mail provider). Add versions to those by
  hand after `apply`:
  ```sh
  gcloud secrets versions add gostore-payfast-merchant-key --data-file=-
  ```
- **`run.tf`** — a dedicated Cloud Run runtime service account (not the
  project's default Compute Engine service account — see "Why a dedicated
  service account" below), the Cloud Run service itself wired to the VPC
  subnet with `PRIVATE_RANGES_ONLY` egress, and public invoker access.
- **`registry.tf`** — the Artifact Registry Docker repository the image is
  pushed to before `apply` (Terraform deploys whatever `container_image`
  points at; it does not build or push it).

## Going live: two variables that are not defaults

Both of these were missing, and both fail quietly rather than loudly.

**`payfast_sandbox` is required, with no default.** The server defaults
`PAYFAST_SANDBOX` to `true`, so that a first afternoon with the project cannot
charge a real card. That default is right for a laptop and wrong here: this file
never set the variable, so **the deployment ran against PayFast's sandbox and
could not take real money** — the same mistake as the default is guarding
against, pointing the other way. There is no safe default for "is this shop
really open", so Terraform refuses to plan until you say.

Turning it off is two changes, not one: the merchant credentials in Secret
Manager must be yours. The server refuses to start with `PAYFAST_SANDBOX=false`
and PayFast's published sandbox merchant id, because that combination signs
every payment with a key printed in PayFast's documentation.

**`TRUST_PROXY_IP` is set to `true`, and on Cloud Run that is a fact rather
than a preference.** Requests always arrive through Google's front end, so
`RemoteAddr` is Google's address and never the caller's. Two things break
without it, both silently:

- The payment callback checks the notification's source IP against PayFast's
  published ranges. Comparing those to Google's address rejects **every genuine
  notification** — so with real payments switched on and this unset, the store
  takes money and records none of it.
- Per-IP rate limiting keys the entire internet to one bucket, so one attacker
  can exhaust the admin login allowance for everybody.

The cost is that `X-Forwarded-For`'s leftmost entry is client-supplied on any
platform that appends rather than replaces. That weakens the source-IP check
specifically, which is why it is defence in depth: a forged notification still
needs a valid signature and still has to be confirmed by PayFast's own servers.
See `internal/middleware/clientip.go`.

## Why a dedicated service account

The Cloud Run quickstart grants roles directly to the project's default
Compute Engine service account. That account is shared by every Compute
Engine VM, GKE node, or other Cloud Run service in the project that doesn't
specify its own — granting it `cloudsql.client` widens what *any* of those
can reach, not just this service, and it's harder to audit later since the
binding doesn't say which workload needed it. `run.tf` instead creates
`gostore-run`, scoped to exactly `roles/cloudsql.client` and
`secretmanager.secretAccessor` on the specific secrets this service reads.

Also skipped: the quickstart's `roles/storage.objectViewer` grant. gostore's
blob storage (`internal/blob`, on minio-go) authenticates with HMAC
access-key/secret-key pairs (`BLOB_ACCESS_KEY_ID`/`BLOB_SECRET_ACCESS_KEY`),
not the service account's own IAM identity — that role would be unused.

## Usage

```sh
cd infra/terraform
cp terraform.tfvars.example terraform.tfvars   # fill in project_id, container_image, etc.
terraform init
terraform plan
terraform apply
```

State is local by default (`terraform.tfstate`), gitignored — it contains
both generated passwords in plaintext. Fine solo; move to a GCS backend
before anyone else touches this.

## Portability

Not portable to another cloud without a rewrite. Terraform the tool is
provider-agnostic, but every resource block here (`google_sql_database_instance`,
`google_compute_network`, `google_cloud_run_v2_service`,
`google_artifact_registry_repository`, `google_secret_manager_secret`) is
part of the `google` provider and has no equivalent on another provider's
schema. Moving to AWS or Azure means writing new resource blocks against
that provider (e.g. `aws_db_instance`, `aws_ecs_service` or an Azure
equivalent) — variables and workflow (`init`/`plan`/`apply`) are the only
parts that carry over.
