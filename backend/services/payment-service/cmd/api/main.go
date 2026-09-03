package main

import "github.com/panda-dev/panda-v2/backend/services/payment-service/internal/app"

func main() {
	if err := app.Run("payment-service"); err != nil {
		panic(err)
	}
}
