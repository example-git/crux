package main

import (
	"log"
	"net/http"
	"time"

	"github.com/example-git/crux/internal/ui/demo"
)

func main() {
	handler, err := demo.NewHandler()
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: "127.0.0.1:8768", Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("Real Crux UI preview listening on http://%s", server.Addr)
	log.Fatal(server.ListenAndServe())
}
