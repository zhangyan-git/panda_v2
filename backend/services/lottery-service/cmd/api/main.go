package main

import "github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/app"

func main() {
	if err := app.Run("lottery-service"); err != nil {
		panic(err)
	}
}
