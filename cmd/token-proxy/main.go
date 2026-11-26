package main

import (
	"flag"
	"fmt"
	"github.com/tym83/aep/internal/tokenproxy"
	"net/http"
	"os"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8081", "Local listen address")
	flag.Parse()
	p, err := tokenproxy.New(tokenproxy.Config{TokenURL: os.Getenv("OAUTH_TOKEN_URL"), MCPURL: os.Getenv("MCP_URL"), ClientID: os.Getenv("OAUTH_CLIENT_ID"), ClientSecret: os.Getenv("OAUTH_CLIENT_SECRET"), Scope: os.Getenv("OAUTH_SCOPE")})
	if err == nil {
		s := &http.Server{Addr: *listen, Handler: p, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
		err = s.ListenAndServe()
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
