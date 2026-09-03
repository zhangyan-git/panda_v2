package main

import "github.com/panda-dev/panda-v2/backend/services/membership-service/internal/app"

func main() {
	if err := app.Run("membership-service"); err != nil {
		panic(err)
	}
}
