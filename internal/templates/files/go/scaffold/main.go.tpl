package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := "127.0.0.1:" + port()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Go app served by xdev. Replace main.go with your app.")
	})
	log.Println("listening on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// port returns the host port xdev allocated for this app.
func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}
