package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/joho/godotenv"
	"github.com/os-playground-api/internal/config"
)

func main() {
	if err := godotenv.Load(); err != nil {
		slog.Warn("could not load .env file", "error", err)
	}

	cfg, err := config.Load(os.LookupEnv)
	if err == nil {
		err = cfg.Validate()
	}
	if err != nil {

		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		os.Exit(2)
	}
	slog.Info("config loaded", "config", cfg)
}
