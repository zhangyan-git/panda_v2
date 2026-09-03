package main

import "github.com/panda-dev/panda-v2/backend/services/inventory-service/internal/app"

func main() {
	if err := app.Run("inventory-service"); err != nil {
		panic(err)
	}
}
