# ScarfBench source variant — 0x45 (Paste69)

- Repo: https://github.com/watzon/0x45.git
- Commit: `70a9c3980980b3a8c7934e91ebd4454f0e793ea0` ("change postgres port")
- Language row: Go (`benchmark-go/`) · Layer: `whole_applications` · License: MIT
- f_s: **Fiber 2.52.5** → f_t candidates: gin / echo / chi
- Toolchain: go 1.23.3 required; verified with go1.27.0 darwin/arm64

## Verified baseline (all captured 2026-08-21, before any change)

Host: `go build -o 0x45-bin .` → OK (16s). `./0x45-bin` → boots on :3000, 116 handlers, banner "Fiber v2.52.5".
Container: `docker build -f Dockerfile.scarfbench -t 0x45-source:70a9c39 .` → OK; runs, no external services.

| Request | Status | Content-Type |
|---|---|---|
| `GET /` `/stats` `/docs` `/submit` | 200 | text/html; charset=utf-8 |
| `POST /p/` (multipart `file=@s.txt`) | 200 | JSON: `id`,`filename`,`url`,`delete_url`,`mime_type`,`size`,`expires_at`,`private` |
| `POST /p/` (raw body, no multipart) | 400 | `{"error":"Invalid request body"}` |
| `GET /p/:id` `/p/:id.txt` `/p/:id/raw` | 200 | text/plain; charset=utf-8 (body = uploaded bytes) |
| `GET /p/:id/download` | 200 | application/octet-stream |
| `GET /p/:id/image` | 200 | image/png |
| `GET /p/zzzzzzzz` | 404 | application/json |
| `DELETE /p/:id/:key` | 200 | — |
| `GET /p/:id` after delete | 404 | application/json |
| `POST /u/` (shorten, no key) | 401 | `{"error":"API key required"}` |

52 routes total across paste, shortener, stats, docs pages.

## Environment decisions (identical for source and target containers)

Defaults already need **no external services**: SQLite (`paste69.db`), local disk storage, Redis off, SMTP off.
`.dockerignore` excludes `config.yaml`, so the container runs on viper defaults + these pinned env vars:

`0X_SERVER_BASE_URL=http://localhost:3000`, `0X_DATABASE_DRIVER=sqlite`, `0X_DATABASE_NAME=/app/data/paste69.db`,
`0X_REDIS_ENABLED=false`, `0X_SMTP_ENABLED=false`, rate limiting (global + per-IP) **disabled**.

Rate limiting is off on purpose: the default per-IP limit is 2 req/s burst 5, which would make smoke tests
flaky. Verified 20 rapid requests → all 200.

AWS S3 is a *dependency* but not required: `storage.type: local` is the default and no route needs S3.

## Not done here

No migration performed. No target-framework code, no `golden.patch`.
Remaining before a run: Gherkin oracle → `smoke.py`, and the agent task prompt.
