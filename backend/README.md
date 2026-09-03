# Panda V2 backend

## Service registry

Services register with etcd when `REGISTRY_ENDPOINT` is set. The value accepts a comma-separated list of etcd endpoints, for example:

```bash
REGISTRY_ENDPOINT=http://127.0.0.1:2379
```

Registration metadata is stored under `/panda/services/<service>/<instance-id>` with a 30-second lease that is renewed automatically. Only service identity and deployment metadata are written; credentials are not stored in the registry. During graceful shutdown, the renewal loop is stopped, the registration key is removed, and the lease is revoked.

When `REGISTRY_ENDPOINT` is empty, the backend uses an in-process no-op registry so services can run locally without etcd.

## Messaging

`platform/messaging` provides `Publisher`, `Consumer`, and generic `Outbox`/`Inbox` interfaces. `NewRabbitMQ(RabbitConfigFromEnv())` reads `RABBITMQ_URL`, `RABBITMQ_EXCHANGE` (default `panda.events`), `RABBITMQ_EXCHANGE_TYPE` (default `topic`), `RABBITMQ_QUEUE`, `RABBITMQ_ROUTING_KEY`, `RABBITMQ_DLX`, `RABBITMQ_DLQ`, and `RABBITMQ_RETRY_LIMIT`. When `RABBITMQ_URL` is unset it returns a no-op adapter, allowing local services and tests to run without RabbitMQ. Consumers use manual acknowledgements: successful handlers are acked, failures are retried up to the configured limit and then rejected for broker dead-letter processing.

`MemoryOutboxInbox` is a non-durable implementation intended only for tests and development. Production services can implement the interfaces using their own SQL transaction and outbox/inbox tables; no business tables are created by this package.

## Observability

`platform/observability` creates JSON structured `log/slog` output and filters sensitive key/value fields such as tokens, passwords, cookies, and authorization headers. Include `request_id` and `trace_id` as structured attributes in log calls. `Init` configures OpenTelemetry tracer and meter providers when `OTEL_EXPORTER_OTLP_ENDPOINT` is set; otherwise global SDK no-op providers are retained and no network activity occurs. `SERVICE_NAME` controls service identity. The collector pipeline in `deploy/observability/otel-collector.yaml` sends logs to OpenSearch, traces to Tempo, and metrics to Prometheus.

## PostgreSQL and Redis adapters

Platform infrastructure exposes lifecycle-only adapters with explicit `Ping` and `Close` methods. `platform/database.NewFromEnv` reads `DATABASE_URL` and creates a pgx v5 pool; `platform/cache.NewFromEnv` reads `REDIS_ADDR`, `REDIS_PASSWORD`, and optional `REDIS_DB` (the programmatic constructor accepts the database number). When `DATABASE_URL` or `REDIS_ADDR` is empty, each constructor returns a no-op implementation, so services start without external dependencies. These constructors do not perform implicit network calls; call `Ping` during startup when the dependency is configured, and always call `Close` during shutdown.

## Discovery

The etcd adapter implements `Resolver` and `Watcher`, reading JSON instances from `/panda/services/<service>/` and emitting initial `put` events followed by `put` and `delete` changes. For Kratos integration, wrap it with `KratosRegistrar` and `KratosDiscovery`:

```go
adapter := registry.New(REGISTRY_ENDPOINT)
registrar := registry.KratosRegistrar{Registry: adapter}
discovery := registry.KratosDiscovery{Resolver: adapter.(registry.Resolver), Watcher: adapter.(registry.Watcher)}
_ = registrar
_ = discovery
```

For gRPC clients using Kratos' discovery resolver, use `platform/client.DialDiscovery` (see `examples/grpc-client`) or register the standard Kratos discovery builder yourself, then use `discovery:///service-name` as the target. The adapter tracks instance snapshots across watch events and returns cancellation or etcd errors to callers.
