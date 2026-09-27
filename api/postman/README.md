# Postman collection

A Postman collection with a request for every operation in [`openapi.yaml`](../openapi.yaml), for trying the API against a development platform.

| File | Contents |
| --- | --- |
| `pressfleet.postman_collection.json` | The requests, one folder per API area, plus a **Smoke test** folder |
| `pressfleet-local.postman_environment.json` | The **PressFleet local** environment: `base_url` and the variables the requests fill in |

## Setup

1. Start a platform with `pressfleet init` ([installation.md](../../docs/installation.md)). On a development machine, add `--skip-preflight` and `--control-plane-image` with an image from `make image`.
2. Point `base_url` at the API. Traefik serves it at `https://<dashboard domain>`, which works as is once the dashboard domain resolves to the host and has its certificate. On a development machine without DNS, publish the control plane's own port on the host instead: add a `compose.override.yaml` next to the rendered `compose.yaml` (`<data_dir>/platform/`), then recreate the project:

   ```yaml
   services:
     control-plane:
       ports:
         - "127.0.0.1:8080:8080"
   ```

   ```sh
   docker compose -p pressfleet -f /var/lib/pressfleet/platform/compose.yaml \
     -f /var/lib/pressfleet/platform/compose.override.yaml up -d
   ```

3. In Postman, import both files, select the **PressFleet local** environment, and set `admin_email` and `admin_password` to the admin that `init` created. Set `base_url` to `https://<dashboard domain>`, or leave it at `http://localhost:8080` if you published that port.
4. Send **Auth → Log in**.

Put credentials in the environment's current values, which stay on your machine. Don't export the environment over the committed file: the export includes whatever values it holds.

## How requests authenticate

A collection pre-request script authenticates every request:

- If `api_token` is set, it sends `Authorization: Bearer <api_token>`. The server then ignores the session cookie.
- Otherwise it relies on the session cookie from **Log in**, which Postman's cookie jar keeps, and adds the `X-CSRF-Token` header that unsafe methods need. **Log in** and **Get current user** save the token to `csrf_token`.

**API tokens → Create API token** saves the new token to `api_token`, so later requests use it. To go back to the session, clear `api_token` or revoke that token with **Revoke API token**.

## Variables

Requests that create something save its ID for the requests that follow: `client_group_id`, `site_id`, `user_id`, `token_id`. Every `202` response saves the queued job's ID to `job_id`.

Site changes run as jobs. Follow them with **Jobs → Get job**. When the collection runs in the Collection Runner or newman, **Get job** repeats every 2 seconds until the job finishes and fails if the job didn't succeed. Sent by hand, it polls once.

## Smoke test

The **Smoke test** folder runs a whole site lifecycle: log in, create a client group and a site, wait for provisioning, retrieve the admin credentials, purge the site's caches, then delete the site and the group. Run it in the Collection Runner, or with [newman](https://github.com/postmanlabs/newman):

```sh
npx newman run api/postman/pressfleet.postman_collection.json \
  -e api/postman/pressfleet-local.postman_environment.json \
  --folder "Smoke test" \
  --env-var admin_email=admin@agency.example --env-var admin_password="$ADMIN_PASSWORD"
```

It needs the site images, like any provisioning (see [images/README.md](../../images/README.md)).

## Keeping it current

The collection is maintained by hand. When an operation is added to or changed in `openapi.yaml`, update the matching request here. Postman can also import `openapi.yaml` directly to produce a collection with no scripts, which is useful for checking what's missing.
