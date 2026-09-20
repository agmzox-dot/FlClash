//go:build !android

package main

import "github.com/metacubex/mihomo/config"

func adaptiveLoadConfig(buf []byte, fallback func([]byte) (*config.Config, error)) (*config.Config, error) {
	return fallback(buf)
}

