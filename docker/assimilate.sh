#!/bin/sh
# Turn the GOOS=cosmo fat APE at /in/server_cosmo_fat into a plain ELF at
# /out/server. Runs in an image build stage that has a shell; the final image
# does not, which is the whole reason this step exists.
#
# The kernel cannot exec an APE: the file starts with the DOS/shell magic, so a
# shell runs its boot script, which writes a real ELF header over a COPY under
# /tmp/.ape-run-1-<uid>/<file identity>/. That copy is what ships.
# see https://github.com/wow-look-at-my/gosmopolitan/blob/master/docs/APE-STAGING.md
set -eu

# The boot script stages the copy BEFORE it execs the program, so the timeout
# that stops a server which would otherwise listen forever does not affect it.
timeout 60 /bin/sh /in/server_cosmo_fat >/dev/null 2>&1 || true

set -- /tmp/.ape-run-1-*/*/server_cosmo_fat
if [ ! -f "${1:-}" ]; then
	echo "assimilate: the APE staged no copy -- its boot script never ran" >&2
	exit 1
fi
if [ "$#" -ne 1 ]; then
	echo "assimilate: expected exactly 1 staged copy, found $#: $*" >&2
	exit 1
fi
if ! head -c 4 "$1" | grep -q "$(printf '\177ELF')"; then
	echo "assimilate: the staged copy is not an ELF: $1" >&2
	exit 1
fi

install -D -m 755 "$1" /out/server
echo "assimilate: $1 -> /out/server"

# Do NOT try to prove this by exec'ing it. execvp falls back to /bin/sh on
# ENOEXEC, so the pristine APE and the assimilated ELF both "run" wherever a
# shell exists, and the one place that distinction matters -- the final image --
# is the one place no shell is there to measure it. The magic bytes ARE the
# property: the file that broke production began "MZqFpD=", and the check above
# is what refuses it.
