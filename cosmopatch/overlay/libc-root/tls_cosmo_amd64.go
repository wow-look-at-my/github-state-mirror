// Copyright 2025 The Libc Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cosmo

package libc // import "modernc.org/libc"

// tls_linux_amd64.go declares these as //go:noescape forward references to
// assembly trampolines (tls_linux_amd64.s) that just call the lowercase
// tlsAlloc/tlsFree/tlsAllocaEntry/tlsAllocaExit helpers with the Go calling
// convention. Cosmo has no matching .s file, and the trampolines carry no
// OS- or architecture-specific logic -- they only forward to a method on
// *TLS -- so this calls those methods directly instead.
func TLSAlloc(tls *TLS, n int) uintptr {
	return tls.Alloc(n)
}

func TLSFree(tls *TLS, n int) {
	tls.Free(n)
}

func TLSAllocaEntry(tls *TLS) {
	tls.AllocaEntry()
}

func TLSAllocaExit(tls *TLS) {
	tls.AllocaExit()
}
