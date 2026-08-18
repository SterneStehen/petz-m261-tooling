// Package webui exposes the pre-built console assets to the simulator binary.
package webui

import "embed"

// Assets is the production frontend bundle produced by `make webui`.
//
//go:embed dist/* dist/assets/*
var Assets embed.FS
