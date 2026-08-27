resource "google_service_account" "run" {
  account_id   = "${var.app_name}-run"
  display_name = "Cloud Run runtime SA for ${var.app_name}"
}

resource "google_project_iam_member" "run_cloudsql_client" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.run.email}"
}

resource "google_secret_manager_secret_iam_member" "run_secret_access" {
  for_each = toset([
    google_secret_manager_secret.database_url.secret_id,
    google_secret_manager_secret.setup_token.secret_id,
    google_secret_manager_secret.payfast_merchant_id.secret_id,
    google_secret_manager_secret.payfast_merchant_key.secret_id,
    google_secret_manager_secret.payfast_passphrase.secret_id,
    google_secret_manager_secret.smtp_password.secret_id,
    google_secret_manager_secret.blob_secret_access_key.secret_id,
  ])
  secret_id = each.key
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.run.email}"
}

resource "google_cloud_run_v2_service" "main" {
  name     = var.app_name
  location = var.region

  template {
    service_account = google_service_account.run.email

    vpc_access {
      network_interfaces {
        network    = google_compute_network.main.id
        subnetwork = google_compute_subnetwork.run.id
      }
      egress = "PRIVATE_RANGES_ONLY"
    }

    containers {
      image = var.container_image

      env {
        name = "DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.database_url.secret_id
            version = "latest"
          }
        }
      }
      env {
        name = "SETUP_TOKEN"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.setup_token.secret_id
            version = "latest"
          }
        }
      }
      env {
        name = "PAYFAST_MERCHANT_ID"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.payfast_merchant_id.secret_id
            version = "latest"
          }
        }
      }
      env {
        name = "PAYFAST_MERCHANT_KEY"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.payfast_merchant_key.secret_id
            version = "latest"
          }
        }
      }
      env {
        name = "PAYFAST_PASSPHRASE"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.payfast_passphrase.secret_id
            version = "latest"
          }
        }
      }
      env {
        name = "SMTP_PASSWORD"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.smtp_password.secret_id
            version = "latest"
          }
        }
      }

      env {
        name = "BLOB_SECRET_ACCESS_KEY"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.blob_secret_access_key.secret_id
            version = "latest"
          }
        }
      }

      env {
        name  = "BASE_URL"
        value = var.base_url
      }
      env {
        name  = "STORE_NAME"
        value = var.store_name
      }
      env {
        name  = "CURRENCY"
        value = var.currency
      }

      # Whether this shop is really open. The server defaults it to true so a
      # laptop cannot charge a card; here it is a required variable, because a
      # production deployment silently running against the sandbox is the same
      # mistake pointing the other way. See variables.tf.
      env {
        name  = "PAYFAST_SANDBOX"
        value = tostring(var.payfast_sandbox)
      }

      # Cloud Run is always behind Google's front end, so this is not a choice
      # about topology — it is a statement of fact about where the request came
      # from. Two things break without it, and both are quiet:
      #
      #   - The payment callback's source-IP check compares PayFast's published
      #     ranges against r.RemoteAddr, which here is Google's address and never
      #     PayFast's. Every genuine notification would be rejected — so with
      #     PAYFAST_SANDBOX=false and this unset, the store takes real money and
      #     records none of it. That is worse than the sandbox bug above.
      #   - Per-IP rate limiting keys every request in the world to one bucket,
      #     so one attacker can exhaust the admin login allowance for everybody.
      #
      # The cost is that X-Forwarded-For's leftmost entry is what a client sent
      # if the platform appends rather than replaces. That weakens the source-IP
      # check specifically, which is why it is defence in depth: a forged
      # notification still needs a valid signature and still has to be confirmed
      # by PayFast's own servers. See internal/middleware/clientip.go.
      env {
        name  = "TRUST_PROXY_IP"
        value = "true"
      }

      # Product images. Required — the server refuses to boot without an image
      # backend — and until now this file set none of it, so the deployment came
      # up with images silently unavailable and the admin explaining why on every
      # product page. IMAGE_DIR is not the alternative here: Cloud Run's
      # filesystem does not survive an instance.
      #
      # The endpoint is GCS's S3 interoperability API, which is what the store
      # speaks; see storage.tf for why.
      env {
        name  = "BLOB_ENDPOINT"
        value = "storage.googleapis.com"
      }
      env {
        name  = "BLOB_BUCKET"
        value = google_storage_bucket.images.name
      }
      env {
        name  = "BLOB_ACCESS_KEY_ID"
        value = google_storage_hmac_key.images.access_id
      }
      # Where images are *read* from, which is not where they are written. The
      # bucket's own public hostname by default; override with a CDN or a custom
      # domain in front of it. Trailing path matters — the store appends the key.
      env {
        name  = "BLOB_PUBLIC_BASE_URL"
        value = coalesce(var.blob_public_base_url, "https://storage.googleapis.com/${google_storage_bucket.images.name}")
      }

      # Mail. Also required, and also previously unset — which is why the
      # SMTP_PASSWORD secret above was mounted and then read by nothing at all.
      #
      # A store must be able to send a receipt, and a digital download's link
      # exists only in that email: only its hash is stored, so a message that
      # never goes out cannot be recovered from.
      env {
        name  = "SMTP_HOST"
        value = var.smtp_host
      }
      env {
        name  = "SMTP_PORT"
        value = tostring(var.smtp_port)
      }
      env {
        name  = "SMTP_USERNAME"
        value = var.smtp_username
      }
      env {
        name  = "EMAIL_FROM"
        value = var.email_from
      }
      env {
        name  = "ORDER_NOTIFY_EMAIL"
        value = var.order_notify_email
      }

      # Cloud Logging reads "severity" and "message"; slog writes "level" and
      # "msg". Without this every line files under DEFAULT severity, so a
      # severity>=ERROR filter matches nothing and alerting never fires.
      env {
        name  = "LOG_FORMAT"
        value = "gcp"
      }

      # Purchased files (digital products) are deliberately NOT configured here.
      # They remain optional: a deployment that sells only parcels needs no
      # private bucket, and the admin says so rather than half-offering the
      # feature. To switch them on, add a second, PRIVATE bucket — never this
      # one, which is world-readable — an HMAC key for it, and DOWNLOAD_ENDPOINT,
      # DOWNLOAD_BUCKET, DOWNLOAD_ACCESS_KEY_ID and DOWNLOAD_SECRET_ACCESS_KEY.
      # DOWNLOAD_DIR is not usable on Cloud Run: the filesystem does not survive
      # an instance, so purchased files would vanish.
    }
  }

  depends_on = [google_secret_manager_secret_version.database_url]
}

resource "google_cloud_run_v2_service_iam_member" "public" {
  location = google_cloud_run_v2_service.main.location
  name     = google_cloud_run_v2_service.main.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}
