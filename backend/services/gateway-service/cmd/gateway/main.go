package main

import (
	"log"
	"net/http"
	"os"

	"github.com/panda-dev/panda-v2/backend/services/gateway-service/internal/proxy"
)

func main() {
	h, err := proxy.NewHandler(proxy.Config{
		MerchantServiceURL: requiredEnv("MERCHANT_SERVICE_URL"),
		UserServiceURL:     requiredEnv("USER_SERVICE_URL"),
	})
	if err != nil {
		log.Fatalf("gateway-service: configure proxy: %v", err)
	}
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("gateway-service: listening on %s", addr)
	if err := http.ListenAndServe(addr, h); err != nil {
		log.Fatalf("gateway-service: serve: %v", err)
	}
}

func requiredEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("gateway-service: %s is required", name)
	}
	return value
}
