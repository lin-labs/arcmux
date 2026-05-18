package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

const (
	DefaultEndpoint = "https://api.typesafe.ai/preview/evaluation"
	DefaultModel    = "speed_latest"
	DefaultTimeout  = 1500 * time.Millisecond
)

type Config struct {
	APIKey   string
	Endpoint string
	Model    string
	Timeout  time.Duration
}

type Client struct {
	apiKey     string
	endpoint   string
	model      string
	httpClient *http.Client
}

type Prompt struct {
	Key          string         `json:"key"`
	Type         string         `json:"type"`
	Instructions any            `json:"instructions,omitempty"`
	Options      []ChoiceOption `json:"options,omitempty"`
	Levels       []ScoreLevel   `json:"levels,omitempty"`
}

type ChoiceOption struct {
	Option      string `json:"option"`
	Description any    `json:"description,omitempty"`
}

type ScoreLevel struct {
	Level       int `json:"level"`
	Description any `json:"description,omitempty"`
}

type EvaluationRequest struct {
	Document any      `json:"document"`
	Model    string   `json:"model"`
	Prompts  []Prompt `json:"prompts"`
}

type ChoiceProbability struct {
	Option      string  `json:"option"`
	Probability float64 `json:"probability"`
}

type Response struct {
	Key           string              `json:"key"`
	Type          string              `json:"type"`
	Probability   float64             `json:"probability,omitempty"`
	Chosen        string              `json:"chosen,omitempty"`
	Probabilities []ChoiceProbability `json:"probabilities,omitempty"`
	Confidence    float64             `json:"confidence,omitempty"`
	Expectation   float64             `json:"expectation,omitempty"`
}

type Usage struct {
	BillingUnits int `json:"billing_units"`
}

type EvaluationResponse struct {
	Model     string     `json:"model"`
	Responses []Response `json:"responses"`
	Usage     Usage      `json:"usage"`
}

func New(cfg Config) *Client {
	if cfg.APIKey == "" {
		return nil
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}

	return &Client{
		apiKey:   cfg.APIKey,
		endpoint: cfg.Endpoint,
		model:    cfg.Model,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

func NewFromEnv() *Client {
	return New(Config{
		APIKey: os.Getenv("TYPESAFE_API_KEY"),
	})
}

func (c *Client) Evaluate(ctx context.Context, document any, prompts []Prompt) (*EvaluationResponse, error) {
	if c == nil || c.apiKey == "" {
		return nil, fmt.Errorf("typesafe client is not configured")
	}
	if len(prompts) == 0 {
		return nil, fmt.Errorf("typesafe evaluation requires at least one prompt")
	}

	reqBody := EvaluationRequest{
		Document: document,
		Model:    c.model,
		Prompts:  prompts,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call typesafe: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("typesafe returned %s", resp.Status)
	}

	var parsed EvaluationResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &parsed, nil
}
