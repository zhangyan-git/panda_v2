module github.com/panda-dev/panda-v2/backend/services/gateway-service

go 1.25.0

require github.com/panda-dev/panda-v2/backend v0.0.0

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/redis/go-redis/v9 v9.16.0 // indirect
)

replace github.com/panda-dev/panda-v2/backend => ../../
