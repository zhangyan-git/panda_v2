package main

import "github.com/panda-dev/panda-v2/backend/services/partner-service/internal/app"

func main() {
	if err := app.Run("partner-service"); err != nil {
		panic(err)
	}
}
