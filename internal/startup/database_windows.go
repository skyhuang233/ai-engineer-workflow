//go:build windows

package startup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func normalizeDatabasePathCase(path string) (string, error) {
	path, err := normalizeWindowsVolumeIdentity(path)
	if err != nil {
		return "", err
	}
	return normalizeWindowsPathCase(normalizeWindowsVolumeCase(path))
}

func normalizeWindowsVolumeCase(path string) string {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return path
	}
	return strings.ToLower(volume) + path[len(volume):]
}

func normalizeWindowsPathCase(path string) (string, error) {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return path, nil
	}
	remainder := strings.TrimLeft(path[len(volume):], `\`)
	current := volume + `\`
	caseSensitive, known, err := windowsDirectoryCaseSensitivity(current)
	if err != nil {
		return "", err
	}
	if remainder == "" {
		return current, nil
	}
	components := strings.Split(remainder, `\`)
	for index, component := range components {
		if known && !caseSensitive {
			component = strings.ToLower(component)
		}
		current = filepath.Join(current, component)
		if index == len(components)-1 {
			continue
		}
		nextCaseSensitive, nextKnown, err := windowsDirectoryCaseSensitivity(current)
		if err != nil {
			return "", err
		}
		if nextKnown {
			caseSensitive = nextCaseSensitive
			known = true
			continue
		}
		if _, err := os.Stat(current); err == nil {
			known = false
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return current, nil
}

func normalizeWindowsVolumeIdentity(path string) (string, error) {
	volume := filepath.VolumeName(path)
	if volume == "" || (strings.HasPrefix(volume, `\\`) && !strings.HasPrefix(strings.ToLower(volume), `\\?\volume{`)) {
		return path, nil
	}
	mountPoint, err := windowsVolumeMountPoint(path)
	if err != nil {
		return "", err
	}
	volumeName, err := windowsVolumeName(mountPoint)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(mountPoint, path)
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSuffix(volumeName, `\`), relative), nil
}

func windowsVolumeMountPoint(path string) (string, error) {
	probe := path
	for {
		pathPointer, err := windows.UTF16PtrFromString(probe)
		if err != nil {
			return "", err
		}
		buffer := make([]uint16, windows.MAX_PATH+1)
		err = windows.GetVolumePathName(pathPointer, &buffer[0], uint32(len(buffer)))
		if err == nil {
			return windows.UTF16ToString(buffer), nil
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		probe = parent
	}
}

func windowsVolumeName(mountPoint string) (string, error) {
	mountPointPointer, err := windows.UTF16PtrFromString(mountPoint)
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeNameForVolumeMountPoint(mountPointPointer, &buffer[0], uint32(len(buffer))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buffer), nil
}

func windowsDirectoryCaseSensitivity(path string) (bool, bool, error) {
	directory, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, err
	}
	defer directory.Close()

	var info struct {
		flags uint32
	}
	err = windows.GetFileInformationByHandleEx(
		windows.Handle(directory.Fd()),
		windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
			errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
			errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			return false, false, nil
		}
		return false, false, err
	}
	return info.flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0, true, nil
}
