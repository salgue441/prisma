// Command token mints HS256 JWTs for trying the examples:
//
//	go run ./examples/tools/token -sub alice -scope "orders:read orders:write"
//
// The secret defaults to $PRISMA_JWT_SECRET, as in the example configs.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	secret := flag.String("secret", os.Getenv("PRISMA_JWT_SECRET"), "HMAC secret (default $PRISMA_JWT_SECRET)")
	sub := flag.String("sub", "alice", "subject")
	scope := flag.String("scope", "", "space-separated scopes")
	iss := flag.String("iss", "https://auth.local/", "issuer")
	aud := flag.String("aud", "prisma-demo", "audience")
	ttl := flag.Duration("ttl", time.Hour, "lifetime (negative for an expired token)")
	flag.Parse()
	if *secret == "" {
		fmt.Fprintln(os.Stderr, "token: set -secret or PRISMA_JWT_SECRET")
		os.Exit(2)
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": *sub, "iss": *iss, "aud": *aud,
		"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(*ttl).Unix(),
	}
	if *scope != "" {
		claims["scope"] = *scope
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(*secret))
	if err != nil {
		fmt.Fprintln(os.Stderr, "token:", err)
		os.Exit(1)
	}
	fmt.Println(tok)
}
