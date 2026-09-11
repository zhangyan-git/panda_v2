# Merchant v1 contract

This directory is the versioned internal merchant contract. `merchant.proto` is the source contract; its generated bindings are committed alongside it.

## Compatibility

- Existing fields and field numbers are preserved. New fields are additive (`status_code`, audit fields, and ownership lookup RPCs).
- Existing clients may continue reading the string status fields. New clients may prefer the enum fields, but must tolerate an unspecified enum when talking to an older producer.
- `MerchantStatus` values mirror the existing merchant lifecycle: `pending`, `active`, and `suspended`.
- `ResourceStatus` and `AuditStatus` are independent for brands and stores. Operational status (`active`/`disabled`) must not be interpreted as audit status (`pending`/`approved`/`rejected`).
- Ownership lookup responses intentionally return only `merchant_id`; callers must not infer ownership from a brand or store's display data. Display names travel on `ResolveScopeNames`, which is a listing aid and never an authorization input.
- Do not reuse or renumber published fields. If a field is removed in a future version, reserve its number and name and publish a new version when wire semantics cannot remain compatible.

## Ownership transport

Ownership is resolved over gRPC, authenticated with the shared service token in `x-service-token` metadata:

| RPC | Result |
| --- | --- |
| `MerchantService.GetMerchant` | The merchant record; callers read `name` and `status` from it |
| `MerchantService.GetBrandMerchant` | Owning merchant ID |
| `MerchantService.GetStoreMerchant` | Owning merchant ID |
| `MerchantService.ResolveScopeNames` | Display names for a batch of brand and store IDs |

`GetMerchant` carries the merchant's display name and status, so it serves what the retired `/v1/merchant-service/merchants/{id}/name` and `/status` routes used to. Callers that only need a name or status must still send a merchant ID and accept the whole record — there are deliberately no single-field RPCs duplicating it.

`ResolveScopeNames` exists because the split moved `brands` and `stores` out of user-service's database, where they had been joined into a merchant account listing. It takes both ID lists at once so one page of accounts costs one round trip, and an ID that no longer exists is absent from the maps rather than an error: a deleted scope leaves its account listable with an empty scope name. Only a storage failure is an error, and it is opaque.

`ListMerchants`, `CreateMerchant`, `UpdateMerchant`, `ListStores`, `Profile`, and `Stores` remain unimplemented on the gRPC surface; the admin and merchant HTTP APIs are their surface. They exist in the proto as the eventual internal shape, and server implementations embed the generated `Unimplemented` struct so adding them later is not a breaking change.

A missing merchant, brand, or store is reported as `codes.NotFound`. This is a real contract, not an implementation detail: user-service's client translates `NotFound` back into a not-found domain error and treats every other status as a hard failure that aborts the operation. Do not widen `NotFound` to cover transport or internal errors.

Ownership timeouts are configured as 1000–30000 milliseconds (default 5000) through `MERCHANT_OWNERSHIP_TIMEOUT_MS`.

### Retired HTTP transport

The internal HTTP routes under `/v1/merchant-service/*` and their `X-Service-Token` middleware are removed. They were never forwarded by the browser gateway, and gRPC replaces them with a transport that authenticates every call rather than only the internal ones.

## Scope reset and deletion

`merchant-service` resets a brand's or store's account scope by calling user-service's `UserService.ResetAccountScope` over the same service-token channel; the `merchant_users` table belongs to user-service.

That call is synchronous and fail-closed. If user-service is unreachable or returns an error, the brand/store deletion fails with a retryable error and **no local delete happens**. It is deliberately not eventually consistent, and a remote scope reset followed by a local delete is deliberately not treated as atomic — the two are separate transactions in separate services, and the ordering is chosen so a partial failure leaves accounts pointing at a still-existing resource rather than at nothing.

Brand and store deletion also refuses when children still exist, and merchant deletion refuses while the merchant still has accounts (checked through user-service's `UserService.HasUsers`).

## Validation

```sh
cd contracts && buf generate
```

Generated code is committed; a diff after this command means the checked-in bindings are stale.
