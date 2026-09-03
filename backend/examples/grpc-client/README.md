# gRPC discovery example

This example is a minimal real Kratos gRPC client. It registers the existing etcd-backed
`platform/registry` adapter with Kratos' discovery resolver and dials a service by name.
The target must use `discovery:///service-name`.

The platform gRPC server already exposes Kratos' standard gRPC health service. Start a
service with a registry and gRPC endpoint, then run the client:

```bash
REGISTRY_ENDPOINT=http://127.0.0.1:2379 \
SERVICE_NAME=gateway-service \
SERVICE_ENDPOINT=discovery:///gateway-service \
go run ./examples/grpc-client
```

The service server registers its actual gRPC listener endpoint (including the ephemeral
port when `GRPC_ADDR=:0`) under `/panda/services/<service>/`. The client creates a
Kratos `DialInsecure` connection with a five-second context/client timeout, invokes the
standard health check, and closes both the connection and registry adapter on exit.

With no `REGISTRY_ENDPOINT`, the adapter is a no-op and discovery has no instances; use
an etcd endpoint for this example. No business RPC or application workflow is included.
