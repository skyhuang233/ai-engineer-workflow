//go:build windows

package credential

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	credentialTypeGeneric         = 1
	credentialPersistLocalMachine = 2
	errorNotFound                 = windows.Errno(1168)
)

var (
	advapi32   = windows.NewLazySystemDLL("advapi32.dll")
	credReadW  = advapi32.NewProc("CredReadW")
	credWriteW = advapi32.NewProc("CredWriteW")
	credFree   = advapi32.NewProc("CredFree")
)

type nativeCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

type WindowsStore struct{}

func NewStore() Store { return WindowsStore{} }

func (WindowsStore) Get(_ context.Context, target string) (string, error) {
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return "", err
	}
	var native *nativeCredential
	ok, _, callErr := credReadW.Call(
		uintptr(unsafe.Pointer(targetPtr)), credentialTypeGeneric, 0,
		uintptr(unsafe.Pointer(&native)),
	)
	if ok == 0 {
		if errors.Is(callErr, errorNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("CredReadW: %w", callErr)
	}
	defer credFree.Call(uintptr(unsafe.Pointer(native)))
	if native.CredentialBlobSize == 0 {
		return "", ErrNotFound
	}
	blob := unsafe.Slice(native.CredentialBlob, int(native.CredentialBlobSize))
	return string(blob), nil
}

func (WindowsStore) Set(_ context.Context, target, secret string) error {
	secret = strings.TrimSpace(secret)
	if target == "" || secret == "" {
		return errors.New("credential target and secret are required")
	}
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	userPtr, err := windows.UTF16PtrFromString("workflow-control-plane")
	if err != nil {
		return err
	}
	blob := []byte(secret)
	native := nativeCredential{
		Type: credentialTypeGeneric, TargetName: targetPtr,
		CredentialBlobSize: uint32(len(blob)), CredentialBlob: &blob[0],
		Persist: credentialPersistLocalMachine, UserName: userPtr,
	}
	ok, _, callErr := credWriteW.Call(uintptr(unsafe.Pointer(&native)), 0)
	if ok == 0 {
		return fmt.Errorf("CredWriteW: %w", callErr)
	}
	return nil
}
