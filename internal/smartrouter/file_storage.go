package smartrouter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	routerMetadataFileMode os.FileMode = 0o600
	routerStorageDirMode   os.FileMode = 0o700
	routerSecretVersion                = 1
	routerSecretAlgorithm              = "AES-256-GCM"
)

// FileRouterMetadataStore stores one crash-safe JSON metadata document.
type FileRouterMetadataStore struct {
	path string
	mu   sync.Mutex
}

func NewFileRouterMetadataStore(path string) (*FileRouterMetadataStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("create router metadata store: path is empty")
	}
	return &FileRouterMetadataStore{path: filepath.Clean(path)}, nil
}

func (s *FileRouterMetadataStore) LoadRouterMetadata(ctx context.Context) (RouterMetadataDocument, error) {
	if err := contextError(ctx); err != nil {
		return RouterMetadataDocument{}, err
	}
	if s == nil {
		return RouterMetadataDocument{}, errors.New("load router metadata: store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *FileRouterMetadataStore) ReplaceRouterMetadata(ctx context.Context, expectedRevision uint64, router config.RouterConfig) (RouterMetadataDocument, error) {
	if err := contextError(ctx); err != nil {
		return RouterMetadataDocument{}, err
	}
	if s == nil {
		return RouterMetadataDocument{}, errors.New("replace router metadata: store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.loadLocked()
	if err != nil {
		return RouterMetadataDocument{}, err
	}
	if current.Revision != expectedRevision {
		return RouterMetadataDocument{}, &RouterRevisionConflictError{
			Expected: expectedRevision,
			Actual:   current.Revision,
		}
	}
	next := RouterMetadataDocument{
		Revision:  current.Revision + 1,
		UpdatedAt: time.Now().UTC(),
		Router:    cloneRouterConfig(router),
	}
	data, errMarshal := json.MarshalIndent(next, "", "  ")
	if errMarshal != nil {
		return RouterMetadataDocument{}, fmt.Errorf("marshal router metadata: %w", errMarshal)
	}
	data = append(data, '\n')
	if errWrite := atomicWritePrivateFile(s.path, data); errWrite != nil {
		return RouterMetadataDocument{}, fmt.Errorf("replace router metadata: %w", errWrite)
	}
	return cloneRouterMetadataDocument(next), nil
}

func (s *FileRouterMetadataStore) loadLocked() (RouterMetadataDocument, error) {
	data, errRead := os.ReadFile(s.path)
	if errors.Is(errRead, os.ErrNotExist) {
		return RouterMetadataDocument{}, nil
	}
	if errRead != nil {
		return RouterMetadataDocument{}, fmt.Errorf("read router metadata: %w", errRead)
	}
	var document RouterMetadataDocument
	if errDecode := json.Unmarshal(data, &document); errDecode != nil {
		return RouterMetadataDocument{}, fmt.Errorf("decode router metadata: %w", errDecode)
	}
	if document.Revision == 0 {
		return RouterMetadataDocument{}, errors.New("decode router metadata: revision must be positive")
	}
	return cloneRouterMetadataDocument(document), nil
}

// ParseRouterMasterKey accepts standard or raw base64 and hexadecimal
// encodings of exactly 32 bytes.
func ParseRouterMasterKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("parse router master key: value is empty")
	}
	decoders := []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, decode := range decoders {
		decoded, errDecode := decode(value)
		if errDecode == nil && len(decoded) == 32 {
			return bytes.Clone(decoded), nil
		}
		clear(decoded)
	}
	return nil, errors.New("parse router master key: expected a base64 or hexadecimal encoding of 32 bytes")
}

// EncryptedFileRouterSecretStore stores versioned AES-256-GCM envelopes.
type EncryptedFileRouterSecretStore struct {
	path string
	key  []byte
	mu   sync.Mutex
}

type encryptedRouterSecretFile struct {
	Version int                                   `json:"version"`
	Entries map[string]encryptedRouterSecretEntry `json:"entries"`
}

type encryptedRouterSecretEntry struct {
	Version    int       `json:"version"`
	Algorithm  string    `json:"algorithm"`
	Nonce      string    `json:"nonce"`
	Ciphertext string    `json:"ciphertext"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func NewEncryptedFileRouterSecretStore(path string, masterKey []byte) (*EncryptedFileRouterSecretStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("create router secret store: path is empty")
	}
	if len(masterKey) != 32 {
		return nil, errors.New("create router secret store: master key must be 32 bytes")
	}
	return &EncryptedFileRouterSecretStore{
		path: filepath.Clean(path),
		key:  bytes.Clone(masterKey),
	}, nil
}

func (s *EncryptedFileRouterSecretStore) PutRouterSecret(ctx context.Context, reference string, secret []byte) (RouterSecretMetadata, error) {
	if err := contextError(ctx); err != nil {
		return RouterSecretMetadata{}, err
	}
	reference = strings.TrimSpace(reference)
	if s == nil {
		return RouterSecretMetadata{}, errors.New("put router secret: store is nil")
	}
	if reference == "" {
		return RouterSecretMetadata{}, errors.New("put router secret: reference is empty")
	}
	if len(secret) == 0 {
		return RouterSecretMetadata{}, errors.New("put router secret: value is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	document, errLoad := s.loadLocked()
	if errLoad != nil {
		return RouterSecretMetadata{}, errLoad
	}
	entry, errEncrypt := s.encrypt(reference, secret)
	if errEncrypt != nil {
		return RouterSecretMetadata{}, errEncrypt
	}
	document.Entries[reference] = entry
	if errWrite := s.writeLocked(document); errWrite != nil {
		return RouterSecretMetadata{}, errWrite
	}
	return RouterSecretMetadata{Configured: true, UpdatedAt: entry.UpdatedAt}, nil
}

func (s *EncryptedFileRouterSecretStore) DeleteRouterSecret(ctx context.Context, reference string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	reference = strings.TrimSpace(reference)
	if s == nil {
		return errors.New("delete router secret: store is nil")
	}
	if reference == "" {
		return errors.New("delete router secret: reference is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	document, errLoad := s.loadLocked()
	if errLoad != nil {
		return errLoad
	}
	if _, exists := document.Entries[reference]; !exists {
		return nil
	}
	delete(document.Entries, reference)
	return s.writeLocked(document)
}

func (s *EncryptedFileRouterSecretStore) RouterSecretMetadata(ctx context.Context, reference string) (RouterSecretMetadata, error) {
	if err := contextError(ctx); err != nil {
		return RouterSecretMetadata{}, err
	}
	reference = strings.TrimSpace(reference)
	if s == nil {
		return RouterSecretMetadata{}, errors.New("read router secret metadata: store is nil")
	}
	if reference == "" {
		return RouterSecretMetadata{}, errors.New("read router secret metadata: reference is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	document, errLoad := s.loadLocked()
	if errLoad != nil {
		return RouterSecretMetadata{}, errLoad
	}
	entry, exists := document.Entries[reference]
	if !exists {
		return RouterSecretMetadata{}, nil
	}
	return RouterSecretMetadata{Configured: true, UpdatedAt: entry.UpdatedAt.UTC()}, nil
}

func (s *EncryptedFileRouterSecretStore) ResolveUpstreamSecret(ctx context.Context, reference string) ([]byte, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	reference = strings.TrimSpace(reference)
	if s == nil {
		return nil, errors.New("resolve router secret: store is nil")
	}
	if reference == "" {
		return nil, errors.New("resolve router secret: reference is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	document, errLoad := s.loadLocked()
	if errLoad != nil {
		return nil, errLoad
	}
	entry, exists := document.Entries[reference]
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrRouterSecretNotFound, reference)
	}
	return s.decrypt(reference, entry)
}

func (s *EncryptedFileRouterSecretStore) encrypt(reference string, secret []byte) (encryptedRouterSecretEntry, error) {
	aead, errAEAD := routerSecretAEAD(s.key)
	if errAEAD != nil {
		return encryptedRouterSecretEntry{}, errAEAD
	}
	nonce := make([]byte, aead.NonceSize())
	if _, errRandom := rand.Read(nonce); errRandom != nil {
		return encryptedRouterSecretEntry{}, fmt.Errorf("encrypt router secret: random nonce: %w", errRandom)
	}
	ciphertext := aead.Seal(nil, nonce, secret, routerSecretAdditionalData(reference))
	now := time.Now().UTC()
	return encryptedRouterSecretEntry{
		Version:    routerSecretVersion,
		Algorithm:  routerSecretAlgorithm,
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
		UpdatedAt:  now,
	}, nil
}

func (s *EncryptedFileRouterSecretStore) decrypt(reference string, entry encryptedRouterSecretEntry) ([]byte, error) {
	if entry.Version != routerSecretVersion || entry.Algorithm != routerSecretAlgorithm {
		return nil, errors.New("decrypt router secret: unsupported envelope")
	}
	nonce, errNonce := base64.RawStdEncoding.DecodeString(entry.Nonce)
	if errNonce != nil {
		return nil, errors.New("decrypt router secret: invalid envelope")
	}
	ciphertext, errCiphertext := base64.RawStdEncoding.DecodeString(entry.Ciphertext)
	if errCiphertext != nil {
		clear(nonce)
		return nil, errors.New("decrypt router secret: invalid envelope")
	}
	defer clear(nonce)
	defer clear(ciphertext)
	aead, errAEAD := routerSecretAEAD(s.key)
	if errAEAD != nil {
		return nil, errAEAD
	}
	plaintext, errOpen := aead.Open(nil, nonce, ciphertext, routerSecretAdditionalData(reference))
	if errOpen != nil {
		return nil, errors.New("decrypt router secret: authentication failed")
	}
	return plaintext, nil
}

func (s *EncryptedFileRouterSecretStore) loadLocked() (encryptedRouterSecretFile, error) {
	document := encryptedRouterSecretFile{
		Version: routerSecretVersion,
		Entries: make(map[string]encryptedRouterSecretEntry),
	}
	data, errRead := os.ReadFile(s.path)
	if errors.Is(errRead, os.ErrNotExist) {
		return document, nil
	}
	if errRead != nil {
		return encryptedRouterSecretFile{}, fmt.Errorf("read router secret store: %w", errRead)
	}
	if errDecode := json.Unmarshal(data, &document); errDecode != nil {
		return encryptedRouterSecretFile{}, fmt.Errorf("decode router secret store: %w", errDecode)
	}
	if document.Version != routerSecretVersion {
		return encryptedRouterSecretFile{}, errors.New("decode router secret store: unsupported version")
	}
	if document.Entries == nil {
		document.Entries = make(map[string]encryptedRouterSecretEntry)
	}
	return document, nil
}

func (s *EncryptedFileRouterSecretStore) writeLocked(document encryptedRouterSecretFile) error {
	document.Version = routerSecretVersion
	data, errMarshal := json.MarshalIndent(document, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal router secret store: %w", errMarshal)
	}
	data = append(data, '\n')
	if errWrite := atomicWritePrivateFile(s.path, data); errWrite != nil {
		return fmt.Errorf("write router secret store: %w", errWrite)
	}
	return nil
}

func routerSecretAEAD(key []byte) (cipher.AEAD, error) {
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return nil, fmt.Errorf("create router secret cipher: %w", errCipher)
	}
	aead, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return nil, fmt.Errorf("create router secret cipher: %w", errGCM)
	}
	return aead, nil
}

func routerSecretAdditionalData(reference string) []byte {
	return []byte(fmt.Sprintf("smart-router-secret:v%d:%s", routerSecretVersion, reference))
}

func atomicWritePrivateFile(path string, data []byte) (err error) {
	directory := filepath.Dir(path)
	if errMkdir := os.MkdirAll(directory, routerStorageDirMode); errMkdir != nil {
		return fmt.Errorf("create storage directory: %w", errMkdir)
	}
	temp, errCreate := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if errCreate != nil {
		return fmt.Errorf("create temporary file: %w", errCreate)
	}
	tempPath := temp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if errChmod := temp.Chmod(routerMetadataFileMode); errChmod != nil {
		_ = temp.Close()
		return fmt.Errorf("set temporary file mode: %w", errChmod)
	}
	if _, errWrite := temp.Write(data); errWrite != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary file: %w", errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary file: %w", errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close temporary file: %w", errClose)
	}
	if errRename := os.Rename(tempPath, path); errRename != nil {
		return fmt.Errorf("replace destination file: %w", errRename)
	}
	directoryHandle, errOpen := os.Open(directory)
	if errOpen != nil {
		return fmt.Errorf("open storage directory: %w", errOpen)
	}
	defer func() {
		if errClose := directoryHandle.Close(); err == nil && errClose != nil {
			err = fmt.Errorf("close storage directory: %w", errClose)
		}
	}()
	if errSync := directoryHandle.Sync(); errSync != nil {
		return fmt.Errorf("sync storage directory: %w", errSync)
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
