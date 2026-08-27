locals {
  database_url = "postgres://${google_sql_user.app.name}:${random_password.db_app.result}@${google_sql_database_instance.main.private_ip_address}:5432/${google_sql_database.app.name}?sslmode=require"
}

resource "google_secret_manager_secret" "db_root_password" {
  secret_id = "${var.app_name}-db-root-password"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "db_root_password" {
  secret      = google_secret_manager_secret.db_root_password.id
  secret_data = random_password.db_root.result
}

resource "google_secret_manager_secret" "database_url" {
  secret_id = "${var.app_name}-database-url"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "database_url" {
  secret      = google_secret_manager_secret.database_url.id
  secret_data = local.database_url
}

# The one-time token that claims the first administrator account. There is no
# admin password or session secret to configure any more: accounts live in
# admin_users, and a session is a row rather than a signed cookie.
#
# Generated here rather than left to the log line the server would otherwise
# print, because reading a Cloud Run container's first boot log to catch a token
# is a worse first-deploy experience than:
#
#   gcloud secrets versions access latest --secret=gostore-setup-token
#
# It is spent by the first claim and refused for ever afterwards, so it is not a
# standing credential — but until it is claimed it opens the admin area, which is
# why it is a secret and not a variable.
resource "random_password" "setup_token" {
  length  = 43
  special = false
}

resource "google_secret_manager_secret" "setup_token" {
  secret_id = "${var.app_name}-setup-token"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "setup_token" {
  secret      = google_secret_manager_secret.setup_token.id
  secret_data = random_password.setup_token.result
}

# PayFast credentials and the SMTP password: generated/rotated out of band
# (PayFast dashboard, mail provider) and populated into these secrets by hand or
# by a deploy script — Terraform only owns the container, not values that come
# from a human decision.
resource "google_secret_manager_secret" "payfast_merchant_id" {
  secret_id = "${var.app_name}-payfast-merchant-id"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret" "payfast_merchant_key" {
  secret_id = "${var.app_name}-payfast-merchant-key"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret" "payfast_passphrase" {
  secret_id = "${var.app_name}-payfast-passphrase"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret" "smtp_password" {
  secret_id = "${var.app_name}-smtp-password"
  replication {
    auto {}
  }
}
