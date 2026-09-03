package main

import "github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/app"

func main() {
	if err := app.Run("merchant-service"); err != nil {
		panic(err)
	}
}
