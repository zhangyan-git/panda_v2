# Panda V2 backend

## Local configuration

本地开发可以在项目根目录创建 `.env` 文件（该文件已被 Git 忽略），后端启动时会自动读取。可从模板开始：

```bash
cp deploy/config/.env.example .env
```

修改其中的本地 PostgreSQL 参数后，直接运行后端服务即可。已有环境变量优先于 `.env` 文件中的值，因此 CI、容器和生产环境仍可通过环境变量配置；不要把真实密码写入 `.env.example` 或提交到 Git。

## Service registry

Services register with etcd when `REGISTRY_ENDPOINT` is set. The value accepts a comma-separated list of etcd endpoints, for example:

```bash
REGISTRY_ENDPOINT=http://127.0.0.1:2379
```

Registration metadata is stored under `/panda/services/<service>/<instance-id>` with a 30-second lease that is renewed automatically. Only service identity and deployment metadata are written; credentials are not stored in the registry. During graceful shutdown, the renewal loop is stopped, the registration key is removed, and the lease is revoked.

When `REGISTRY_ENDPOINT` is empty, the backend uses an in-process no-op registry so services can run locally without etcd.

## Messaging

`platform/messaging` provides `Publisher`, `Consumer`, and generic `Outbox`/`Inbox` interfaces. `NewRabbitMQ(RabbitConfigFromEnv())` reads `RABBITMQ_URL`, `RABBITMQ_EXCHANGE` (default `panda.events`), `RABBITMQ_EXCHANGE_TYPE` (default `topic`), `RABBITMQ_QUEUE`, `RABBITMQ_ROUTING_KEY`, `RABBITMQ_DLX` (default `panda.events.dlx`), `RABBITMQ_DLQ` (default `panda.events.dlq`), and `RABBITMQ_RETRY_LIMIT` (default 5; set `0` to disable retries). When `RABBITMQ_URL` is unset it returns a no-op adapter, allowing local services and tests to run without RabbitMQ. Consumers use manual acknowledgements: successful handlers are acked, failures are retried up to the configured limit and then rejected for broker dead-letter processing.

Three properties make the adapter safe to leave running unattended:

- **Messages are published persistent.** The exchange and queues are durable, but a message published without `DeliveryMode: Persistent` is not — a broker restart discards it, and the data volume cannot help. Both the first publish and each retry set it.
- **A lost connection reconnects itself.** `NotifyClose` drives a supervisor that re-dials with exponential backoff (500ms up to 30s) and re-declares the topology. Publishing fails fast while a generation is down — the outbox relay retries the row — and the consumer resubscribes on the new connection instead of returning, which previously left messages accumulating in the queue with `/readyz` still reporting 200.
- **Failed messages dead-letter instead of disappearing.** With the default retry limit, a poison message is retried and then routed to the DLX/DLQ, where it can be inspected and replayed.

Queue arguments are part of a queue's identity: adding the dead-letter topology to a queue that already exists without it is rejected by the broker (406 PRECONDITION_FAILED). On an existing broker, delete the queue first or apply the arguments with a policy.

Integration tests for the reconnect and persistence paths are gated on `TEST_RABBITMQ_URL` and skip when it is unset.

`MemoryOutboxInbox` is a non-durable implementation intended only for tests and development. Production services can implement the interfaces using their own SQL transaction and outbox/inbox tables; no business tables are created by this package.

## Observability

`platform/observability` creates JSON structured `log/slog` output and filters sensitive key/value fields such as tokens, passwords, cookies, and authorization headers. Include `request_id` and `trace_id` as structured attributes in log calls. `Init` configures OpenTelemetry tracer and meter providers when `OTEL_EXPORTER_OTLP_ENDPOINT` is set; otherwise global SDK no-op providers are retained and no network activity occurs. `SERVICE_NAME` controls service identity. The collector pipeline in `deploy/observability/otel-collector.yaml` sends logs to OpenSearch, traces to Tempo, and metrics to Prometheus.

## PostgreSQL and Redis adapters

Platform infrastructure exposes lifecycle-only adapters with explicit `Ping` and `Close` methods. `platform/database.NewFromEnv` reads `DATABASE_URL` and creates a pgx v5 pool; `platform/cache.NewFromEnv` reads `REDIS_ADDR`, `REDIS_PASSWORD`, and optional `REDIS_DB` (the programmatic constructor accepts the database number). When `DATABASE_URL` or `REDIS_ADDR` is empty, each constructor returns a no-op implementation, so services start without external dependencies. These constructors do not perform implicit network calls; call `Ping` during startup when the dependency is configured, and always call `Close` during shutdown.

Each service instance caps its pool at `DB_POOL_MAX_CONNS` (default 10), falling back to `pool_max_conns` in the DSN if the variable is blank. The default is pinned rather than left to pgx's `max(4, NumCPU)` so the connection budget is arithmetic: `DB_POOL_MAX_CONNS × services × replicas` against Postgres' `max_connections`. An unparseable or non-positive value is an error rather than a silent fallback — a pool nobody chose only misbehaves under load, when it is hardest to attribute.

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
