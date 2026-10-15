# Deploying forge-hris (component 1)

## Docker Compose (recommended)

```bash
cp .env.example .env   # optional; sane defaults are built in
docker compose up -d
```

This starts two services:

| Service    | Image / build        | Notes |
|------------|----------------------|-------|
| `postgres` | `postgres:16-alpine` | Data persists in the `pgdata` volume. The component-1 schema (`store/migrations/001_init.sql`) is mounted at `/docker-entrypoint-initdb.d` so a **first** init creates the schema; later boots are no-ops. |
| `api`      | built from `Dockerfile` | REST API on port 8080. On **every boot** it runs the migrations embedded in the binary (`store.ApplyMigration`) before listening. All migrations are idempotent (`IF NOT EXISTS` / `DROP IF EXISTS`), so restarts and version upgrades are safe. |

Health:

```bash
curl localhost:8080/healthz
# {"status":"ok"}

docker compose ps   # both services should show "healthy"
```

Tear down (keeps the data volume):

```bash
docker compose down
```

Tear down **including** the database:

```bash
docker compose down -v
```

## Environment variables

| Variable | Default | Meaning |
|----------|---------|---------|
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | `forge` / `forge` / `forge_hris` | Postgres credentials and database name. Change the password in production (`.env`). |
| `POSTGRES_PORT` | `5432` | Host port mapped to Postgres. |
| `API_PORT` | `8080` | Host port mapped to the API. |
| `DATABASE_URL` | built from the above | Postgres connection string for the API. Override directly when Postgres lives elsewhere. |
| `PORT` | `8080` | Port the API listens on **inside** its container. |

## How the API picks its storage

`cmd/forge-hris-api` selects storage from the environment:

- `DATABASE_URL` **set** → Postgres via the pgx adapter. Schema is auto-applied on boot (see above); the process exits non-zero if migration fails.
- `DATABASE_URL` **unset** → in-memory store. Data is lost on restart; fine for development and tests.

`PORT` sets the listen port (default `8080`).

## Local development without Docker

Prerequisite: Go 1.27+.

```bash
# In-memory store (no Postgres needed)
go run ./cmd/forge-hris-api

# Or against a local Postgres 16 (schema auto-applied on boot)
export DATABASE_URL=postgres://forge:forge@localhost:5432/forge_hris?sslmode=disable
export PORT=8080
go run ./cmd/forge-hris-api
```

Run the test suite:

```bash
go test ./... -race -count=1
# Postgres adapter tests (skip without TEST_DATABASE_URL):
TEST_DATABASE_URL=postgres://forge:forge@localhost:5432/forge_hris go test ./store/ -run TestPostgres -v
```

## First run: bootstrap an organization

The bootstrap route is the only unauthenticated mutating route. It creates
the organization, its owner employee, the owner membership, and a personal
access token. **The token plaintext is shown once** — save it; only its
SHA-256 hash is stored.

```bash
curl -s -X POST localhost:8080/api/v1/organizations \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Sdn Bhd","slug":"acme","country_code":"MY",
       "owner":{"first_name":"Azmi","last_name":"Muwahid","email":"azmi@acme.test"}}'
```

Use the token for every other request:

```bash
curl -s localhost:8080/api/v1/organizations/<org-id>/employees \
  -H "Authorization: Bearer <token>"
```

The full API contract is in [`docs/openapi.yaml`](docs/openapi.yaml).

## Production notes

- Put the API behind TLS (reverse proxy or load balancer); it serves plain HTTP.
- The in-process rate limiter is 100 req/min per credential, per replica.
- Row-level security policies exist in the schema, but the app connects as the
  table owner, which bypasses RLS in Postgres — tenant isolation today rests
  on the explicit `organization_id` predicates in every query. For a
  hardened deployment, run the app as a non-owner role with
  `FORCE ROW LEVEL SECURITY` on the tenant tables (note: the global token
  lookup in `GetCredentialByTokenHash` would then need a tenant context or an
  explicit policy exemption — test before enabling).
