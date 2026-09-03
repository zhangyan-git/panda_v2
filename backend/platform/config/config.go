package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	ServiceName, Version, Environment string
	HTTPAddress, GRPCAddress          string
	RegistryEndpoint                  string
	DatabaseURL, RedisAddress         string
	RedisPassword                     string
	RedisDB                           int
}

func Load(service string) (Config, error) {
	env := os.Getenv("PANDA_ENV")
	if env == "" {
		env = "dev"
	}
	version := os.Getenv("SERVICE_VERSION")
	if version == "" {
		version = "0.1.0"
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	grpcAddr := os.Getenv("GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":9090"
	}
	registryEndpoint := os.Getenv("REGISTRY_ENDPOINT")
	redisDB, err := parseRedisDB(os.Getenv("REDIS_DB"))
	if err != nil {
		return Config{}, err
	}
	return Config{ServiceName: service, Version: version, Environment: env, HTTPAddress: addr, GRPCAddress: grpcAddr, RegistryEndpoint: registryEndpoint, DatabaseURL: os.Getenv("DATABASE_URL"), RedisAddress: os.Getenv("REDIS_ADDR"), RedisPassword: os.Getenv("REDIS_PASSWORD"), RedisDB: redisDB}, nil
}

func parseRedisDB(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	db, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid REDIS_DB %q: %w", value, err)
	}
	if db < 0 {
		return 0, fmt.Errorf("REDIS_DB must not be negative: %d", db)
	}
	return db, nil
}
