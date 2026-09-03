# Panda V2

Panda V2 platform skeleton. This repository contains service and platform contracts only; domain workflows are intentionally not implemented.

## Services

user, merchant, membership, coupon, account, lottery, order, payment, settlement, fulfillment, inventory, partner. `gateway` is the platform entrypoint.

## Local checks

```bash
cd backend && go test ./... && go build ./...
```

Development infrastructure is defined in `deploy/compose/dev`. No secrets belong in Git.
