// Command hmac-sign sends an HMAC-signed request, signed exactly the way the
// gateway verifies it (authn.Sign):
//
//	go run ./examples/tools/hmac-sign -key partner-key -secret "$PRISMA_HMAC_SECRET" \
//	    -X POST -d '{"item":"book"}' http://localhost:8080/api/orders
//
// Run it twice with -replay to see the gateway reject a reused signature.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"time"

	"prisma/internal/filters/authn"
)

func main() {
	keyID := flag.String("key", "partner-key", "signing key ID (consumers[].hmacKeys)")
	secret := flag.String("secret", os.Getenv("PRISMA_HMAC_SECRET"), "shared secret (default $PRISMA_HMAC_SECRET)")
	method := flag.String("X", http.MethodGet, "HTTP method")
	data := flag.String("d", "", "request body")
	replay := flag.Bool("replay", false, "send the same signed request twice")
	flag.Parse()
	if flag.NArg() != 1 || *secret == "" {
		fmt.Fprintln(os.Stderr, "usage: hmac-sign -key ID -secret S [-X METHOD] [-d BODY] URL")
		os.Exit(2)
	}

	body := []byte(*data)
	req, err := http.NewRequest(*method, flag.Arg(0), bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	ts := time.Now().Unix()
	req.Header.Set(authn.HeaderSignatureKey, *keyID)
	req.Header.Set(authn.HeaderSignatureTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(authn.HeaderSignature, authn.Sign([]byte(*secret), req.Method, req.URL.RequestURI(), req.Host, ts, body, req.Header, nil))

	sends := 1
	if *replay {
		sends = 2
	}
	for i := range sends {
		req.Body = io.NopCloser(bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		dump, _ := httputil.DumpResponse(resp, true)
		resp.Body.Close()
		fmt.Printf("--- attempt %d ---\n%s\n", i+1, dump)
	}
}
