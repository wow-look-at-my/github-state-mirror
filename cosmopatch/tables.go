//go:build cosmopatch_tool

package main

// libcCopies is every modernc.org/libc file the cosmo build needs that
// upstream never shipped a cosmo variant of. Each is an exact copy of an
// existing linux/amd64 or linux/arm64 file (verified content-identical
// apart from the build tag when this table was built) with its build
// tag forced to "cosmo" by addCosmoFile.
var libcCopies = []copySpec{
	{"errno/errno_linux_amd64.go", "errno/errno_cosmo_amd64.go", ""},
	{"errno/capi_linux_amd64.go", "errno/capi_cosmo_amd64.go", ""},
	{"errno/errno_linux_arm64.go", "errno/errno_cosmo_arm64.go", ""},
	{"errno/capi_linux_arm64.go", "errno/capi_cosmo_arm64.go", ""},
	{"grp/grp_linux_amd64.go", "grp/grp_cosmo_amd64.go", ""},
	{"grp/capi_linux_amd64.go", "grp/capi_cosmo_amd64.go", ""},
	{"grp/grp_linux_arm64.go", "grp/grp_cosmo_arm64.go", ""},
	{"grp/capi_linux_arm64.go", "grp/capi_cosmo_arm64.go", ""},
	{"limits/limits_linux_amd64.go", "limits/limits_cosmo_amd64.go", ""},
	{"limits/capi_linux_amd64.go", "limits/capi_cosmo_amd64.go", ""},
	{"limits/limits_linux_arm64.go", "limits/limits_cosmo_arm64.go", ""},
	{"limits/capi_linux_arm64.go", "limits/capi_cosmo_arm64.go", ""},
	{"poll/poll_linux_amd64.go", "poll/poll_cosmo_amd64.go", ""},
	{"poll/capi_linux_amd64.go", "poll/capi_cosmo_amd64.go", ""},
	{"poll/poll_linux_arm64.go", "poll/poll_cosmo_arm64.go", ""},
	{"poll/capi_linux_arm64.go", "poll/capi_cosmo_arm64.go", ""},
	{"pthread/pthread_linux_amd64.go", "pthread/pthread_cosmo_amd64.go", ""},
	{"pthread/capi_linux_amd64.go", "pthread/capi_cosmo_amd64.go", ""},
	{"pthread/pthread_linux_arm64.go", "pthread/pthread_cosmo_arm64.go", ""},
	{"pthread/capi_linux_arm64.go", "pthread/capi_cosmo_arm64.go", ""},
	{"pwd/pwd_linux_amd64.go", "pwd/pwd_cosmo_amd64.go", ""},
	{"pwd/capi_linux_amd64.go", "pwd/capi_cosmo_amd64.go", ""},
	{"pwd/pwd_linux_arm64.go", "pwd/pwd_cosmo_arm64.go", ""},
	{"pwd/capi_linux_arm64.go", "pwd/capi_cosmo_arm64.go", ""},
	{"signal/more_linux_amd64.go", "signal/more_cosmo_amd64.go", ""},
	{"signal/signal_linux_amd64.go", "signal/signal_cosmo_amd64.go", ""},
	{"signal/capi_linux_amd64.go", "signal/capi_cosmo_amd64.go", ""},
	{"signal/more_linux_arm64.go", "signal/more_cosmo_arm64.go", ""},
	{"signal/signal_linux_arm64.go", "signal/signal_cosmo_arm64.go", ""},
	{"signal/capi_linux_arm64.go", "signal/capi_cosmo_arm64.go", ""},
	{"stdio/stdio_linux_amd64.go", "stdio/stdio_cosmo_amd64.go", ""},
	{"stdio/capi_linux_amd64.go", "stdio/capi_cosmo_amd64.go", ""},
	{"stdio/stdio_linux_arm64.go", "stdio/stdio_cosmo_arm64.go", ""},
	{"stdio/capi_linux_arm64.go", "stdio/capi_cosmo_arm64.go", ""},
	{"stdlib/stdlib_linux_amd64.go", "stdlib/stdlib_cosmo_amd64.go", ""},
	{"stdlib/capi_linux_amd64.go", "stdlib/capi_cosmo_amd64.go", ""},
	{"stdlib/stdlib_linux_arm64.go", "stdlib/stdlib_cosmo_arm64.go", ""},
	{"stdlib/capi_linux_arm64.go", "stdlib/capi_cosmo_arm64.go", ""},
	{"sys/types/types_linux_amd64.go", "sys/types/types_cosmo_amd64.go", ""},
	{"sys/types/capi_linux_amd64.go", "sys/types/capi_cosmo_amd64.go", ""},
	{"sys/types/types_linux_arm64.go", "sys/types/types_cosmo_arm64.go", ""},
	{"sys/types/capi_linux_arm64.go", "sys/types/capi_cosmo_arm64.go", ""},
	{"time/time_linux_amd64.go", "time/time_cosmo_amd64.go", ""},
	{"time/capi_linux_amd64.go", "time/capi_cosmo_amd64.go", ""},
	{"time/time_linux_arm64.go", "time/time_cosmo_arm64.go", ""},
	{"time/capi_linux_arm64.go", "time/capi_cosmo_arm64.go", ""},
	{"unistd/unistd_linux_amd64.go", "unistd/unistd_cosmo_amd64.go", ""},
	{"unistd/capi_linux_amd64.go", "unistd/capi_cosmo_amd64.go", ""},
	{"unistd/unistd_linux_arm64.go", "unistd/unistd_cosmo_arm64.go", ""},
	{"unistd/capi_linux_arm64.go", "unistd/capi_cosmo_arm64.go", ""},
	{"uuid/uuid/uuid_linux_amd64.go", "uuid/uuid/uuid_cosmo_amd64.go", ""},
	{"uuid/uuid/capi_linux_amd64.go", "uuid/uuid/capi_cosmo_amd64.go", ""},
	{"uuid/uuid/uuid_linux_arm64.go", "uuid/uuid/uuid_cosmo_arm64.go", ""},
	{"uuid/uuid/capi_linux_arm64.go", "uuid/uuid/capi_cosmo_arm64.go", ""},

	// The ABI0-calling-convention wrapper trampolines qbecc emits are an
	// amd64-only mechanism (arm64's ccgo output never calls the
	// Y-prefixed wrappers), so there is no arm64 counterpart to add.
	{"abi0_linux_amd64.go", "abi0_cosmo_amd64.go", ""},
	{"abi0_linux_amd64.s", "abi0_cosmo_amd64.s", ""},

	{"capi_linux_amd64.go", "capi2_cosmo_amd64.go", ""},
	{"capi_linux_arm64.go", "capi2_cosmo_arm64.go", ""},
	{"ccgo_linux_amd64.go", "ccgo_cosmo_amd64.go", ""},
	{"ccgo_linux_arm64.go", "ccgo_cosmo_arm64.go", ""},
	{"libc_musl_linux_amd64.go", "libc_musl_cosmo_amd64.go", ""},
	{"libc_musl_linux_arm64.go", "libc_musl_cosmo_arm64.go", ""},

	// These six are each already multi-arch (tagged for every linux
	// GOARCH the module supports), so one cosmo copy of each covers
	// both amd64 and arm64.
	{"libc_musl.go", "libc_cosmo_musl.go", ""},
	{"pthread_musl.go", "pthread_cosmo_musl.go", ""},
	{"etc_musl.go", "etc_cosmo_musl.go", ""},
	{"mem_musl.go", "mem_cosmo_musl.go", ""},
	{"builtin.go", "builtin_cosmo.go", ""},
	{"syscall_musl.go", "syscall_cosmo_musl.go", ""},
	{"rtl.go", "rtl_cosmo.go", ""},
	{"atomic.go", "atomic_cosmo.go", ""},
	{"atomic64.go", "atomic64_cosmo.go", ""},
}

// libcTagEdits lists modernc.org/libc files whose existing negative
// build tag ("everything except linux/amd64", "everything except
// linux", and similar) is also true under GOOS=cosmo, so it collides
// with one of the addCosmoFile siblings above declaring the same
// symbols for cosmo. See README.md, "why the exclusions".
var libcTagEdits = []tagEdit{
	{"ccgo.go"},
	{"etc.go"},
	{"ioutil_linux.go"},
	{"libc.go"},
	{"libc64.go"},
	{"libc_amd64.go"},
	{"libc_arm64.go"},
	{"libc_linux_amd64.go"},
	{"libc_unix.go"},
	{"libc_unix1.go"},
	{"libc_unix3.go"},
	{"mem.go"},
	{"mem_brk.go"},
	{"memgrind.go"},
	{"printf.go"},
	{"pthread.go"},
	{"pthread_all.go"},
	{"scanf.go"},
	{"sync.go"},
}

// sysCopies is every golang.org/x/sys/unix file the cosmo build needs.
// Most are arch-generic (their pristine source is already tagged for
// every linux GOARCH x/sys supports, or carries no arch suffix at all),
// so one cosmo copy covers every architecture; the syscall-number and
// type-layout tables genuinely differ per architecture and so are
// copied once per arch.
var sysCopies = []copySpec{
	{"unix/affinity_linux.go", "unix/affinity_cosmo.go", ""},
	{"unix/aliases.go", "unix/aliases_cosmo.go", ""},
	{"unix/auxv.go", "unix/auxv2_cosmo.go", ""},
	{"unix/bluetooth_linux.go", "unix/bluetooth_cosmo.go", ""},
	{"unix/constants.go", "unix/constants_cosmo.go", ""},
	{"unix/dirent.go", "unix/dirent_cosmo.go", ""},
	{"unix/env_unix.go", "unix/env_cosmo.go", ""},
	{"unix/fcntl.go", "unix/fcntl_cosmo.go", ""},
	{"unix/fdset.go", "unix/fdset_cosmo.go", ""},
	{"unix/ifreq_linux.go", "unix/ifreq_cosmo.go", ""},
	{"unix/ioctl_unsigned.go", "unix/ioctl_unsigned_cosmo.go", ""},
	{"unix/mremap.go", "unix/mremap_cosmo.go", ""},
	{"unix/pagesize_unix.go", "unix/pagesize_cosmo.go", ""},
	// race0.go/race.go's own tag pair (aix || (darwin && !race) || ...,
	// vs (darwin && race) || ...) is entirely OS-keyed and none of those
	// OSes is ever "cosmo", so a flat "cosmo" tag on both copies would
	// make them collide instead of staying mutually exclusive -- keep
	// the !race/race split explicitly instead.
	{"unix/race0.go", "unix/race0_cosmo.go", "!race"},
	{"unix/race.go", "unix/race_cosmo.go", "race"},
	{"unix/readdirent_getdents.go", "unix/readdirent_cosmo.go", ""},
	{"unix/sockcmsg_unix.go", "unix/sockcmsg_cosmo.go", ""},
	{"unix/sockcmsg_unix_other.go", "unix/sockcmsgother_cosmo.go", ""},
	{"unix/syscall.go", "unix/syscall3_cosmo.go", ""},
	{"unix/syscall_unix.go", "unix/syscall_cosmo.go", ""},
	{"unix/sysvshm_linux.go", "unix/sysvshm2_cosmo.go", ""},
	{"unix/sysvshm_unix.go", "unix/sysvshm_cosmo.go", ""},
	{"unix/timestruct.go", "unix/timestruct_cosmo.go", ""},
	{"unix/vgetrandom_linux.go", "unix/vgetrandom_cosmo.go", ""},
	{"unix/zerrors_linux.go", "unix/zerrors_cosmo.go", ""},
	{"unix/zptrace_x86_linux.go", "unix/zptracex86_cosmo.go", ""},
	{"unix/zsyscall_linux.go", "unix/zsyscall_cosmo.go", ""},
	{"unix/ztypes_linux.go", "unix/ztypes_cosmo.go", ""},

	{"unix/zsysnum_linux_amd64.go", "unix/zsysnum_cosmo_amd64.go", ""},
	{"unix/ztypes_linux_amd64.go", "unix/ztypes_cosmo_amd64.go", ""},
	{"unix/zsyscall_linux_amd64.go", "unix/zsyscall_cosmo_amd64.go", ""},
	{"unix/zerrors_linux_amd64.go", "unix/zerrors_cosmo_amd64.go", ""},
	{"unix/syscall_linux_amd64.go", "unix/syscall4_cosmo_amd64.go", ""},

	{"unix/zsysnum_linux_arm64.go", "unix/zsysnum_cosmo_arm64.go", ""},
	{"unix/ztypes_linux_arm64.go", "unix/ztypes_cosmo_arm64.go", ""},
	{"unix/zsyscall_linux_arm64.go", "unix/zsyscall_cosmo_arm64.go", ""},
	{"unix/zerrors_linux_arm64.go", "unix/zerrors_cosmo_arm64.go", ""},
	{"unix/syscall_linux_arm64.go", "unix/syscall4_cosmo_arm64.go", ""},

	// syscall2_cosmo.go starts as this copy; patchPrlimit then rewrites
	// its one incompatible function (see README.md, "the Prlimit patch").
	{"unix/syscall_linux.go", "unix/syscall2_cosmo.go", ""},
}

// sysTagEdits: vgetrandom_unsupported.go's fallback ("not linux, or too
// old a Go release for the real vgetrandom path") is also true under
// cosmo, colliding with vgetrandom_cosmo.go's real implementation.
var sysTagEdits = []tagEdit{
	{"unix/vgetrandom_unsupported.go"},
}

// sqliteCopies: the whole sqlite3.c-to-Go translation lives in one file
// per platform; each cosmo copy is that file for the matching arch.
var sqliteCopies = []copySpec{
	{"lib/sqlite_linux_amd64.go", "lib/sqlite_cosmo_amd64.go", ""},
	{"lib/sqlite_linux_arm64.go", "lib/sqlite_cosmo_arm64.go", ""},
}
