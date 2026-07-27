package smartrouter

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type routerManagementRuntime struct {
	mu         sync.Mutex
	prepareErr error
	commitErr  error
	prepared   []RouterMetadataDocument
	commits    int
	secrets    RouterSecretStore
	checkRef   string
	checkValue string
}

func (r *routerManagementRuntime) PrepareRouterRuntime(ctx context.Context, document RouterMetadataDocument) (RouterRuntimeUpdate, error) {
	r.mu.Lock()
	r.prepared = append(r.prepared, cloneRouterMetadataDocument(document))
	errPrepare := r.prepareErr
	secrets := r.secrets
	checkRef := r.checkRef
	checkValue := r.checkValue
	r.mu.Unlock()
	if errPrepare != nil {
		return nil, errPrepare
	}
	if checkRef != "" {
		value, errResolve := secrets.ResolveUpstreamSecret(ctx, checkRef)
		if errResolve != nil {
			return nil, errResolve
		}
		defer clear(value)
		if string(value) != checkValue {
			return nil, errors.New("runtime observed unexpected secret")
		}
	}
	return routerManagementRuntimeUpdate{runtime: r}, nil
}

type routerManagementRuntimeUpdate struct {
	runtime *routerManagementRuntime
}

func (u routerManagementRuntimeUpdate) CommitRouterRuntime(context.Context) error {
	u.runtime.mu.Lock()
	defer u.runtime.mu.Unlock()
	if u.runtime.commitErr != nil {
		err := u.runtime.commitErr
		u.runtime.commitErr = nil
		return err
	}
	u.runtime.commits++
	return nil
}

type routerManagementAudit struct {
	mu     sync.Mutex
	events []RouterAuditEvent
	err    error
}

func (a *routerManagementAudit) WriteRouterAudit(_ context.Context, event RouterAuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	return a.err
}

func TestRouterManagementServiceReplaceCommitsValidatedDocumentAndSecret(t *testing.T) {
	metadata, secrets := routerManagementTestStores(t)
	initial := routerStorageTestConfig()
	if _, err := metadata.ReplaceRouterMetadata(context.Background(), 0, initial); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}

	const reference = "router/upstreams/primary"
	runtime := &routerManagementRuntime{
		secrets:    secrets,
		checkRef:   reference,
		checkValue: "new-secret",
	}
	audit := &routerManagementAudit{}
	service, err := NewRouterManagementService(metadata, secrets, audit, runtime)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}
	next := routerStorageTestConfig()
	next.Upstreams[0].Name = "Updated"
	next.Upstreams[0].Auth.Type = config.RouterAuthBearer
	next.Upstreams[0].Auth.SecretRef = reference

	document, err := service.Replace(context.Background(), RouterMutation{
		ExpectedRevision: 1,
		Router:           next,
		Secrets: []RouterSecretMutation{
			{Reference: reference, Value: []byte("new-secret")},
		},
		Actor:         "admin",
		Action:        "patch",
		ResourceType:  "upstream",
		ResourceID:    "primary",
		ChangedFields: []string{"name", "auth"},
	})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if document.Revision != 2 || document.Router.Upstreams[0].Name != "Updated" {
		t.Fatalf("document = %#v", document)
	}
	if runtime.commits != 1 || len(runtime.prepared) != 1 || runtime.prepared[0].Revision != 2 {
		t.Fatalf("runtime prepares = %d commits = %d", len(runtime.prepared), runtime.commits)
	}
	if len(audit.events) != 1 || audit.events[0].Revision != 2 || audit.events[0].Outcome != "success" {
		t.Fatalf("audit events = %#v", audit.events)
	}
	resolved, err := secrets.ResolveUpstreamSecret(context.Background(), reference)
	if err != nil {
		t.Fatalf("ResolveUpstreamSecret() error = %v", err)
	}
	defer clear(resolved)
	if string(resolved) != "new-secret" {
		t.Fatalf("resolved secret = %q", resolved)
	}
}

func TestRouterManagementServiceInvalidMutationChangesNothing(t *testing.T) {
	metadata, secrets := routerManagementTestStores(t)
	initial := routerStorageTestConfig()
	if _, err := metadata.ReplaceRouterMetadata(context.Background(), 0, initial); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	const reference = "router/upstreams/primary"
	if _, err := secrets.PutRouterSecret(context.Background(), reference, []byte("old-secret")); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	runtime := &routerManagementRuntime{}
	service, err := NewRouterManagementService(metadata, secrets, nil, runtime)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}
	invalid := routerStorageTestConfig()
	invalid.ModelGroups[0].Routes[0].UpstreamID = "missing"

	_, err = service.Replace(context.Background(), RouterMutation{
		ExpectedRevision: 1,
		Router:           invalid,
		Secrets: []RouterSecretMutation{
			{Reference: reference, Value: []byte("new-secret")},
		},
	})
	if err == nil {
		t.Fatal("Replace() unexpectedly accepted invalid document")
	}
	loaded, err := metadata.LoadRouterMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadRouterMetadata() error = %v", err)
	}
	if loaded.Revision != 1 || loaded.Router.ModelGroups[0].Routes[0].UpstreamID != "primary" {
		t.Fatalf("metadata changed = %#v", loaded)
	}
	resolved, err := secrets.ResolveUpstreamSecret(context.Background(), reference)
	if err != nil {
		t.Fatalf("ResolveUpstreamSecret() error = %v", err)
	}
	defer clear(resolved)
	if string(resolved) != "old-secret" {
		t.Fatalf("secret changed = %q", resolved)
	}
	if len(runtime.prepared) != 0 || runtime.commits != 0 {
		t.Fatalf("runtime prepared invalid document")
	}
}

func TestRouterManagementServicePrepareFailureRestoresSecret(t *testing.T) {
	metadata, secrets := routerManagementTestStores(t)
	initial := routerStorageTestConfig()
	if _, err := metadata.ReplaceRouterMetadata(context.Background(), 0, initial); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	const reference = "router/upstreams/primary"
	if _, err := secrets.PutRouterSecret(context.Background(), reference, []byte("old-secret")); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	runtime := &routerManagementRuntime{prepareErr: errors.New("runtime unavailable")}
	service, err := NewRouterManagementService(metadata, secrets, nil, runtime)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}

	_, err = service.Replace(context.Background(), RouterMutation{
		ExpectedRevision: 1,
		Router:           initial,
		Secrets: []RouterSecretMutation{
			{Reference: reference, Value: []byte("new-secret")},
		},
	})
	if err == nil {
		t.Fatal("Replace() unexpectedly succeeded")
	}
	loaded, err := metadata.LoadRouterMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadRouterMetadata() error = %v", err)
	}
	if loaded.Revision != 1 {
		t.Fatalf("metadata revision = %d, want 1", loaded.Revision)
	}
	resolved, err := secrets.ResolveUpstreamSecret(context.Background(), reference)
	if err != nil {
		t.Fatalf("ResolveUpstreamSecret() error = %v", err)
	}
	defer clear(resolved)
	if string(resolved) != "old-secret" {
		t.Fatalf("secret was not restored: %q", resolved)
	}
}

func TestRouterManagementServiceCommitFailureRollsBackPersistedState(t *testing.T) {
	metadata, secrets := routerManagementTestStores(t)
	initial := routerStorageTestConfig()
	if _, err := metadata.ReplaceRouterMetadata(context.Background(), 0, initial); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	const reference = "router/upstreams/primary"
	if _, err := secrets.PutRouterSecret(context.Background(), reference, []byte("old-secret")); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	runtime := &routerManagementRuntime{commitErr: errors.New("commit failed")}
	service, err := NewRouterManagementService(metadata, secrets, nil, runtime)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}
	next := routerStorageTestConfig()
	next.Upstreams[0].Name = "Should Roll Back"

	_, err = service.Replace(context.Background(), RouterMutation{
		ExpectedRevision: 1,
		Router:           next,
		Secrets: []RouterSecretMutation{
			{Reference: reference, Value: []byte("new-secret")},
		},
	})
	if err == nil {
		t.Fatal("Replace() unexpectedly succeeded")
	}
	loaded, err := metadata.LoadRouterMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadRouterMetadata() error = %v", err)
	}
	if loaded.Revision != 3 || loaded.Router.Upstreams[0].Name != initial.Upstreams[0].Name {
		t.Fatalf("rolled-back metadata = %#v", loaded)
	}
	resolved, err := secrets.ResolveUpstreamSecret(context.Background(), reference)
	if err != nil {
		t.Fatalf("ResolveUpstreamSecret() error = %v", err)
	}
	defer clear(resolved)
	if string(resolved) != "old-secret" {
		t.Fatalf("secret was not restored: %q", resolved)
	}
	if runtime.commits != 1 || len(runtime.prepared) != 2 || runtime.prepared[1].Revision != 3 {
		t.Fatalf("runtime commits = %d, prepared = %#v", runtime.commits, runtime.prepared)
	}

	afterRollback := routerStorageTestConfig()
	afterRollback.Upstreams[0].Name = "After Rollback"
	document, err := service.Replace(context.Background(), RouterMutation{
		ExpectedRevision: 3,
		Router:           afterRollback,
	})
	if err != nil {
		t.Fatalf("Replace() after rollback error = %v", err)
	}
	if document.Revision != 4 || runtime.commits != 2 {
		t.Fatalf("post-rollback document = %#v, runtime commits = %d", document, runtime.commits)
	}
}

func TestRouterManagementServiceAuditFailureReportsCommittedDocument(t *testing.T) {
	metadata, secrets := routerManagementTestStores(t)
	initial := routerStorageTestConfig()
	if _, err := metadata.ReplaceRouterMetadata(context.Background(), 0, initial); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	runtime := &routerManagementRuntime{}
	audit := &routerManagementAudit{err: errors.New("audit disk full")}
	service, err := NewRouterManagementService(metadata, secrets, audit, runtime)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}
	next := routerStorageTestConfig()
	next.Upstreams[0].Name = "Committed"

	document, err := service.Replace(context.Background(), RouterMutation{
		ExpectedRevision: 1,
		Router:           next,
		Actor:            "admin",
		Action:           "patch",
		ResourceType:     "upstream",
		ResourceID:       "primary",
	})
	var postCommit *RouterPostCommitError
	if !errors.As(err, &postCommit) {
		t.Fatalf("Replace() error = %T %v", err, err)
	}
	if document.Revision != 2 || runtime.commits != 1 {
		t.Fatalf("committed document = %#v, runtime commits = %d", document, runtime.commits)
	}
	loaded, errLoad := metadata.LoadRouterMetadata(context.Background())
	if errLoad != nil {
		t.Fatalf("LoadRouterMetadata() error = %v", errLoad)
	}
	if loaded.Revision != 2 || loaded.Router.Upstreams[0].Name != "Committed" {
		t.Fatalf("persisted document = %#v", loaded)
	}
}

func TestRouterManagementServiceRevisionConflictDoesNotTouchSecrets(t *testing.T) {
	metadata, secrets := routerManagementTestStores(t)
	initial := routerStorageTestConfig()
	if _, err := metadata.ReplaceRouterMetadata(context.Background(), 0, initial); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	const reference = "router/upstreams/primary"
	if _, err := secrets.PutRouterSecret(context.Background(), reference, []byte("old-secret")); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	service, err := NewRouterManagementService(metadata, secrets, nil, nil)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}

	_, err = service.Replace(context.Background(), RouterMutation{
		ExpectedRevision: 0,
		Router:           initial,
		Secrets: []RouterSecretMutation{
			{Reference: reference, Value: []byte("new-secret")},
		},
	})
	if !errors.Is(err, ErrRouterRevisionConflict) {
		t.Fatalf("Replace() error = %v", err)
	}
	resolved, err := secrets.ResolveUpstreamSecret(context.Background(), reference)
	if err != nil {
		t.Fatalf("ResolveUpstreamSecret() error = %v", err)
	}
	defer clear(resolved)
	if !bytes.Equal(resolved, []byte("old-secret")) {
		t.Fatalf("secret changed = %q", resolved)
	}
}

func TestRouterManagementServiceConcurrentReplaceDoesNotLoseUpdates(t *testing.T) {
	metadata, secrets := routerManagementTestStores(t)
	initial := routerStorageTestConfig()
	if _, err := metadata.ReplaceRouterMetadata(context.Background(), 0, initial); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	service, err := NewRouterManagementService(metadata, secrets, nil, nil)
	if err != nil {
		t.Fatalf("NewRouterManagementService() error = %v", err)
	}

	results := make(chan error, 2)
	start := make(chan struct{})
	for _, name := range []string{"First", "Second"} {
		name := name
		go func() {
			<-start
			next := routerStorageTestConfig()
			next.Upstreams[0].Name = name
			_, errReplace := service.Replace(context.Background(), RouterMutation{
				ExpectedRevision: 1,
				Router:           next,
			})
			results <- errReplace
		}()
	}
	close(start)

	var successes, conflicts int
	for range 2 {
		errResult := <-results
		switch {
		case errResult == nil:
			successes++
		case errors.Is(errResult, ErrRouterRevisionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent Replace() error = %v", errResult)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes = %d, conflicts = %d", successes, conflicts)
	}
	document, err := metadata.LoadRouterMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadRouterMetadata() error = %v", err)
	}
	if document.Revision != 2 {
		t.Fatalf("metadata revision = %d, want 2", document.Revision)
	}
	if document.Router.Upstreams[0].Name != "First" && document.Router.Upstreams[0].Name != "Second" {
		t.Fatalf("metadata name = %q", document.Router.Upstreams[0].Name)
	}
}

func routerManagementTestStores(t *testing.T) (*FileRouterMetadataStore, *EncryptedFileRouterSecretStore) {
	t.Helper()
	directory := t.TempDir()
	metadata, err := NewFileRouterMetadataStore(filepath.Join(directory, "metadata.json"))
	if err != nil {
		t.Fatalf("NewFileRouterMetadataStore() error = %v", err)
	}
	secrets, err := NewEncryptedFileRouterSecretStore(filepath.Join(directory, "secrets.json"), bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatalf("NewEncryptedFileRouterSecretStore() error = %v", err)
	}
	return metadata, secrets
}
