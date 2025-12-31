# Callback Listener

A lightweight Beeceptor-style webhook listener built with Go and Gin. It receives API calls under `/hook/*`, stores the full request to MariaDB, and lets you configure response rules from a built-in UI.

## Quick start

1. Set up a MariaDB database and user.
2. Copy `config.yml.example` to `config.yml`, then update the values:

```bash
cp config.yml.example config.yml
```

```yaml
addr: ":8080"
db_dsn: "user:pass@tcp(127.0.0.1:3306)/callback_listener?parseTime=true"
default_status: 200
default_body: '{"success": true, "message": "received"}'
```

3. Run the server:

```bash
go run ./
```

4. Open the UI:

- Requests list: `http://localhost:8080/`
- Rules editor: `http://localhost:8080/rules`
- Settings: `http://localhost:8080/settings`

## Docker

Build and run the container:

```bash
docker build -t callback-listener .
docker run --rm -p 8080:8080 -v "$(pwd)/config.yml:/app/config.yml:ro" callback-listener
```

Or use compose:

```bash
docker compose up --build
```

## Send a test request

```bash
curl -X POST http://localhost:8080/hook/orders \
  -H 'Content-Type: application/json' \
  -d '{"order_id": 123, "total": 48.5}'
```

## Configuration (config.yml)

- `addr`: bind address (default `:8080`)
- `db_dsn`: MariaDB DSN, for example `user:pass@tcp(127.0.0.1:3306)/callback_listener?parseTime=true`
- `default_status`: fallback status code when a rule omits one (default `200`)
- `default_body`: fallback response body (default `{"success": true, "message": "received"}`)

## Requests view

- Filter requests by path using the UI filter (or `/?path=/orders`).
- Live view updates the table in-place without a full page refresh.

## Settings

- The Settings page lets you delete stored requests by path.
- Paths are grouped from existing stored requests.

## Rules behavior

- Rules match exactly on `method + path`.
- Paths are relative to `/hook`. A rule path of `/orders` matches `POST /hook/orders`.
- If a rule omits status or body, the defaults are used.
- Response headers are a JSON object, for example:

```json
{
  "Content-Type": "application/json",
  "X-Source": "callback-listener"
}
```
