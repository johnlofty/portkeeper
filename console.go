// Package localgateway embeds the web console.
//
// It lives at the repo root only because go:embed cannot reach outside its own
// package directory, and the daemon lives under cmd/portkeeperd.
package localgateway

import _ "embed"

//go:embed web/index.html
var Console []byte
