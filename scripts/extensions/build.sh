#!/bin/sh
# Build pgvector for one platform against coyote's pinned PostgreSQL tree,
# and pack it into the archive coyote installs (D53; docs/ARCHITECTURE.md, "Extensions").
#
#   scripts/extensions/build.sh TARGET PG_ROOT OUT_DIR
#
# TARGET is a PostgreSQL build target (internal/pg/version.go):
#   aarch64-apple-darwin, x86_64-apple-darwin      run on a Mac (Xcode tools)
#   x86_64-unknown-linux-gnu, aarch64-unknown-linux-gnu,
#   x86_64-pc-windows-msvc                         run in the builder image
#                                                  (scripts/extensions/build-linux.sh)
# PG_ROOT is that target's pinned tree (extbuild pgtree prints it).
# For a cross build (Linux arm64 on amd64), HOST_PG_ROOT must be a tree that
# runs here: its pg_config answers, with paths moved to PG_ROOT.
#
# Writes OUT_DIR/pgvector-<v>-pg<pg>-<target>.tar.gz and prints its sha256.
# Run from the repository root (extbuild is built from ./scripts/extensions).
set -eu

[ $# -eq 3 ] || { sed -n 2,16p "$0" >&2; exit 2; }
target=$1
pgroot=$(cd "$2" && pwd)
mkdir -p "$3"
out=$(cd "$3" && pwd)
here=$(cd "$(dirname "$0")" && pwd)
. "$here/pgvector.env"

pgversion=$(basename "$pgroot" | sed -n 's/^postgresql-\([0-9.]*\)-.*/\1/p')
[ -n "$pgversion" ] || { echo "cannot read the PostgreSQL version from $pgroot" >&2; exit 1; }
name="pgvector-$PGVECTOR_VERSION-pg$pgversion-$target"

work=$(mktemp -d "${TMPDIR:-/tmp}/pgvector-build.XXXXXX")
trap 'rm -rf "$work"' EXIT
src="$work/src"

# 1. Source: the pinned tag, checked against the pinned commit.
git -c advice.detachedHead=false clone -q --depth 1 --branch "$PGVECTOR_TAG" "$PGVECTOR_REPO" "$src"
got=$(git -C "$src" rev-parse HEAD)
[ "$got" = "$PGVECTOR_COMMIT" ] || { echo "pgvector $PGVECTOR_TAG is commit $got, pinned $PGVECTOR_COMMIT" >&2; exit 1; }
grep -q "^default_version = '$PGVECTOR_VERSION'" "$src/vector.control" || { echo "vector.control is not $PGVECTOR_VERSION" >&2; exit 1; }

# Upstream's flags except -march=native (OPTFLAGS=""): binaries must run
# on any processor of the platform, like the PostgreSQL build.
vecflags="-ftree-vectorize -fassociative-math -fno-signed-zeros -fno-trapping-math -ffp-contract=fast"
# No build-machine paths in the binaries (they would make every build
# differ): the work folder and the PostgreSQL tree are renamed.
mapflags="-ffile-prefix-map=$work=build -ffile-prefix-map=$pgroot=postgresql"

case "$target" in
*-apple-darwin)
	suffix=.dylib
	arch=${target%%-*}
	[ "$arch" = aarch64 ] && arch=arm64
	minos=$(otool -l "$pgroot/bin/postgres" | awk '/LC_BUILD_VERSION/{f=1} f&&/minos/{print $2; exit}')
	export MACOSX_DEPLOYMENT_TARGET="$minos"
	cc="clang -arch $arch"
	# The x86_64 tree's pg_config runs through Rosetta on Apple silicon.
	make -s -C "$src" PG_CONFIG="$pgroot/bin/pg_config" OPTFLAGS="" with_llvm=no \
		CC="$cc" PG_SYSROOT="$(xcrun --show-sdk-path)" PG_CFLAGS="$vecflags $mapflags"
	built="$src/vector.dylib"
	file "$built" | grep -q "$arch" || { echo "built for the wrong processor: $(file "$built")" >&2; exit 1; }
	;;
*-linux-gnu)
	suffix=.so
	case "$target" in
	aarch64-*) cc=aarch64-linux-gnu-gcc ;;
	x86_64-*) cc=gcc ;;
	esac
	pgconfig="$pgroot/bin/pg_config"
	if [ -n "${HOST_PG_ROOT:-}" ]; then
		# Cross build: answer with a tree that runs here, paths moved to
		# the target tree (pgxs asks pg_config only for paths; flags come
		# from the target tree's Makefile.global).
		pgconfig="$work/pg_config"
		printf '#!/bin/sh\n"%s/bin/pg_config" "$@" | sed "s#%s#%s#g"\n' "$HOST_PG_ROOT" "$HOST_PG_ROOT" "$pgroot" >"$pgconfig"
		chmod +x "$pgconfig"
	fi
	make -s -C "$src" PG_CONFIG="$pgconfig" OPTFLAGS="" with_llvm=no CC="$cc" PG_CFLAGS="$vecflags $mapflags"
	built="$src/vector.so"
	# The PostgreSQL build needs glibc 2.36 at most (Debian 12); so must we.
	need=$("${cc%gcc}objdump" -T "$built" | grep -o 'GLIBC_[0-9.]*' | sort -t. -k2,2n -k3,3n -u | tail -1)
	echo "highest glibc symbol version used: $need"
	case "$need" in GLIBC_2.3[7-9]* | GLIBC_2.[4-9]*) echo "needs a newer glibc than PostgreSQL ($need)" >&2; exit 1 ;; esac
	;;
x86_64-pc-windows-msvc)
	# No MSVC here: MinGW-w64 against the UCRT (the C runtime the MSVC
	# PostgreSQL build uses), linked to the tree's postgres.lib. Where the
	# PostgreSQL headers give MinGW a different binary layout than the
	# MSVC build, the MSVC one is used (checked below and at run time on
	# Windows):
	# - spinlocks: MinGW would get a 1-byte slock_t, the MSVC postgres.exe
	#   spins on 4 bytes (s_lock() is called on contention): a patched
	#   copy of s_lock.h gives MinGW the MSVC definition;
	# - strtof: the headers send MinGW to pg_strtof, which the MSVC build
	#   does not have; the UCRT strtof is what postgres.exe itself uses;
	# - PG_TRY's jump buffer differs too: pgvector uses no PG_TRY (checked).
	suffix=.dll
	cc=x86_64-w64-mingw32ucrt-gcc
	if grep -rqE 'PG_TRY|sigsetjmp|siglongjmp' "$src/src"; then
		echo "pgvector now uses PG_TRY; the MinGW build cannot be used with the MSVC PostgreSQL build" >&2
		exit 1
	fi
	cp -R "$pgroot/include" "$work/include"
	slock="$work/include/server/storage/s_lock.h"
	awk '
		/^#ifdef __x86_64__/ && !done {
			print "#if defined(__x86_64__) && defined(__MINGW64__)	/* CoyoteDB (D53): the MSVC layout */"
			print "#define HAS_TEST_AND_SET"
			print "typedef LONG slock_t;"
			print "#define TAS(lock) (InterlockedCompareExchange(lock, 1, 0))"
			print "#define SPIN_DELAY() spin_delay()"
			print "static __inline__ void spin_delay(void) { __asm__ __volatile__(\" rep; nop\\n\"); }"
			print "#define S_UNLOCK(lock) do { __asm__ __volatile__(\"\" : : : \"memory\"); (*(lock)) = 0; } while (0)"
			print "#endif"
			print "#if defined(__x86_64__) && !defined(__MINGW64__)"
			done = 1
			next
		}
		{ print }
	' "$pgroot/include/server/storage/s_lock.h" >"$slock"
	grep -q 'CoyoteDB (D53)' "$slock" || { echo "s_lock.h patch did not apply" >&2; exit 1; }
	# - pg_bitutils.h turns pg_popcount32/64 into function pointers (data
	#   in postgres.exe) only under _MSC_VER; MinGW would call them as
	#   functions. HAVE_X86_64_POPCNTQ gives MinGW the MSVC declarations.
	cflags="-O2 -Wall -Wno-unused-parameter -Wno-format -Wno-pointer-sign -fno-strict-aliasing -fwrapv -fexcess-precision=standard -Dpg_strtof=strtof -DHAVE_X86_64_POPCNTQ $vecflags $mapflags"
	inc="-I$work/include/server/port/win32 -I$work/include/server -I$work/include"
	cat >"$work/abi.c" <<'EOF2'
#include "postgres.h"
#include "storage/spin.h"
#include "port/atomics.h"
_Static_assert(sizeof(slock_t) == 4, "slock_t must be the MSVC LONG");
_Static_assert(sizeof(long) == 4 && sizeof(void *) == 8, "LLP64");
_Static_assert(sizeof(pg_atomic_uint64) == 8 && _Alignof(pg_atomic_uint64) == 8, "atomics");
_Static_assert(sizeof(sigjmp_buf) > 0, "");
EOF2
	# shellcheck disable=SC2086
	$cc $cflags $inc -c "$work/abi.c" -o "$work/abi.o"
	objs=""
	for c in "$src"/src/*.c; do
		o="$work/$(basename "$c" .c).o"
		# shellcheck disable=SC2086
		$cc $cflags $inc -c "$c" -o "$o"
		objs="$objs $o"
	done
	# Every name taken from postgres.exe must be used the way the MSVC
	# build exports it. Data (only __imp_NAME in postgres.lib) read as a
	# function is refused by the linker (--disable-auto-import); a function
	# (NAME and __imp_NAME) read through __imp_ as data is checked here.
	x86_64-w64-mingw32-nm "$pgroot/lib/postgres.lib" 2>/dev/null | awk '$2 == "T" {print $3}' | sort -u >"$work/code.txt"
	# shellcheck disable=SC2086
	x86_64-w64-mingw32-nm -u $objs | sed -n 's/^ *U __imp_//p' | sort -u >"$work/imp.txt"
	grep -qx palloc "$work/code.txt" && grep -qx CurrentMemoryContext "$work/imp.txt" || { echo "cannot read the import lists" >&2; exit 1; }
	bad=$(comm -12 "$work/code.txt" "$work/imp.txt")
	[ -z "$bad" ] || { echo "functions of postgres.exe used as data (header differs from the MSVC build): $bad" >&2; exit 1; }
	# No time stamp, and no image base derived from the (temporary) output
	# path: the same sources give the same DLL. -s drops the C runtime's
	# debug sections.
	# shellcheck disable=SC2086
	$cc -shared -o "$src/vector.dll" $objs "$pgroot/lib/postgres.lib" -static-libgcc -s \
		-Wl,--no-insert-timestamp -Wl,--disable-auto-image-base \
		-Wl,--disable-auto-import -Wl,--disable-runtime-pseudo-reloc
	cp "$src/sql/vector.sql" "$src/sql/vector--$PGVECTOR_VERSION.sql"
	built="$src/vector.dll"
	echo "imports:"
	x86_64-w64-mingw32-objdump -p "$built" | sed -n 's/^.*DLL Name: /  /p'
	if x86_64-w64-mingw32-objdump -p "$built" | grep -i 'DLL Name:' | grep -iv 'postgres.exe\|KERNEL32.dll\|api-ms-win-crt-' >/dev/null; then
		echo "the DLL needs a library that is not part of Windows or the PostgreSQL build" >&2
		exit 1
	fi
	;;
*)
	echo "unknown target $target" >&2
	exit 2
	;;
esac

# 2. Stage exactly the files coyote may install (internal/pg/ext.go).
stage="$work/$name"
mkdir -p "$stage/lib" "$stage/share/extension" "$stage/dependency-notices/pgvector"
cp "$built" "$stage/lib/vector$suffix"
cp "$src/vector.control" "$stage/share/extension/"
cp "$src/sql/vector--$PGVECTOR_VERSION.sql" "$src"/sql/vector--*--*.sql "$stage/share/extension/"
cp "$src/LICENSE" "$stage/dependency-notices/pgvector/LICENSE"
pgsha=$(sed -n 's/^.*"archive_sha256":"\([0-9a-f]*\)".*$/\1/p' "$pgroot/.coyote-verified")
cat >"$stage/build-info.json" <<EOF
{
  "project": "pgvector",
  "version": "$PGVECTOR_VERSION",
  "source": "$PGVECTOR_REPO",
  "tag": "$PGVECTOR_TAG",
  "commit": "$PGVECTOR_COMMIT",
  "postgresql": "$pgversion",
  "postgresql_target": "$target",
  "postgresql_archive_sha256": "$pgsha",
  "compiler": "$($cc --version | head -1 | sed 's/"/\\"/g')",
  "flags": "OPTFLAGS='' $vecflags",
  "script": "scripts/extensions/build.sh"
}
EOF

# 3. Pack reproducibly.
go run ./scripts/extensions/extbuild pack -src "$stage" -out "$out/$name.tar.gz"
