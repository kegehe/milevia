//go:build windows

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

type dataBlob struct {
	Size uint32
	Data *byte
}

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	cryptProtectData   = crypt32.NewProc("CryptProtectData")
	cryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	localFree          = kernel32.NewProc("LocalFree")
)

func loadCredentialFile(path string) (storedCredential, error) {
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		return storedCredential{}, err
	}
	plaintext, err := unprotectCredential(ciphertext)
	if err != nil {
		return storedCredential{}, err
	}
	return parseStoredCredential(plaintext)
}

func saveCredentialFile(path string, credential storedCredential) error {
	ciphertext, err := protectCredential(serializeCredential(credential))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, ciphertext, 0o600)
}

func protectCredential(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("empty credential")
	}
	in := dataBlob{Size: uint32(len(plaintext)), Data: &plaintext[0]}
	var out dataBlob
	ok, _, callErr := cryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)),
	)
	if ok == 0 {
		return nil, fmt.Errorf("DPAPI encrypt: %w", callErr)
	}
	defer localFree.Call(uintptr(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func unprotectCredential(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, fmt.Errorf("empty credential")
	}
	in := dataBlob{Size: uint32(len(ciphertext)), Data: &ciphertext[0]}
	var out dataBlob
	ok, _, callErr := cryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)),
	)
	if ok == 0 {
		return nil, fmt.Errorf("DPAPI decrypt: %w", callErr)
	}
	defer localFree.Call(uintptr(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
