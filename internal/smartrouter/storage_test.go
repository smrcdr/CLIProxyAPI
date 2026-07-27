package smartrouter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestFileRouterMetadataStoreRevisionAndDefensiveCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router", "metadata.json")
	store, err := NewFileRouterMetadataStore(path)
	if err != nil {
		t.Fatalf("NewFileRouterMetadataStore() error = %v", err)
	}

	empty, err := store.LoadRouterMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadRouterMetadata(empty) error = %v", err)
	}
	if empty.Revision != 0 || len(empty.Router.Upstreams) != 0 {
		t.Fatalf("empty document = %#v", empty)
	}

	router := routerStorageTestConfig()
	created, err := store.ReplaceRouterMetadata(context.Background(), 0, router)
	if err != nil {
		t.Fatalf("ReplaceRouterMetadata() error = %v", err)
	}
	if created.Revision != 1 || created.UpdatedAt.IsZero() {
		t.Fatalf("created document = %#v", created)
	}
	created.Router.Upstreams[0].Headers["X-Test"] = "mutated"
	created.Router.ModelGroups[0].Routes[0].UpstreamModel = "mutated"

	loaded, err := store.LoadRouterMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadRouterMetadata() error = %v", err)
	}
	if got := loaded.Router.Upstreams[0].Headers["X-Test"]; got != "value" {
		t.Fatalf("persisted header = %q", got)
	}
	if got := loaded.Router.ModelGroups[0].Routes[0].UpstreamModel; got != "upstream-model" {
		t.Fatalf("persisted upstream model = %q", got)
	}

	_, err = store.ReplaceRouterMetadata(context.Background(), 0, router)
	var conflict *RouterRevisionConflictError
	if !errors.As(err, &conflict) || conflict.Expected != 0 || conflict.Actual != 1 {
		t.Fatalf("revision conflict = %T %v", err, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != routerMetadataFileMode {
		t.Fatalf("metadata mode = %o, want %o", got, routerMetadataFileMode)
	}
}

func TestFileRouterMetadataStoreConcurrentReplaceHasOneWinner(t *testing.T) {
	store, err := NewFileRouterMetadataStore(filepath.Join(t.TempDir(), "metadata.json"))
	if err != nil {
		t.Fatalf("NewFileRouterMetadataStore() error = %v", err)
	}
	if _, err = store.ReplaceRouterMetadata(context.Background(), 0, routerStorageTestConfig()); err != nil {
		t.Fatalf("initial ReplaceRouterMetadata() error = %v", err)
	}

	const writers = 12
	var wg sync.WaitGroup
	var successes int
	var conflicts int
	var resultMu sync.Mutex
	for index := 0; index < writers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			router := routerStorageTestConfig()
			router.ModelGroups[0].PublicModel = "model-" + string(rune('a'+index))
			_, errReplace := store.ReplaceRouterMetadata(context.Background(), 1, router)
			resultMu.Lock()
			defer resultMu.Unlock()
			switch {
			case errReplace == nil:
				successes++
			case errors.Is(errReplace, ErrRouterRevisionConflict):
				conflicts++
			default:
				t.Errorf("ReplaceRouterMetadata() error = %v", errReplace)
			}
		}(index)
	}
	wg.Wait()
	if successes != 1 || conflicts != writers-1 {
		t.Fatalf("successes = %d, conflicts = %d", successes, conflicts)
	}
	loaded, err := store.LoadRouterMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadRouterMetadata() error = %v", err)
	}
	if loaded.Revision != 2 {
		t.Fatalf("revision = %d, want 2", loaded.Revision)
	}
}

func TestEncryptedFileRouterSecretStoreRoundTripAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router", "secrets.json")
	key := bytes.Repeat([]byte{0x21}, 32)
	store, err := NewEncryptedFileRouterSecretStore(path, key)
	if err != nil {
		t.Fatalf("NewEncryptedFileRouterSecretStore() error = %v", err)
	}
	const reference = "router/upstreams/primary"
	secret := []byte("secret-fixture-do-not-persist")

	metadata, err := store.PutRouterSecret(context.Background(), reference, secret)
	if err != nil {
		t.Fatalf("PutRouterSecret() error = %v", err)
	}
	if !metadata.Configured || metadata.UpdatedAt.IsZero() {
		t.Fatalf("metadata = %#v", metadata)
	}
	secret[0] = 'X'

	rawFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if bytes.Contains(rawFile, []byte("secret-fixture-do-not-persist")) {
		t.Fatalf("plaintext secret found in encrypted file: %s", rawFile)
	}
	var serialized map[string]any
	if err = json.Unmarshal(rawFile, &serialized); err != nil {
		t.Fatalf("secret file is not JSON: %v", err)
	}
	resolved, err := store.ResolveUpstreamSecret(context.Background(), reference)
	if err != nil {
		t.Fatalf("ResolveUpstreamSecret() error = %v", err)
	}
	if string(resolved) != "secret-fixture-do-not-persist" {
		t.Fatalf("resolved secret = %q", resolved)
	}
	resolved[0] = 'X'
	resolvedAgain, err := store.ResolveUpstreamSecret(context.Background(), reference)
	if err != nil {
		t.Fatalf("ResolveUpstreamSecret(second) error = %v", err)
	}
	if string(resolvedAgain) != "secret-fixture-do-not-persist" {
		t.Fatalf("second resolved secret = %q", resolvedAgain)
	}

	wrongKeyStore, err := NewEncryptedFileRouterSecretStore(path, bytes.Repeat([]byte{0x22}, 32))
	if err != nil {
		t.Fatalf("NewEncryptedFileRouterSecretStore(wrong key) error = %v", err)
	}
	if _, err = wrongKeyStore.ResolveUpstreamSecret(context.Background(), reference); err == nil || strings.Contains(err.Error(), "secret-fixture") {
		t.Fatalf("wrong-key error = %v", err)
	}

	if err = store.DeleteRouterSecret(context.Background(), reference); err != nil {
		t.Fatalf("DeleteRouterSecret() error = %v", err)
	}
	metadata, err = store.RouterSecretMetadata(context.Background(), reference)
	if err != nil {
		t.Fatalf("RouterSecretMetadata() error = %v", err)
	}
	if metadata.Configured {
		t.Fatalf("deleted secret metadata = %#v", metadata)
	}
	if _, err = store.ResolveUpstreamSecret(context.Background(), reference); !errors.Is(err, ErrRouterSecretNotFound) {
		t.Fatalf("ResolveUpstreamSecret(deleted) error = %v", err)
	}
}

func TestEncryptedFileRouterSecretStoreBindsCiphertextToReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	store, err := NewEncryptedFileRouterSecretStore(path, bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatalf("NewEncryptedFileRouterSecretStore() error = %v", err)
	}
	if _, err = store.PutRouterSecret(context.Background(), "router/upstreams/a", []byte("secret-a")); err != nil {
		t.Fatalf("PutRouterSecret() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var document encryptedRouterSecretFile
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	document.Entries["router/upstreams/b"] = document.Entries["router/upstreams/a"]
	mutated, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err = os.WriteFile(path, mutated, routerMetadataFileMode); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err = store.ResolveUpstreamSecret(context.Background(), "router/upstreams/b"); err == nil {
		t.Fatal("ciphertext unexpectedly decrypted under a different reference")
	}
}

func TestParseRouterMasterKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, 32)
	for name, value := range map[string]string{
		"standard base64": base64.StdEncoding.EncodeToString(key),
		"raw base64":      base64.RawStdEncoding.EncodeToString(key),
		"hex":             strings.Repeat("41", 32),
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := ParseRouterMasterKey(value)
			if err != nil {
				t.Fatalf("ParseRouterMasterKey() error = %v", err)
			}
			if !bytes.Equal(parsed, key) {
				t.Fatalf("parsed key differs")
			}
		})
	}
	for _, value := range []string{"", "short", base64.StdEncoding.EncodeToString([]byte("too-short"))} {
		if _, err := ParseRouterMasterKey(value); err == nil {
			t.Fatalf("invalid key %q unexpectedly passed", value)
		}
	}
}

func TestFileRouterAuditSinkWritesAllowlistedEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router", "audit.jsonl")
	sink, err := NewFileRouterAuditSink(path)
	if err != nil {
		t.Fatalf("NewFileRouterAuditSink() error = %v", err)
	}
	event := RouterAuditEvent{
		Actor:         "127.0.0.1",
		Action:        "patch",
		ResourceType:  "upstream",
		ResourceID:    "primary",
		Revision:      4,
		Outcome:       "success",
		ChangedFields: []string{"name", "enabled"},
	}
	if err = sink.WriteRouterAudit(context.Background(), event); err != nil {
		t.Fatalf("WriteRouterAudit() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if bytes.Contains(raw, []byte("secret")) || !bytes.Contains(raw, []byte(`"resource_id":"primary"`)) {
		t.Fatalf("audit file = %s", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != routerMetadataFileMode {
		t.Fatalf("audit mode = %o, want %o", got, routerMetadataFileMode)
	}
}

func routerStorageTestConfig() config.RouterConfig {
	return config.RouterConfig{
		Upstreams: []config.RouterUpstream{
			{
				ID:       "primary",
				Name:     "Primary",
				Protocol: config.RouterProtocolOpenAIResponses,
				BaseURL:  "https://primary.example.com/v1",
				Headers:  map[string]string{"X-Test": "value"},
				Capabilities: config.RouterCapabilities{
					Endpoints: []string{config.RouterEndpointResponses},
					Streaming: true,
				},
			},
		},
		ModelGroups: []config.RouterModelGroup{
			{
				ID:          "public-model",
				PublicModel: "public-model",
				Capability:  config.RouterCapabilityText,
				Routes: []config.RouterRoute{
					{
						ID:            "public-model-primary",
						UpstreamID:    "primary",
						UpstreamModel: "upstream-model",
						Priority:      100,
						Weight:        100,
					},
				},
			},
		},
	}
}
