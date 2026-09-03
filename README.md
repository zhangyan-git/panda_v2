# Panda V2

Panda V2 platform skeleton. This repository contains service and platform contracts only; domain workflows are intentionally not implemented.

## Services

user, merchant, membership, coupon, account, lottery, order, payment, settlement, fulfillment, inventory, partner. `gateway` is the platform entrypoint.

## Contracts

`contracts/proto` is the source of versioned protobuf contracts. The `user/v1`, `account/v1`, and `gateway/v1` packages define the initial profile, login, and routing RPC messages. Generated code is intentionally not checked in; this checkout has `protoc` for schema validation but no protobuf code generators.

## Local checks

```bash
cd backend && go test ./... && go build ./...
protoc --descriptor_set_out=/tmp/panda-v2-protos.pb --include_imports \\
  contracts/proto/user/v1/user.proto \\
  contracts/proto/account/v1/account.proto \\
  contracts/proto/gateway/v1/gateway.proto
```

Development infrastructure is defined in `deploy/compose/dev`. No secrets belong in Git.
