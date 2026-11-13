//go:build !(windows && embedffmpeg)

package main

import "io"

func openEmbeddedFFmpeg() (io.ReadCloser, int64, error) { return nil, 0, nil }
