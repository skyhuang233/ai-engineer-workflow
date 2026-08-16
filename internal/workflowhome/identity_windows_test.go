//go:build windows

package workflowhome

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSameFilesystemPathAcceptsWindowsShortAndLongNames(t *testing.T) {
	long := t.TempDir()
	input, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetShortPathName(input, &buffer[0], uint32(len(buffer)))
	if err != nil || length == 0 {
		t.Skip("Windows short names are unavailable")
	}
	short := filepath.Clean(windows.UTF16ToString(buffer[:length]))
	if short == long {
		t.Skip("volume does not expose an 8.3 alias")
	}
	same, err := SameFilesystemPath(short, long)
	if err != nil || !same {
		t.Fatalf("short %q and long %q were not one filesystem identity: %v", short, long, err)
	}
}
