// healthcheck — бинарник для HEALTHCHECK образа: GET /healthz на PORT.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8360"
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "healthz:", resp.Status)
		os.Exit(1)
	}
}
