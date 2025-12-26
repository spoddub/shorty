package config

import (
	"github.com/joho/godotenv"
	"log"
	"os"
)

type Config struct {
	Port        string
	DatabaseUrl string
	BaseUrl     string
}

func Load() Config {
	_ = godotenv.Load()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Println("Database URL is empty")
	}
	baseURL := os.Getenv("BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost" + port
	}

	return Config{
		Port:        port,
		DatabaseUrl: dbURL,
		BaseUrl:     baseURL,
	}
}
