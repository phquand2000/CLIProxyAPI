// Package letta provides middleware for injecting Letta agent memory into chat completion requests.
// When enabled via LETTA_ENABLED=true, it intercepts requests to:
// 1. Pre-request: Query Letta agent memory and inject into system prompt
// 2. Post-response: Capture assistant response and update Letta memory (async)
package letta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Config holds configuration for Letta integration
// Option A Architecture: Single Agent with Rich Memory Blocks
type Config struct {
	Enabled   bool   `json:"enabled"`
	ServerURL string `json:"server_url"` // e.g., "http://localhost:8283"
	AgentID   string `json:"agent_id"`   // Single comprehensive agent ID
	Timeout   int    `json:"timeout_ms"` // Timeout for Letta queries in milliseconds
}

// Client handles communication with Letta server
type Client struct {
	config     Config
	httpClient *http.Client
	mu         sync.RWMutex
}

// NewClient creates a new Letta client
func NewClient(cfg Config) *Client {
	timeout := time.Duration(cfg.Timeout) * time.Millisecond
	if timeout == 0 {
		timeout = 300 * time.Millisecond // Default 300ms timeout
	}
	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// MemoryBlock represents a Letta memory block
type MemoryBlock struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// GetMemory retrieves memory blocks from Letta agent
func (c *Client) GetMemory(ctx context.Context) ([]MemoryBlock, error) {
	if !c.config.Enabled || c.config.AgentID == "" {
		return nil, nil
	}

	// Letta API: GET /v1/agents/{agent_id} returns agent with memory.blocks
	url := fmt.Sprintf("%s/v1/agents/%s", c.config.ServerURL, c.config.AgentID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("letta returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Parse memory blocks from agent response (memory.blocks)
	var blocks []MemoryBlock
	blocksJSON := gjson.GetBytes(body, "memory.blocks")
	if blocksJSON.IsArray() {
		for _, block := range blocksJSON.Array() {
			blocks = append(blocks, MemoryBlock{
				Label: block.Get("label").String(),
				Value: block.Get("value").String(),
			})
		}
	}

	return blocks, nil
}

// UpdateMemory sends a message to Letta agent to update its memory
func (c *Client) UpdateMemory(ctx context.Context, userMessage, assistantResponse string) error {
	if !c.config.Enabled || c.config.AgentID == "" {
		return nil
	}

	// Create a summary message for Letta to process
	summaryMessage := fmt.Sprintf(
		"[Memory Update] User asked: %s\n\nAssistant responded (summary): %s",
		truncate(userMessage, 500),
		truncate(assistantResponse, 1000),
	)

	url := fmt.Sprintf("%s/v1/agents/%s/messages", c.config.ServerURL, c.config.AgentID)
	payload := map[string]interface{}{
		"messages": []map[string]string{
			{"role": "user", "content": summaryMessage},
		},
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// FormatMemoryForInjection formats memory blocks for system prompt injection
func FormatMemoryForInjection(blocks []MemoryBlock) string {
	if len(blocks) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n--- Agent Memory Context ---\n")
	sb.WriteString("The following is context from your persistent memory. Use it to maintain continuity:\n\n")

	for _, block := range blocks {
		if block.Value != "" {
			sb.WriteString(fmt.Sprintf("[%s]\n%s\n\n", block.Label, block.Value))
		}
	}

	sb.WriteString("--- End Memory Context ---\n")
	return sb.String()
}

// truncate truncates a string to maxLen characters
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// LoadConfigFromEnv loads Letta configuration from environment variables
// Option A: Single agent architecture - no multi-agent routing needed
func LoadConfigFromEnv() Config {
	cfg := Config{
		Enabled:   os.Getenv("LETTA_ENABLED") == "true",
		ServerURL: os.Getenv("LETTA_SERVER_URL"),
		AgentID:   os.Getenv("LETTA_AGENT_ID"),
		Timeout:   300,
	}

	// Default values
	if cfg.ServerURL == "" {
		cfg.ServerURL = "http://localhost:8283"
	}

	return cfg
}

// Global client instance (set by NewMiddleware)
var globalClient *Client

// NewMiddleware creates the Gin middleware for memory injection.
// Option A Architecture: Single agent with rich memory blocks
// Returns nil if Letta is not enabled.
func NewMiddleware() gin.HandlerFunc {
	cfg := LoadConfigFromEnv()
	if !cfg.Enabled {
		fmt.Fprintf(os.Stderr, "[letta] Memory injection disabled (set LETTA_ENABLED=true to enable)\n")
		return nil
	}

	globalClient = NewClient(cfg)
	fmt.Fprintf(os.Stderr, "[letta] Memory injection enabled (Option A: Single Agent)\n")
	fmt.Fprintf(os.Stderr, "[letta] Server: %s\n", cfg.ServerURL)
	fmt.Fprintf(os.Stderr, "[letta] Agent: %s\n", cfg.AgentID)

	return memoryInjectionMiddleware(globalClient)
}

// memoryInjectionMiddleware is the Gin middleware that handles memory injection
func memoryInjectionMiddleware(client *Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only intercept chat completions
		if !strings.Contains(c.Request.URL.Path, "/chat/completions") {
			c.Next()
			return
		}

		// Skip if client is not enabled
		if client == nil || !client.config.Enabled {
			c.Next()
			return
		}

		// Read the original body
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Next()
			return
		}
		c.Request.Body.Close()

		// Try to inject memory into the request
		modifiedBody := injectMemoryIntoRequest(c.Request.Context(), client, bodyBytes)

		// Replace the body with the modified one
		c.Request.Body = io.NopCloser(bytes.NewReader(modifiedBody))
		c.Request.ContentLength = int64(len(modifiedBody))

		// Store original user message for post-response update
		userMessage := extractUserMessage(bodyBytes)
		c.Set("letta_user_message", userMessage)

		// Create a response writer wrapper to capture the response
		rw := &responseCapture{
			ResponseWriter: c.Writer,
			body:           &bytes.Buffer{},
		}
		c.Writer = rw

		// Continue to next handler
		c.Next()

		// Post-response: update memory asynchronously
		if userMessage != "" && rw.body.Len() > 0 {
			go func(cl *Client, userMsg string, respBody []byte) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				assistantResponse := extractAssistantResponse(respBody)
				if assistantResponse != "" {
					if err := cl.UpdateMemory(ctx, userMsg, assistantResponse); err != nil {
						fmt.Fprintf(os.Stderr, "[letta] memory update error: %v\n", err)
					}
				}
			}(client, userMessage, rw.body.Bytes())
		}
	}
}

// responseCapture wraps gin.ResponseWriter to capture response body
type responseCapture struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (rc *responseCapture) Write(data []byte) (int, error) {
	rc.body.Write(data)
	return rc.ResponseWriter.Write(data)
}

// injectMemoryIntoRequest modifies the request body to include memory context
func injectMemoryIntoRequest(ctx context.Context, client *Client, body []byte) []byte {
	// Query Letta for memory with timeout
	queryCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()

	blocks, err := client.GetMemory(queryCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[letta] memory query error (continuing without): %v\n", err)
		return body
	}

	if len(blocks) == 0 {
		return body
	}

	memoryContext := FormatMemoryForInjection(blocks)

	// Find and modify the system message
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}

	// Look for existing system message
	systemIndex := -1
	for i, msg := range messages.Array() {
		if msg.Get("role").String() == "system" {
			systemIndex = i
			break
		}
	}

	var modifiedBody []byte
	if systemIndex >= 0 {
		// Append memory to existing system message
		existingContent := gjson.GetBytes(body, fmt.Sprintf("messages.%d.content", systemIndex)).String()
		newContent := existingContent + memoryContext
		modifiedBody, _ = sjson.SetBytes(body, fmt.Sprintf("messages.%d.content", systemIndex), newContent)
	} else {
		// Prepend a new system message with memory
		systemMsg := map[string]string{
			"role":    "system",
			"content": "You are an AI assistant." + memoryContext,
		}
		existingMessages := messages.Array()
		newMessages := make([]interface{}, 0, len(existingMessages)+1)
		newMessages = append(newMessages, systemMsg)
		for _, msg := range existingMessages {
			newMessages = append(newMessages, msg.Value())
		}
		modifiedBody, _ = sjson.SetBytes(body, "messages", newMessages)
	}

	if modifiedBody == nil {
		return body
	}

	fmt.Fprintf(os.Stderr, "[letta] injected %d memory blocks\n", len(blocks))
	return modifiedBody
}

// extractUserMessage extracts the last user message from the request
func extractUserMessage(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}

	// Get the last user message
	var lastUserMsg string
	for _, msg := range messages.Array() {
		if msg.Get("role").String() == "user" {
			lastUserMsg = msg.Get("content").String()
		}
	}
	return lastUserMsg
}

// extractAssistantResponse extracts assistant response from the response body
func extractAssistantResponse(body []byte) string {
	// For non-streaming responses
	content := gjson.GetBytes(body, "choices.0.message.content").String()
	if content != "" {
		return content
	}

	// For streaming responses, try to extract from concatenated chunks
	// This is a simplified approach - streaming responses need more complex handling
	return ""
}
