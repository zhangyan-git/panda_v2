package main

import "github.com/panda-dev/panda-v2/backend/services/gateway-service/internal/app"

func main() {
	if err := app.Run("gateway-service"); err != nil {
		panic(err)
	}
}
