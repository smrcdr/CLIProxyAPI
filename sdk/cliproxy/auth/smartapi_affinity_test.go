package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSmartAPIAffinityCommitsOnlyAfterSuccessAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smartapi-affinity.json")
	selector := NewSmartAPIAffinitySelector(&RoundRobinSelector{}, path, time.Hour)
	headers := make(http.Header)
	headers.Set(smartAPIAffinityHeader, "client-pseudonym")
	auths := []*Auth{{ID: "auth-a", Provider: "claude"}, {ID: "auth-b", Provider: "claude"}}
	opts := cliproxyexecutor.Options{Headers: headers}

	first, errPick := selector.Pick(context.Background(), "claude", "model-a", opts, auths)
	if errPick != nil {
		t.Fatalf("first Pick: %v", errPick)
	}
	second, errPick := selector.Pick(context.Background(), "claude", "model-a", opts, auths)
	if errPick != nil {
		t.Fatalf("second Pick: %v", errPick)
	}
	if second.ID != first.ID {
		t.Fatalf("rendezvous pick changed from %s to %s", first.ID, second.ID)
	}

	selector.Commit(headers, "claude", "auth-b")
	pinned, errPick := selector.Pick(context.Background(), "claude", "another-model", opts, auths)
	if errPick != nil {
		t.Fatalf("pinned Pick: %v", errPick)
	}
	if pinned.ID != "auth-b" {
		t.Fatalf("pinned auth = %s, want auth-b", pinned.ID)
	}

	stored, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read store: %v", errRead)
	}
	if string(stored) == "" || strings.Contains(string(stored), "client-pseudonym") || strings.Contains(string(stored), "auth-b") {
		t.Fatalf("store leaked a raw affinity value: %s", stored)
	}

	restarted := NewSmartAPIAffinitySelector(&RoundRobinSelector{}, path, time.Hour)
	afterRestart, errPick := restarted.Pick(context.Background(), "claude", "model-a", opts, auths)
	if errPick != nil {
		t.Fatalf("restart Pick: %v", errPick)
	}
	if afterRestart.ID != "auth-b" {
		t.Fatalf("persisted auth = %s, want auth-b", afterRestart.ID)
	}
}
