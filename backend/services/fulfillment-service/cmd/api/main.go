package main

import "github.com/panda-dev/panda-v2/backend/services/fulfillment-service/internal/app"

func main() {
	if err := app.Run("fulfillment-service"); err != nil {
		panic(err)
	}
}
