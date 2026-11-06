package main

import (
	"log"

	"github.com/Cadoranee/Doverka/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	log.Print(cfg.HTTPAddr)
}
