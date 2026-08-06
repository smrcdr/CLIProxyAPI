package api

import (
	"bytes"
	"testing"
)

func TestSmartManagementPageIncludesModelInstructionEditor(t *testing.T) {
	required := [][]byte{
		[]byte(`id="instructionsTab"`),
		[]byte(`id="instructionsView"`),
		[]byte(`id="addInstruction"`),
		[]byte(`id="saveInstructions"`),
		[]byte(`/v0/management`),
		[]byte(`/model-instructions`),
		[]byte(`prepend`),
		[]byte(`append`),
		[]byte(`override`),
	}
	for _, marker := range required {
		if !bytes.Contains(smartManagementHTML, marker) {
			t.Fatalf("smart management page is missing %q", marker)
		}
	}
}
