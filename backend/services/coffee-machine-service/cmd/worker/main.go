package main

import "github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/app"

func main() {
	if err := app.RunWorker("coffee-machine-service"); err != nil {
		panic(err)
	}
}
