package main

import (
	"log"
	"net/http"
)

func main() {
	srv := &http.Server{
		Addr:    ":8888",
		Handler: router(),
	}

	log.Println("listening on: ", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}
