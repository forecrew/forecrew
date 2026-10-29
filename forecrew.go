// Package forecrew embeds the files the binaries ship with: the role packs
// and the contract schemas they refer to.
package forecrew

import "embed"

// Files holds packs/ and docs/contracts/*.json, with paths relative to the
// repo root, e.g. packs/roles/developer.yaml.
//
//go:embed packs docs/contracts/*.json
var Files embed.FS
