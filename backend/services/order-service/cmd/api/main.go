package main

import "github.com/panda-dev/panda-v2/backend/services/order-service/internal/app"

func main() {
	if err := app.Run("order-service"); err != nil {
		panic(err)
	}
}
