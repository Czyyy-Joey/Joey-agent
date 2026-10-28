package main

import (
	"fmt"
	"net/http"
)

// TODO: read the port from a flag
func main() {
	port := 8080
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "hello") })
	fmt.Println("listening on", port)
	_ = http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}
