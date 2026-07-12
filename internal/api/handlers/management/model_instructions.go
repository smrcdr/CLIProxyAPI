package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func validateModelInstruction(instruction config.ModelInstruction) (config.ModelInstruction, bool) {
	instruction.Mode = strings.ToLower(strings.TrimSpace(instruction.Mode))
	if instruction.Mode == "" {
		instruction.Mode = "prepend"
	}
	if instruction.Mode != "prepend" && instruction.Mode != "append" && instruction.Mode != "override" {
		return config.ModelInstruction{}, false
	}
	if instruction.Enabled && strings.TrimSpace(instruction.Prompt) == "" {
		return config.ModelInstruction{}, false
	}
	return instruction, true
}

func validateModelInstructions(items map[string]config.ModelInstruction) (map[string]config.ModelInstruction, bool) {
	validated := make(map[string]config.ModelInstruction, len(items))
	for model, instruction := range items {
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, false
		}
		instruction, ok := validateModelInstruction(instruction)
		if !ok {
			return nil, false
		}
		validated[model] = instruction
	}
	return validated, true
}

func (h *Handler) GetModelInstructions(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusOK, gin.H{"model-instructions": map[string]config.ModelInstruction{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"model-instructions": h.cfg.ModelInstructions})
}

func (h *Handler) PutModelInstructions(c *gin.Context) {
	var body struct {
		Instructions map[string]config.ModelInstruction `json:"model-instructions"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Instructions == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model-instructions"})
		return
	}
	validated, ok := validateModelInstructions(body.Instructions)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model-instructions"})
		return
	}
	h.mu.Lock()
	h.cfg.ModelInstructions = validated
	h.persistLocked(c)
	h.mu.Unlock()
}

func (h *Handler) PatchModelInstructions(c *gin.Context) {
	var body map[string]*config.ModelInstruction
	if err := c.ShouldBindJSON(&body); err != nil || body == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model-instructions patch"})
		return
	}
	h.mu.Lock()
	if h.cfg.ModelInstructions == nil {
		h.cfg.ModelInstructions = make(map[string]config.ModelInstruction)
	}
	for model, instruction := range body {
		model = strings.TrimSpace(model)
		if model == "" {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model name"})
			return
		}
		if instruction == nil {
			delete(h.cfg.ModelInstructions, model)
			continue
		}
		validated, ok := validateModelInstruction(*instruction)
		if !ok {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model instruction"})
			return
		}
		h.cfg.ModelInstructions[model] = validated
	}
	h.persistLocked(c)
	h.mu.Unlock()
}
