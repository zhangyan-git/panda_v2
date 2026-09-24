# Container images

Two Dockerfiles cover five images: three Go services and two web apps.

Both take the **repository root as the build context**. This is a requirement,
not a convention — `backend/services/*/go.mod` reach their siblings through
relative `replace` directives (`../../` for backend, `../../../contracts` and
`../../../migrations`), and the web apps are pnpm workspace members that import
from `packages/*`. A smaller context cannot resolve either.

```bash
# Go services. CMD defaults to ./cmd, which is where every service keeps its main
# package; pass --build-arg CMD=./cmd/<tool> only for a module's second binary.
docker build -f deploy/docker/Dockerfile.backend --build-arg SERVICE=user-service     -t panda/user-service     .
docker build -f deploy/docker/Dockerfile.backend --build-arg SERVICE=merchant-service -t panda/merchant-service .
docker build -f deploy/docker/Dockerfile.backend --build-arg SERVICE=gateway-service  -t panda/gateway-service .

# Web apps.
docker build -f deploy/docker/Dockerfile.web --build-arg APP=admin-web    -t panda/admin-web    .
docker build -f deploy/docker/Dockerfile.web --build-arg APP=merchant-web -t panda/merchant-web .
```

`.dockerignore` at the repository root keeps host-built `node_modules` and
`dist` out of the context. That matters more than context size: dependencies
installed on a developer's machine are compiled for that machine, and a stale
`dist` copied into an image ships as the new build.

## Running the stack

The application services are behind a compose profile, so the default stays
what it was — middleware only, with the services running on the host:

```bash
cd deploy/compose/dev
docker compose up                        # nine middleware containers, as before
docker compose --profile app up --build  # everything, including the five images
```

Then `http://localhost:8000` (admin), `http://localhost:8001` (merchant), and
the gateway directly on `:8080`.

## The two numbers in nginx.conf that are not defaults

| Setting | Value | Why |
| --- | --- | --- |
| `client_max_body_size` | `12m` | nginx defaults to `1m` and enforces it *before* any Go timeout, so a 10MiB upload would be answered `413` by a component that appears nowhere in the API contract. 12m covers `UPLOAD_MAX_FILE_SIZE` (10MiB) plus multipart framing. |
| `proxy_read_timeout` | `180s` | Has to exceed `GATEWAY_UPLOAD_TIMEOUT_MS` (120s). Set it lower and nginx abandons uploads the gateway is still completing, turning working requests into 504s. |

The budgets layer, outer to inner: **nginx 180s > gateway 120s > merchant-service
90s > OSS SDK 60s**. Changing one means checking the ones inside it.

`nginx.conf` also forwards `X-Forwarded-For`, which the gateway ignores unless
`TRUSTED_PROXY_CIDRS` covers this proxy. Without that, every request is keyed on
nginx's own address and the whole site shares one rate-limit bucket.

## What has and has not been exercised

This checkout has no registry access, so the two families were verified to
different depths. Both need a connected machine to confirm fully.

- **Go images** — the builder stage was built for all three services, with the
  module cache bind-mounted in place of `go mod download`, and the resulting
  binaries were run against the dev stack: `/livez` and `/readyz` both
  answered, both listeners came up, and `ldd` reports "not a dynamic
  executable" (so a runtime with no libc is safe). What remains untested is the
  `golang:1.25` pull, the `go mod download` layer, and the runtime stage —
  no runtime base image was available locally to build it against. The base has
  since changed from distroless to `debian:bookworm-slim` (see
  `Dockerfile.backend`), and CI has not yet produced a green image, so treat the
  runtime stage as unverified until it has.
- **Web images** — not built at all. `pnpm install` needs the package registry
  and no nginx image was present locally, so neither the install layer nor
  `nginx.conf` has been executed. Run `--profile app up --build` once on a
  connected machine before trusting either.
