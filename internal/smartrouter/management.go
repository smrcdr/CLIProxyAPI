package smartrouter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// RouterRuntimeUpdate is a fully prepared, non-mutating runtime update.
// Commit is called only after metadata and secret persistence succeeds.
type RouterRuntimeUpdate interface {
	CommitRouterRuntime(context.Context) error
}

// RouterRuntimePreparer validates runtime-only dependencies such as secret
// resolution without changing the active selector or auth registrations.
type RouterRuntimePreparer interface {
	PrepareRouterRuntime(context.Context, RouterMetadataDocument) (RouterRuntimeUpdate, error)
}

// RouterSecretMutation is an internal write-only secret operation accompanying
// one metadata mutation.
type RouterSecretMutation struct {
	Reference string
	Value     []byte
	Delete    bool
}

// RouterMutation describes the safe audit facts for one complete replacement.
type RouterMutation struct {
	ExpectedRevision uint64
	Router           config.RouterConfig
	Secrets          []RouterSecretMutation
	Actor            string
	Action           string
	ResourceType     string
	ResourceID       string
	ChangedFields    []string
}

// RouterManagementService serializes local mutations and coordinates
// validation, persistence, runtime preparation, commit, and masked audit.
type RouterManagementService struct {
	metadata RouterMetadataStore
	secrets  RouterSecretStore
	audit    RouterAuditSink
	runtime  RouterRuntimePreparer
	mu       sync.Mutex
}

func NewRouterManagementService(metadata RouterMetadataStore, secrets RouterSecretStore, audit RouterAuditSink, runtime RouterRuntimePreparer) (*RouterManagementService, error) {
	if metadata == nil {
		return nil, errors.New("create router management service: metadata store is required")
	}
	return &RouterManagementService{
		metadata: metadata,
		secrets:  secrets,
		audit:    audit,
		runtime:  runtime,
	}, nil
}

func (s *RouterManagementService) Load(ctx context.Context) (RouterMetadataDocument, error) {
	if s == nil || s.metadata == nil {
		return RouterMetadataDocument{}, errors.New("load router management document: service is not configured")
	}
	return s.metadata.LoadRouterMetadata(ctx)
}

func (s *RouterManagementService) SecretMetadata(ctx context.Context, reference string) (RouterSecretMetadata, error) {
	if s == nil {
		return RouterSecretMetadata{}, errors.New("read router secret metadata: service is not configured")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" || s.secrets == nil {
		return RouterSecretMetadata{}, nil
	}
	return s.secrets.RouterSecretMetadata(ctx, reference)
}

// Validate compiles a complete candidate without persisting or applying it.
func (s *RouterManagementService) Validate(router config.RouterConfig, revision uint64) error {
	_, err := normalizeRouterManagementConfig(router, revision)
	return err
}

func normalizeRouterManagementConfig(router config.RouterConfig, revision uint64) (config.RouterConfig, error) {
	candidate := &config.Config{
		ServiceRole: config.ServiceRoleRouter,
		Router:      cloneRouterConfig(router),
	}
	if _, err := CompileSnapshot(candidate, revision); err != nil {
		return config.RouterConfig{}, fmt.Errorf("validate router management document: %w", err)
	}
	if err := candidate.NormalizeAndValidateRouter(); err != nil {
		return config.RouterConfig{}, fmt.Errorf("validate router management document: %w", err)
	}
	return cloneRouterConfig(candidate.Router), nil
}

func (s *RouterManagementService) Replace(ctx context.Context, mutation RouterMutation) (RouterMetadataDocument, error) {
	if err := contextError(ctx); err != nil {
		return RouterMetadataDocument{}, err
	}
	if s == nil || s.metadata == nil {
		return RouterMetadataDocument{}, errors.New("replace router management document: service is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	current, errLoad := s.metadata.LoadRouterMetadata(ctx)
	if errLoad != nil {
		return RouterMetadataDocument{}, fmt.Errorf("replace router management document: %w", errLoad)
	}
	if current.Revision != mutation.ExpectedRevision {
		return RouterMetadataDocument{}, &RouterRevisionConflictError{
			Expected: mutation.ExpectedRevision,
			Actual:   current.Revision,
		}
	}
	nextRevision := current.Revision + 1
	normalizedRouter, errValidate := normalizeRouterManagementConfig(mutation.Router, nextRevision)
	if errValidate != nil {
		return RouterMetadataDocument{}, errValidate
	}
	mutation.Router = normalizedRouter

	secretBackups, errSecrets := s.applySecretWrites(ctx, mutation.Secrets)
	if errSecrets != nil {
		return RouterMetadataDocument{}, errSecrets
	}
	rollbackSecrets := func() error {
		return s.restoreSecrets(ctx, secretBackups)
	}

	prepared, errPrepare := s.prepareRuntime(ctx, RouterMetadataDocument{
		Revision: nextRevision,
		Router:   cloneRouterConfig(mutation.Router),
	})
	if errPrepare != nil {
		return RouterMetadataDocument{}, errors.Join(
			fmt.Errorf("prepare router runtime: %w", errPrepare),
			rollbackSecrets(),
		)
	}

	persisted, errPersist := s.metadata.ReplaceRouterMetadata(ctx, current.Revision, mutation.Router)
	if errPersist != nil {
		return RouterMetadataDocument{}, errors.Join(
			fmt.Errorf("persist router management document: %w", errPersist),
			rollbackSecrets(),
		)
	}
	if errDeletes := s.applySecretDeletes(ctx, mutation.Secrets); errDeletes != nil {
		return RouterMetadataDocument{}, s.rollback(ctx, current, persisted, secretBackups, fmt.Errorf("delete router secret: %w", errDeletes))
	}
	if prepared != nil {
		if errCommit := prepared.CommitRouterRuntime(ctx); errCommit != nil {
			return RouterMetadataDocument{}, s.rollback(ctx, current, persisted, secretBackups, fmt.Errorf("commit router runtime: %w", errCommit))
		}
	}

	if s.audit != nil {
		errAudit := s.audit.WriteRouterAudit(ctx, RouterAuditEvent{
			Actor:         strings.TrimSpace(mutation.Actor),
			Action:        strings.TrimSpace(mutation.Action),
			ResourceType:  strings.TrimSpace(mutation.ResourceType),
			ResourceID:    strings.TrimSpace(mutation.ResourceID),
			Revision:      persisted.Revision,
			Outcome:       "success",
			ChangedFields: append([]string(nil), mutation.ChangedFields...),
		})
		if errAudit != nil {
			return persisted, &RouterPostCommitError{Cause: fmt.Errorf("write router audit: %w", errAudit)}
		}
	}
	return persisted, nil
}

// RouterPostCommitError reports a non-runtime failure after a mutation became
// active. HTTP callers can return the successful document with a warning.
type RouterPostCommitError struct {
	Cause error
}

func (e *RouterPostCommitError) Error() string {
	if e == nil || e.Cause == nil {
		return "router mutation committed with a post-commit error"
	}
	return "router mutation committed with a post-commit error: " + e.Cause.Error()
}

func (e *RouterPostCommitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type routerSecretBackup struct {
	reference  string
	configured bool
	value      []byte
}

func (s *RouterManagementService) applySecretWrites(ctx context.Context, mutations []RouterSecretMutation) ([]routerSecretBackup, error) {
	if len(mutations) == 0 {
		return nil, nil
	}
	if s.secrets == nil {
		return nil, errors.New("update router secrets: secret store is not configured")
	}
	backups := make([]routerSecretBackup, 0, len(mutations))
	seen := make(map[string]struct{}, len(mutations))
	for index := range mutations {
		mutation := mutations[index]
		reference := strings.TrimSpace(mutation.Reference)
		if reference == "" {
			return backups, errors.Join(
				errors.New("update router secrets: reference is empty"),
				s.restoreSecrets(ctx, backups),
			)
		}
		if _, duplicate := seen[reference]; duplicate {
			return backups, errors.Join(
				fmt.Errorf("update router secrets: duplicate reference %q", reference),
				s.restoreSecrets(ctx, backups),
			)
		}
		seen[reference] = struct{}{}
		metadata, errMetadata := s.secrets.RouterSecretMetadata(ctx, reference)
		if errMetadata != nil {
			return backups, errors.Join(
				fmt.Errorf("read router secret metadata: %w", errMetadata),
				s.restoreSecrets(ctx, backups),
			)
		}
		backup := routerSecretBackup{reference: reference, configured: metadata.Configured}
		if metadata.Configured {
			value, errResolve := s.secrets.ResolveUpstreamSecret(ctx, reference)
			if errResolve != nil {
				return backups, errors.Join(
					fmt.Errorf("backup router secret: %w", errResolve),
					s.restoreSecrets(ctx, backups),
				)
			}
			backup.value = value
		}
		backups = append(backups, backup)
		if mutation.Delete {
			continue
		}
		if len(mutation.Value) == 0 {
			return backups, errors.Join(
				errors.New("update router secrets: value is empty"),
				s.restoreSecrets(ctx, backups),
			)
		}
		if _, errPut := s.secrets.PutRouterSecret(ctx, reference, bytes.Clone(mutation.Value)); errPut != nil {
			return backups, errors.Join(
				fmt.Errorf("update router secret: %w", errPut),
				s.restoreSecrets(ctx, backups),
			)
		}
	}
	return backups, nil
}

func (s *RouterManagementService) applySecretDeletes(ctx context.Context, mutations []RouterSecretMutation) error {
	for index := range mutations {
		if !mutations[index].Delete {
			continue
		}
		if errDelete := s.secrets.DeleteRouterSecret(ctx, strings.TrimSpace(mutations[index].Reference)); errDelete != nil {
			return errDelete
		}
	}
	return nil
}

func (s *RouterManagementService) restoreSecrets(ctx context.Context, backups []routerSecretBackup) error {
	if len(backups) == 0 || s.secrets == nil {
		return nil
	}
	var restoreErrors []error
	for index := len(backups) - 1; index >= 0; index-- {
		backup := backups[index]
		switch {
		case backup.configured:
			if _, errPut := s.secrets.PutRouterSecret(ctx, backup.reference, backup.value); errPut != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore router secret %q: %w", backup.reference, errPut))
			}
		default:
			if errDelete := s.secrets.DeleteRouterSecret(ctx, backup.reference); errDelete != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("remove router secret %q: %w", backup.reference, errDelete))
			}
		}
		clear(backup.value)
	}
	return errors.Join(restoreErrors...)
}

func (s *RouterManagementService) prepareRuntime(ctx context.Context, document RouterMetadataDocument) (RouterRuntimeUpdate, error) {
	if s.runtime == nil {
		return nil, nil
	}
	prepared, err := s.runtime.PrepareRouterRuntime(ctx, cloneRouterMetadataDocument(document))
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

func (s *RouterManagementService) rollback(ctx context.Context, current, persisted RouterMetadataDocument, secretBackups []routerSecretBackup, cause error) error {
	rollbackContext := context.WithoutCancel(ctx)
	rolledBack, errMetadata := s.metadata.ReplaceRouterMetadata(rollbackContext, persisted.Revision, current.Router)
	errSecrets := s.restoreSecrets(rollbackContext, secretBackups)
	var errRuntime error
	if errMetadata == nil {
		prepared, errPrepare := s.prepareRuntime(rollbackContext, rolledBack)
		if errPrepare != nil {
			errRuntime = fmt.Errorf("prepare rolled-back router runtime: %w", errPrepare)
		} else if prepared != nil {
			if errCommit := prepared.CommitRouterRuntime(rollbackContext); errCommit != nil {
				errRuntime = fmt.Errorf("commit rolled-back router runtime: %w", errCommit)
			}
		}
	}
	return errors.Join(cause, errMetadata, errSecrets, errRuntime)
}
