// Package client embeds the standalone browser client in the Go server.
package client

import _ "embed"

//go:embed index.html
var HTML []byte
