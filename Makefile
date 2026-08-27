COMPOSE ?= docker compose
TEST_DATABASE_URL ?= postgres://gostore:gostore@localhost:5432/gostore?sslmode=disable

# There are no admin credentials to set: the first administrator is claimed at
# /admin/setup with the one-time token `make run` prints on a fresh database.
# SETUP_TOKEN, if exported, supplies that token instead of having one generated.
# PayFast's own published sandbox credentials, matching compose.yaml, so that
# `make run`, `make migrate` and `make seed` work on a clean checkout with no
# .env at all. They are in PayFast's documentation and take no real money —
# but PAYFAST_SANDBOX must stay true for that to remain the case.
PAYFAST_MERCHANT_ID ?= 10000100
PAYFAST_MERCHANT_KEY ?= 46f0cd694581a
PAYFAST_PASSPHRASE ?= jt7NOE43FZPn
PAYFAST_SANDBOX ?= true
# Recipes using DEV_ENV are prefixed with @ so a real PAYFAST_PASSPHRASE or
# SETUP_TOKEN is not echoed into a terminal or a CI log.
DEV_ENV = DATABASE_URL="$(TEST_DATABASE_URL)" \
	SETUP_TOKEN="$(SETUP_TOKEN)" \
	PAYFAST_MERCHANT_ID="$(PAYFAST_MERCHANT_ID)" \
	PAYFAST_MERCHANT_KEY="$(PAYFAST_MERCHANT_KEY)" \
	PAYFAST_PASSPHRASE="$(PAYFAST_PASSPHRASE)" \
	PAYFAST_SANDBOX="$(PAYFAST_SANDBOX)" \
	DOWNLOAD_ENDPOINT="$(DOWNLOAD_ENDPOINT)" \
	DOWNLOAD_BUCKET="$(DOWNLOAD_BUCKET)" \
	DOWNLOAD_ACCESS_KEY_ID="$(DOWNLOAD_ACCESS_KEY_ID)" \
	DOWNLOAD_SECRET_ACCESS_KEY="$(DOWNLOAD_SECRET_ACCESS_KEY)" \
	DOWNLOAD_USE_TLS="$(DOWNLOAD_USE_TLS)" \
	IMAGE_DIR="$(IMAGE_DIR)" \
	SMTP_HOST="$(SMTP_HOST)" \
	SMTP_PORT="$(SMTP_PORT)" \
	SMTP_TLS="$(SMTP_TLS)" \
	EMAIL_FROM="$(EMAIL_FROM)"

# Images and mail are required, so `make run` has to supply both. A directory
# under .local keeps uploaded photographs out of the working tree; mailpit is the
# compose relay, which `make run` starts alongside postgres.
IMAGE_DIR ?= .local/images
SMTP_HOST ?= localhost
SMTP_PORT ?= 1025
SMTP_TLS ?= none
EMAIL_FROM ?= orders@gostore.example

# Purchased files live in the compose MinIO for both `make run` and `make seed`,
# which run on the host and so reach it at the same address a browser does. The
# compose *server* reaches it at minio:9000 and signs for localhost:9000 —
# see DOWNLOAD_PUBLIC_ENDPOINT in compose.yaml — so all three agree on where a
# seeded file actually is.
DOWNLOAD_ENDPOINT ?= localhost:9000
DOWNLOAD_BUCKET ?= gostore-downloads
DOWNLOAD_ACCESS_KEY_ID ?= gostore
DOWNLOAD_SECRET_ACCESS_KEY ?= gostore123
DOWNLOAD_USE_TLS ?= false

SEED_FILE ?= testdata/products.json

# `make run` serves ./theme and re-reads it on every request, so a theme edit
# needs a page refresh rather than a restart. Never on in a deployment.
THEME_RELOAD ?= true

# sqlc generates the row structs and scan code for the stores. It is pinned here
# rather than as a `go tool` directive in go.mod, so that go.mod keeps stating the
# dependencies of the *binary* — sqlc adds about forty indirect modules and never
# links into it. See the README's dependency section.
SQLC_VERSION ?= v1.31.1
SQLC ?= sqlc

.PHONY: up down logs run build test vet fmt tidy psql migrate migrate-status seed hashpw \
	check-config sqlc sqlc-check sqlc-install

## up: build and start the whole local stack
up:
	$(COMPOSE) up --build -d
	@echo "server   http://localhost:8080/healthz"
	@echo "mailpit  http://localhost:8025"
	@echo "minio    http://localhost:9001"

## down: stop the stack (add ARGS=-v to also delete data volumes)
down:
	$(COMPOSE) down $(ARGS)

logs:
	$(COMPOSE) logs -f server

## run: run the server on the host against the compose Postgres
# Themed from ./theme with reloading on, matching the compose stack: edit a file
# there and refresh, no restart. THEME_RELOAD=false for the read-once behaviour a
# deployment has.
run:
	$(COMPOSE) up -d postgres mailpit minio
	@$(DEV_ENV) TEMPLATE_DIR=theme/templates STATIC_DIR=theme/static \
		THEME_RELOAD="$(THEME_RELOAD)" go run .

## migrate: apply pending migrations without starting the server
migrate:
	$(COMPOSE) up -d postgres
	@$(DEV_ENV) go run . -migrate

## seed: load a products JSON file (SEED_FILE=...) into the database
# Depends on migrate, so seeding a database nobody has migrated yet reports a
# missing migration rather than a missing table.
seed: migrate
	@$(COMPOSE) up -d minio minio-init
	@DATABASE_URL="$(TEST_DATABASE_URL)" \
		DOWNLOAD_ENDPOINT="$(DOWNLOAD_ENDPOINT)" \
		DOWNLOAD_BUCKET="$(DOWNLOAD_BUCKET)" \
		DOWNLOAD_ACCESS_KEY_ID="$(DOWNLOAD_ACCESS_KEY_ID)" \
		DOWNLOAD_SECRET_ACCESS_KEY="$(DOWNLOAD_SECRET_ACCESS_KEY)" \
		DOWNLOAD_USE_TLS="$(DOWNLOAD_USE_TLS)" \
		go run ./cmd/seed -file "$(SEED_FILE)"

## migrate-status: show which migrations have been applied
migrate-status:
	@$(DEV_ENV) go run . -migrate-status

## check-config: validate the full server configuration without starting anything
# The migration targets deliberately need only DATABASE_URL, so this is what
# catches a missing payment credential or an unreadable password hash — run it
# in a deploy before -migrate, to fail before the schema moves rather than after.
check-config:
	@$(DEV_ENV) go run . -check-config

## hashpw: read a password from the terminal and print an argon2id hash
# A lockout-recovery path, not part of setup: the first administrator is claimed at
# /admin/setup. See cmd/hashpw for the UPDATE this hash goes into, and the DELETE
# that must go with it.
#
# The password is never echoed and never becomes a command-line argument, so it
# stays out of shell history and out of `ps`.
hashpw:
	@read -rs -p "Admin password: " P; echo; printf %s "$$P" | go run ./cmd/hashpw

## sqlc: regenerate internal/db/gen from the queries and the migrations
sqlc:
	$(SQLC) generate

## sqlc-check: fail if the checked-in generated code is stale
# What CI runs. `sqlc diff` compares what would be generated against what is on
# disk, so a query edited without regenerating is caught on the PR rather than by
# a reviewer noticing the SQL and the Go disagree.
sqlc-check:
	$(SQLC) diff

## sqlc-install: install the pinned sqlc
sqlc-install:
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)

build:
	go build ./...

## test: run every test, including the database-backed ones
test:
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

tidy:
	go mod tidy

psql:
	$(COMPOSE) exec postgres psql -U gostore -d gostore
