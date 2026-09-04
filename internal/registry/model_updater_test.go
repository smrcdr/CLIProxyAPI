package registry

import "testing"

func TestLoadModelsFromBytesRetainsAstraOverride(t *testing.T) {
	original := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

	if err := loadModelsFromBytes([]byte(`{"codex-pro":[]}`), "test"); err != nil {
		t.Fatalf("loadModelsFromBytes() error = %v", err)
	}
	if got := LookupStaticModelInfo("gpt-6-astra"); got == nil {
		t.Fatal("LookupStaticModelInfo(gpt-6-astra) = nil after catalog refresh")
	}
}
