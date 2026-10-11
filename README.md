# simple-photos

A small, self-hosted photo gallery and uploader. Photos are stored in S3, and their metadata lives in Postgres. Sign-in is with Google.

- **Google sign-in.** You can limit access to specific emails or domains.
- **Multi-select uploads.** Pick several files in the file dialog or drag and drop them. Uploads run three at a time, each with its own progress bar, and a failed file doesn't stop the rest.
- **10 MB per photo.** The browser checks the size first, and the server enforces the limit. The server also identifies the image type from the file's contents (JPEG, PNG, GIF, WebP, HEIC, AVIF) and ignores the type the client claims.
- **Photos expire after 30 days.** An expired photo disappears from the gallery right away. A background job deletes its S3 objects and database row every 15 minutes. An optional S3 lifecycle rule acts as a backstop.
- **Grid gallery view.** The gallery is a square-cropped, lazy-loaded grid with paging through older photos. The browser makes a 480px thumbnail at upload time, so the server does no image processing.
- **Light on resources.** It's one static Go binary with templates and assets built in, and it used under 50 MB of RAM in testing. The S3 bucket stays private, and images are served through short-lived presigned URLs, so photo traffic doesn't pass through your server.

## Why Postgres for metadata

You already run Postgres, so storing metadata there costs nothing extra. The app needs three small tables (`users`, `sessions`, `photos`), and they're created automatically at startup.

## Setup

### 0. DNS
Point your domain (and optionally `www.<domain>`) A/AAAA records at your server. With Docker Compose, also add `s3.<domain>`. Ports 80 and 443 must be reachable so Caddy can get a certificate.

### 1. Google OAuth client
In [Google Cloud Console](https://console.cloud.google.com/), create or select a project, then:
1. **APIs & Services → OAuth consent screen**: choose **External**. Set the app name, support email and your domain as an authorized domain. The only scopes needed are `openid`, `email` and `profile`, which Google treats as non-sensitive, so no verification review is required. While the app is in **Testing** status, only the test users you list can sign in; click **Publish app** to let anyone on your allowlist in.
2. **APIs & Services → Credentials → Create credentials → OAuth client ID → Web application**:
   - Authorized JavaScript origin: `https://<domain>`
   - Authorized redirect URI: `https://<domain>/auth/callback`
3. Put the client ID and secret in `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET`.

For local development, also add `http://localhost:8080/auth/callback` as a redirect URI and run with `BASE_URL=http://localhost:8080`.

### 2. Bucket
Create a **private** bucket. Any S3-compatible provider works:
- **Cloudflare R2** has no egress fees. Set `S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com` and `S3_REGION=auto`.
- **Backblaze B2** and **Wasabi** also work.

The access key needs `s3:PutObject`, `s3:GetObject` and `s3:DeleteObject` on the bucket.

Uploads go through the app, so the bucket doesn't need a CORS rule.

Optional backstop: add a lifecycle rule that expires objects after 31 days.
```sh
aws s3api put-bucket-lifecycle-configuration --bucket my-photos \
  --lifecycle-configuration file://deploy/s3-lifecycle.json
```
Alternatively, set `S3_MANAGE_LIFECYCLE=true` and the app installs the rule itself. That replaces any existing rules on the bucket, so only use it on a bucket dedicated to this app.

### 3. Database
```sh
sudo -u postgres createuser -P photos
sudo -u postgres createdb -O photos photos
```

### 4. Run
```sh
go build -o simple-photos .             # or: docker build -t simple-photos .
sudo install simple-photos /usr/local/bin/
sudo cp .env.example /etc/simple-photos.env && sudo chmod 600 /etc/simple-photos.env  # then fill it in
sudo cp deploy/simple-photos.service /etc/systemd/system/
sudo systemctl enable --now simple-photos
```
Put it behind a TLS reverse proxy. `deploy/Caddyfile` is a ready-made config; it reads the domain from the `DOMAIN` environment variable, so set that for Caddy (`sudo systemctl edit caddy`, then add `Environment=DOMAIN=<domain>` under `[Service]`). When `BASE_URL` starts with `https://`, the app sets cookies as `Secure`.

All settings are documented in `.env.example`.

## Running with Docker Compose (Postgres + MinIO)

`docker-compose.yml` runs Postgres, MinIO (a self-hosted S3), the app and Caddy on one box, so you don't need a cloud bucket. The domain is set once, as `DOMAIN` in `.env`: the site is served at `https://<domain>` and photos at `https://s3.<domain>`.

```sh
cp .env.compose.example .env      # set DOMAIN, passwords (openssl rand -hex 24) and Google creds
docker compose up -d --build
```

- A one-shot `minio-init` service creates the bucket. It also adds a 31-day expiry rule and an access key that can only touch that bucket. It's safe to re-run.
- Caddy listens publicly on ports 80 and 443 and gets HTTPS certificates automatically. Every other port is bound to `127.0.0.1`. The MinIO web console is at http://localhost:9001; sign in as `MINIO_ROOT_USER`.
- Browsers load photos directly from MinIO, so MinIO needs a public address. Caddy serves it at `s3.<domain>`, which is the default `S3_PUBLIC_ENDPOINT`.
- To try it locally, set `BASE_URL=http://localhost:8080` and `S3_PUBLIC_ENDPOINT=http://localhost:9000` in `.env`, and start everything except Caddy: `docker compose up -d --build postgres minio minio-init app`. Also add `http://localhost:8080/auth/callback` as a redirect URI in Google.
- To run the app binary under systemd but keep Postgres and MinIO in Docker, run `docker compose up -d postgres minio minio-init`. Then point `/etc/simple-photos.env` at `localhost:5432` and `http://localhost:9000`.
- Data lives in the `pgdata` and `miniodata` Docker volumes; Caddy's certificates are in `caddydata`.

## Development

```sh
go test ./...                                              # unit tests
TEST_DATABASE_URL=postgres://localhost/photos_test go test -p 1 ./...   # + integration tests
```
The integration tests use a real Postgres database and an in-memory store in place of S3.
