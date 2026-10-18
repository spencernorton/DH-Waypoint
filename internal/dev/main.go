package main

import (
	_ "github.com/joho/godotenv/autoload"
	"github.com/spencernorton/DH-Waypoint/internal/dev/app"
)

func main() {
	app.New().Run()
}
