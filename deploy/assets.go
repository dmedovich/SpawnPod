// Package deploy embeds the reference security baseline in the CLI bundle.
package deploy

import _ "embed"

//go:embed security.yaml
var Security []byte
