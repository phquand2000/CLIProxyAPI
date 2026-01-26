// Custom CLIProxyAPI server with Letta memory injection middleware.
// This server intercepts chat completion requests and:
// 1. Pre-request: Queries Letta agent memory and injects into system prompt
// 2. Post-response: Captures assistant response and updates Letta memory (async)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	configaccess "github.com/router-for-me/CLIProxyAPI/v6/internal/access/config_access"
	internallogging "github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/logging"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// init initializes the shared logger setup (matching cmd/server behavior).
func init() {
	internallogging.SetupBaseLogger()
}

// LettaConfig holds configuration for Letta integration
type LettaConfig struct {
	Enabled   bool   `json:"enabled"`
	ServerURL string `json:"server_url"` // e.g., "http://localhost:8283"
	AgentID   string `json:"agent_id"`   // e.g., "agent-b66fc4bb-1c1c-4898-af4e-778b5e0136b7"
	Timeout   int    `json:"timeout_ms"` // Timeout for Letta queries in milliseconds
}

// LettaClient handles communication with Letta server
type LettaClient struct {
	config     LettaConfig
	httpClient *http.Client
	mu         sync.RWMutex
}

// NewLettaClient creates a new Letta client
func NewLettaClient(cfg LettaConfig) *LettaClient {
	timeout := time.Duration(cfg.Timeout) * time.Millisecond
	if timeout == 0 {
		timeout = 300 * time.Millisecond // Default 300ms timeout
	}
	return &LettaClient{
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
func (lc *LettaClient) GetMemory(ctx context.Context) ([]MemoryBlock, error) {
	if !lc.config.Enabled || lc.config.AgentID == "" {
		return nil, nil
	}

	url := fmt.Sprintf("%s/v1/agents/%s/memory", lc.config.ServerURL, lc.config.AgentID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := lc.httpClient.Do(req)
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

	// Parse memory blocks from response
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
func (lc *LettaClient) UpdateMemory(ctx context.Context, userMessage, assistantResponse string) error {
	if !lc.config.Enabled || lc.config.AgentID == "" {
		return nil
	}

	// Create a summary message for Letta to process
	summaryMessage := fmt.Sprintf(
		"[Memory Update] User asked: %s\n\nAssistant responded (summary): %s",
		truncate(userMessage, 500),
		truncate(assistantResponse, 1000),
	)

	url := fmt.Sprintf("%s/v1/agents/%s/messages", lc.config.ServerURL, lc.config.AgentID)
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

	resp, err := lc.httpClient.Do(req)
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

// Global Letta client instance
var lettaClient *LettaClient

// loadLettaConfig loads Letta configuration from environment or config file
func loadLettaConfig() LettaConfig {
	cfg := LettaConfig{
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

// memoryInjectionMiddleware is the Gin middleware that handles memory injection
func memoryInjectionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only intercept chat completions
		if !strings.Contains(c.Request.URL.Path, "/chat/completions") {
			c.Next()
			return
		}

		// Skip if Letta is not enabled
		if lettaClient == nil || !lettaClient.config.Enabled {
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
		modifiedBody := injectMemoryIntoRequest(c.Request.Context(), bodyBytes)

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
			go func(userMsg string, respBody []byte) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				assistantResponse := extractAssistantResponse(respBody)
				if assistantResponse != "" {
					if err := lettaClient.UpdateMemory(ctx, userMsg, assistantResponse); err != nil {
						fmt.Fprintf(os.Stderr, "[letta-middleware] memory update error: %v\n", err)
					}
				}
			}(userMessage, rw.body.Bytes())
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
func injectMemoryIntoRequest(ctx context.Context, body []byte) []byte {
	// Query Letta for memory with timeout
	queryCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()

	blocks, err := lettaClient.GetMemory(queryCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[letta-middleware] memory query error (continuing without): %v\n", err)
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

	fmt.Fprintf(os.Stderr, "[letta-middleware] injected %d memory blocks\n", len(blocks))
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

func main() {
	// Load Letta configuration
	lettaCfg := loadLettaConfig()
	if lettaCfg.Enabled {
		lettaClient = NewLettaClient(lettaCfg)
		fmt.Printf("[letta-server] Memory injection enabled\n")
		fmt.Printf("[letta-server] Letta server: %s\n", lettaCfg.ServerURL)
		fmt.Printf("[letta-server] Agent ID: %s\n", lettaCfg.AgentID)
	} else {
		fmt.Printf("[letta-server] Memory injection disabled (set LETTA_ENABLED=true to enable)\n")
	}

	// Load CLIProxyAPI config
	// Support --config flag like the original server
	configPath := "config.yaml"
	for i, arg := range os.Args[1:] {
		if arg == "--config" && i+1 < len(os.Args[1:]) {
			configPath = os.Args[i+2]
			break
		} else if strings.HasPrefix(arg, "--config=") {
			configPath = strings.TrimPrefix(arg, "--config=")
			break
		}
	}
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	cfg, err := config.LoadConfigOptional(configPath, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[letta-server] Warning: failed to load config: %v, using defaults\n", err)
		cfg = nil
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	
	// Ensure config file exists (SDK requires it for watcher)
	if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
		// Create parent directory if needed
		if dir := filepath.Dir(configPath); dir != "" && dir != "." {
			os.MkdirAll(dir, 0755)
		}
		// Create minimal config file
		if writeErr := os.WriteFile(configPath, []byte("# Auto-generated config\nport: 8317\n"), 0644); writeErr != nil {
			fmt.Fprintf(os.Stderr, "[letta-server] Warning: could not create config file: %v\n", writeErr)
		}
	}
	// Set defaults if not configured
	if cfg.Port == 0 {
		cfg.Port = 8317
	}
	if cfg.AuthDir == "" {
		cfg.AuthDir = "~/.cli-proxy-api"
	}

	// Setup auth manager - must register token store before using it
	sdkAuth.RegisterTokenStore(sdkAuth.NewFileTokenStore())
	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}
	core := coreauth.NewManager(tokenStore, nil, nil)

	// Register access providers (required for api-key authentication)
	configaccess.Register()

	// Build service with memory injection middleware
	// Note: Don't use WithConfigPath to avoid file watcher panic when config file doesn't exist
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithCoreAuthManager(core).
		WithServerOptions(
			// Memory injection middleware
			api.WithMiddleware(memoryInjectionMiddleware()),
			// Request logger
			api.WithRequestLoggerFactory(func(cfg *config.Config, cfgPath string) logging.RequestLogger {
				return logging.NewFileRequestLogger(cfg.RequestLog, "logs", filepath.Dir(cfgPath))
			}),
		)
	
	// Only add config path if the file actually exists (enables hot-reload)
	builder = builder.WithConfigPath(configPath)
	
	svc, err := builder.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[letta-server] Failed to build service: %v\n", err)
		os.Exit(1)
	}

	// Handle graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("\n[letta-server] Shutting down...")
		cancel()
	}()

	fmt.Printf("[letta-server] Starting on port %d\n", cfg.Port)

	if errRun := svc.Run(ctx); errRun != nil && !errors.Is(errRun, context.Canceled) {
		fmt.Fprintf(os.Stderr, "[letta-server] Error: %v\n", errRun)
		os.Exit(1)
	}

	fmt.Println("[letta-server] Stopped")
}
