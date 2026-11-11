#!/usr/bin/env bash
set -u

repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

failures=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); }
check() { # check <description> <command...>
	local desc=$1
	shift
	if "$@"; then pass "$desc"; else fail "$desc"; fi
}

flac() { # flac <path> <freq>
	mkdir -p "$(dirname "$1")"
	ffmpeg -nostdin -hide_banner -loglevel error -f lavfi -i "sine=frequency=$2:duration=1" -c:a flac "$1"
}

# A sandboxed run cannot write the shared Go caches, so keep them in the repo.
export GOMODCACHE=${GOMODCACHE:-$repo/.deps/mod}
export GOCACHE=${GOCACHE:-$repo/.deps/build}
export GOPATH=${GOPATH:-$repo/.deps/gopath}

cd "$repo" || exit 1
bin="$work/flac2alac"
if go build -o "$bin" . ; then pass "go build"; else fail "go build"; exit 1; fi

in="$work/in"
out="$work/out"
flac "$in/My FLAC Rip/album/01 Lower.flac" 440
flac "$in/My FLAC Rip/album/02 Upper.FLAC" 550
flac "$in/My FLAC Rip/album/03 Mixed.Flac" 660
printf 'cover\n' >"$in/My FLAC Rip/album/cover.jpg"
printf 'cue sheet\n' >"$in/My FLAC Rip/album/album.cue"
printf 'plain\n' >"$in/notes.txt"
chmod 640 "$in/My FLAC Rip/album/album.cue"
flac "$in/plain.flac" 770

"$bin" "$in" "$out" 2>"$work/stderr"
check "exit 0 on a clean tree" [ $? -eq 0 ]

for src in "01 Lower" "02 Upper" "03 Mixed"; do
	m4a="$out/My FLAC Rip/album/$src.m4a"
	check "$src.FLAC becomes $src.m4a" test -f "$m4a"
	check "$src.m4a is ALAC" test "$(ffprobe -v error -show_entries stream=codec_name -of csv=p=0 "$m4a")" = alac
done
check "plain.flac at the input root converts" test -f "$out/plain.m4a"

for f in "My FLAC Rip/album/cover.jpg" "My FLAC Rip/album/album.cue" notes.txt; do
	check "non-FLAC $f copied byte for byte" cmp -s "$in/$f" "$out/$f"
done
check "copied album.cue keeps mode 640" test "$(stat -f %Lp "$out/My FLAC Rip/album/album.cue")" = 640
check "parent directory named 'My FLAC Rip' keeps its name" test -d "$out/My FLAC Rip/album"

"$bin" "$in" "$out" 2>/dev/null
check "second run converges and exits 0" [ $? -eq 0 ]
check "no temp files left behind" test -z "$(find "$out" -name '.flac2alac-*')"

printf 'not a flac\n' >"$in/broken.flac"
"$bin" "$in" "$out" 2>"$work/stderr"
check "broken FLAC exits 1" [ $? -eq 1 ]
check "failure names broken.flac on stderr" grep -q broken.flac "$work/stderr"
check "broken FLAC leaves no .m4a" test ! -e "$out/broken.m4a"

"$bin" 2>/dev/null
check "no arguments exits 2" [ $? -eq 2 ]

"$bin" "$in" "$in/copies" 2>/dev/null
check "output inside input is refused" [ $? -eq 1 ]
check "refused output writes nothing" test ! -e "$in/copies"

"$bin" "$work/missing" "$out" 2>/dev/null
check "missing input exits 1" [ $? -eq 1 ]

if [ "$failures" -eq 0 ]; then
	printf 'all checks passed\n'
else
	printf '%d check(s) failed\n' "$failures"
	exit 1
fi
