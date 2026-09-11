# Contracts

Proto is the source contract. HTTP APIs use `/v1`; published fields and event versions are backward compatible.

Versioned internal contracts live under `proto/<domain>/v1`. Go consumers now exist, so the generated bindings are **committed** — `go build ./...` must work without `buf` or `protoc` installed. Each contract README documents compatibility alongside the generated code it describes.

## Layout

`contracts` is its own Go module (`github.com/panda-dev/panda-v2/contracts`), following the repository's per-directory module convention. A consuming service adds it with a `require` plus a `replace ... => ../../../contracts`, exactly as it does for `backend`.

`buf.yaml` declares `proto` as the buf module root, and `buf.gen.yaml` writes to `proto` with `paths=source_relative` — source-relative paths are relative to the buf module root, so generating into the source tree is what makes the `.../contracts/proto/<domain>/v1` import paths resolve. Each proto's `go_package` matches its directory.

## Regenerating

```sh
cd contracts && buf generate
```

CI re-runs this and fails on any resulting diff, which is what keeps the committed bindings honest. `clean` is deliberately off in `buf.gen.yaml`: it would delete the hand-written `README.md` files that sit next to the protos.

Generated `*_grpc.pb.go` files use `require_unimplemented_servers`; server implementations embed the `Unimplemented*Server` struct so that adding an RPC does not break existing implementers.

## Authentication of internal RPCs

Internal RPCs are authenticated at the gRPC layer (`platform/auth`), not by HTTP headers:

- Service-to-service calls present the shared service token in `x-service-token` metadata.
- Calls made on behalf of an end user present that user's own access token in `authorization` metadata. No RPC accepts a caller-supplied identity field — identity is only ever established from the presented credential.
- Presenting both credentials is rejected rather than resolved by precedence.
- `grpc.health.v1.Health` and server reflection are exempt, so probes keep working.
