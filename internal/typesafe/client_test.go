package typesafe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientEvaluate(t *testing.T) {
	t.Parallel()

	var seenAuth string
	var seenBody EvaluationRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(EvaluationResponse{
			Model: "speed_v9_angry_pig",
			Responses: []Response{
				{
					Key:         "delivery_state",
					Type:        "choice",
					Chosen:      "ingested",
					Confidence:  0.91,
					Probability: 0,
				},
			},
			Usage: Usage{BillingUnits: 1},
		})
	}))
	defer server.Close()

	client := New(Config{
		APIKey:   "secret",
		Endpoint: server.URL,
		Model:    "speed_latest",
	})
	if client == nil {
		t.Fatal("expected client")
	}

	resp, err := client.Evaluate(context.Background(), map[string]any{"screen": "Working"}, []Prompt{
		{
			Key:          "delivery_state",
			Type:         "choice",
			Instructions: "Which state best describes prompt delivery?",
			Options: []ChoiceOption{
				{Option: "ingested", Description: "The agent has started working."},
			},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if seenAuth != "Bearer secret" {
		t.Fatalf("Authorization header = %q", seenAuth)
	}
	if seenBody.Model != "speed_latest" {
		t.Fatalf("model = %q", seenBody.Model)
	}
	if len(seenBody.Prompts) != 1 || seenBody.Prompts[0].Key != "delivery_state" {
		t.Fatalf("unexpected prompts: %+v", seenBody.Prompts)
	}
	if resp.Model != "speed_v9_angry_pig" {
		t.Fatalf("response model = %q", resp.Model)
	}
	if len(resp.Responses) != 1 || resp.Responses[0].Chosen != "ingested" {
		t.Fatalf("unexpected responses: %+v", resp.Responses)
	}
}
