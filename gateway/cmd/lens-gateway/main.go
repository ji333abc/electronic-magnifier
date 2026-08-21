package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"electronic-magnifier/gateway/internal/gateway"
)

func main() {
	configPath := flag.String("config", "/etc/lens-gateway/config.json", "path to JSON configuration")
	hashPassword := flag.String("hash-password", "", "print a bcrypt password hash and exit")
	flag.Parse()
	if *hashPassword != "" {
		hash, err := gateway.HashPassword(*hashPassword)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(hash)
		return
	}

	config, secrets, err := gateway.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	server, err := gateway.NewServer(config, secrets)
	if err != nil {
		log.Fatal(err)
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-interrupt
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("lens gateway listening on %s", config.Listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
