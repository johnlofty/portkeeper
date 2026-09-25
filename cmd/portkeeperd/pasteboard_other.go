//go:build !darwin

package main

// noPasteboard lets the daemon build and its tests run off a Mac. It never holds an
// image, so the clipboard socket answers every request with "no image".
type noPasteboard struct{}

func (noPasteboard) changeCount() int64        { return 0 }
func (noPasteboard) imagePNG() ([]byte, int64) { return nil, 0 }

func systemPasteboard() pasteboard { return noPasteboard{} }
