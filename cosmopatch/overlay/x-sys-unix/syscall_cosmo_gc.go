// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cosmo

package unix

import (
	"syscall"
	"unsafe"
)

// The gosmopolitan fork's standard "syscall" package already implements
// these natively for cosmo (src/syscall/syscall_cosmo.go), unlike the
// assembly trampolines golang.org/x/sys/unix keeps for other platforms
// (syscall_unix_gc.go). Delegating avoids needing cosmo assembly here too.
func Syscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno) {
	return syscall.Syscall(trap, a1, a2, a3)
}

func Syscall6(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno) {
	return syscall.Syscall6(trap, a1, a2, a3, a4, a5, a6)
}

func RawSyscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno) {
	return syscall.RawSyscall(trap, a1, a2, a3)
}

func RawSyscall6(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno) {
	return syscall.RawSyscall6(trap, a1, a2, a3, a4, a5, a6)
}

// The *NoError variants are only a fast path on other platforms (raw
// assembly that skips errno conversion, for syscalls known not to fail).
// Dropping the error return from the checked call is observably identical.
func SyscallNoError(trap, a1, a2, a3 uintptr) (r1, r2 uintptr) {
	r1, r2, _ = syscall.Syscall(trap, a1, a2, a3)
	return r1, r2
}

func RawSyscallNoError(trap, a1, a2, a3 uintptr) (r1, r2 uintptr) {
	r1, r2, _ = syscall.RawSyscall(trap, a1, a2, a3)
	return r1, r2
}

// syscall_linux_amd64.go's Gettimeofday wants this -- same asm-backed shape
// as Syscall/Syscall6 above (syscall_linux_amd64_gc.go). unix.Timeval and
// syscall.Timeval share the same {Sec, Usec int64} layout on amd64, so the
// pointer cast is safe; the stdlib's cosmo Gettimeofday always returns a
// syscall.Errno (never another error type) when it returns non-nil.
func gettimeofday(tv *Timeval) (err syscall.Errno) {
	e := syscall.Gettimeofday((*syscall.Timeval)(unsafe.Pointer(tv)))
	if e == nil {
		return 0
	}
	return e.(syscall.Errno)
}
