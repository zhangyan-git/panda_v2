# Merchant v1 contract

This directory is the versioned internal merchant contract. `merchant.proto` is the source contract; generated code is intentionally not checked in.

## Compatibility

- Existing fields and field numbers are preserved. New fields are additive (`status_code`, audit fields, and ownership lookup RPCs).
- Existing clients may continue reading the string status fields. New clients may prefer the enum fields, but must tolerate an unspecified enum when talking to an older producer.
- `MerchantStatus` values mirror the existing merchant lifecycle: `pending`, `active`, and `suspended`.
- `ResourceStatus` and `AuditStatus` are independent for brands and stores. Operational status (`active`/`disabled`) must not be interpreted as audit status (`pending`/`approved`/`rejected`).
- Ownership lookup responses intentionally return only `merchant_id`; callers must not infer ownership from a brand or store's display data.
- Do not reuse or renumber published fields. If a field is removed in a future version, reserve its number and name and publish a new version when wire semantics cannot remain compatible.

## Validation

From the repository root, run:

```sh
protoc --proto_path=. --descriptor_set_out=/tmp/merchant-v1.pb contracts/proto/merchant/v1/merchant.proto
```

This validates syntax and imports without generating language bindings.
