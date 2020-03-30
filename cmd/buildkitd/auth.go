package main

import (
	"github.com/moby/buildkit/auth"
)

var authorizer auth.Authorizer

func init() {
	authorizer = auth.NoopAuthorizer{}
}

func setupAuthorizer(endpoint string) {
	authorizer = auth.NewService(endpoint)
}
