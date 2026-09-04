package smartrouter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var (
	ErrRouterRevisionConflict = errors.New("router metadata revision conflict")
	ErrRouterSecretNotFound   = errors.New("router secret not found")
)

// RouterMetadataDocument is the complete persisted routing configuration.
// Mutations replace the whole document and advance Revision monotonically.
type RouterMetadataDocument struct {
	Revision  uint64              `json:"revision"`
	UpdatedAt time.Time           `json:"updated_at"`
	Router    config.RouterConfig `json:"router"`
}

// RouterMetadataStore persists complete router documents atomically.
type RouterMetadataStore interface {
	LoadRouterMetadata(context.Context) (RouterMetadataDocument, error)
	ReplaceRouterMetadata(context.Context, uint64, config.RouterConfig) (RouterMetadataDocument, error)
}

// RouterSecretMetadata is the only secret information exposed to management
// callers. Secret values are intentionally absent.
type RouterSecretMetadata struct {
	Configured bool      `json:"configured"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
}

// RouterSecretStore owns encrypted upstream secret material. Reads are for the
// runtime adapter only; management handlers use Put, Delete, and Metadata.
type RouterSecretStore interface {
	PutRouterSecret(context.Context, string, []byte) (RouterSecretMetadata, error)
	DeleteRouterSecret(context.Context, string) error
	RouterSecretMetadata(context.Context, string) (RouterSecretMetadata, error)
	ResolveUpstreamSecret(context.Context, string) ([]byte, error)
}

// RouterAuditEvent contains allowlisted mutation facts only.
type RouterAuditEvent struct {
	Timestamp       time.Time `json:"timestamp"`
	Actor           string    `json:"actor,omitempty"`
	Action          string    `json:"action"`
	ResourceType    string    `json:"resource_type"`
	ResourceID      string    `json:"resource_id,omitempty"`
	Revision        uint64    `json:"revision,omitempty"`
	Outcome         string    `json:"outcome"`
	ChangedFields   []string  `json:"changed_fields,omitempty"`
	FailureCategory string    `json:"failure_category,omitempty"`
}

// RouterAuditSink records masked management events.
type RouterAuditSink interface {
	WriteRouterAudit(context.Context, RouterAuditEvent) error
}

// RouterRevisionConflictError reports optimistic concurrency conflicts without
// retaining request bodies or secret values.
type RouterRevisionConflictError struct {
	Expected uint64
	Actual   uint64
}

func (e *RouterRevisionConflictError) Error() string {
	if e == nil {
		return ErrRouterRevisionConflict.Error()
	}
	return fmt.Sprintf("%s: expected %d, actual %d", ErrRouterRevisionConflict, e.Expected, e.Actual)
}

func (e *RouterRevisionConflictError) Unwrap() error {
	return ErrRouterRevisionConflict
}

func cloneRouterMetadataDocument(source RouterMetadataDocument) RouterMetadataDocument {
	source.Router = cloneRouterConfig(source.Router)
	source.UpdatedAt = source.UpdatedAt.UTC()
	return source
}

func cloneRouterConfig(source config.RouterConfig) config.RouterConfig {
	cloned := (&config.Config{Router: source}).CloneForRuntime()
	if cloned == nil {
		return config.RouterConfig{}
	}
	return cloned.Router
}
