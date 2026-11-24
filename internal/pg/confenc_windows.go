//go:build windows

package pg

// PostgreSQL on Windows opens files through the C runtime's narrow
// ("ANSI") functions: every path it reads from a config file, and every
// path in its command line after the C runtime converts it, is in the
// system's ANSI code page (1252 on a Western Windows), not UTF-8. A path
// written to coyote.conf as UTF-8 therefore names a different file as soon
// as it holds a non-ASCII letter (seen on a Windows test PC: a data folder "…\Jörg…" gave
// "could not access private key file …: No such file or directory"). So the
// config file is written in the ANSI code page, and a data folder whose
// name that code page cannot hold at all is refused up front with a plain
// message (Postgres could not use it, whatever we wrote).
//
// The same narrow functions are limited to MAX_PATH (260 characters), and
// Postgres' own files go about 95 characters below the data folder (its
// program files in cache\postgresql-…\share\timezone\America\…). On a Windows test PC a
// 165-character data folder worked and a 180-character one made initdb fail
// ("invalid binary … initdb.exe"), retried forever. So a data folder longer
// than maxDataDirLen is refused up front.

import (
	"fmt"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	cpACP            = 0     // CP_ACP
	cpUTF8           = 65001 // CP_UTF8 (the "Use Unicode UTF-8" system option)
	wcNoBestFitChars = 0x400 // WC_NO_BEST_FIT_CHARS
)

var procWideCharToMultiByte = windows.NewLazySystemDLL("kernel32.dll").NewProc("WideCharToMultiByte")

// toANSI converts s to the ANSI code page. ok is false if some character
// has no exact equivalent there (no best-fit guesses: "ø" must not quietly
// become "o" and name another folder).
func toANSI(s string) (b []byte, ok bool, err error) {
	if s == "" {
		return nil, true, nil
	}
	if windows.GetACP() == cpUTF8 {
		return []byte(s), true, nil
	}
	u := utf16.Encode([]rune(s))
	var usedDefault int32
	n, _, e := procWideCharToMultiByte.Call(cpACP, wcNoBestFitChars,
		uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)), 0, 0, 0, uintptr(unsafe.Pointer(&usedDefault)))
	if n == 0 {
		return nil, false, fmt.Errorf("convert to code page %d: %w", windows.GetACP(), e)
	}
	out := make([]byte, n)
	usedDefault = 0
	n, _, e = procWideCharToMultiByte.Call(cpACP, wcNoBestFitChars,
		uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)), uintptr(unsafe.Pointer(&out[0])), n, 0, uintptr(unsafe.Pointer(&usedDefault)))
	if n == 0 {
		return nil, false, fmt.Errorf("convert to code page %d: %w", windows.GetACP(), e)
	}
	return out[:n], usedDefault == 0, nil
}

func encodeConfFile(s string) ([]byte, error) {
	b, ok, err := toANSI(s)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("the Postgres config holds characters Windows code page %d cannot represent", windows.GetACP())
	}
	return b, nil
}

// maxDataDirLen is the longest data folder path (in UTF-16 characters)
// Postgres can work under on Windows; see the comment at the top.
const maxDataDirLen = 160

func checkPostgresPath(p string) error {
	if n := len(utf16.Encode([]rune(p))); n > maxDataDirLen {
		return fmt.Errorf("PostgreSQL on Windows cannot use the folder %s: its path is %d characters long and Windows limits the files inside it to 260. Pick a folder whose path has at most %d characters, for example %s",
			p, n, maxDataDirLen, `C:\ProgramData\CoyoteDB`)
	}
	_, ok, err := toANSI(p)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("PostgreSQL on Windows cannot use the folder %s: its name has characters this system's code page (%d) cannot represent. Pick a folder whose name uses only letters of this system's language, for example %s",
			p, windows.GetACP(), `C:\ProgramData\CoyoteDB`)
	}
	return nil
}
