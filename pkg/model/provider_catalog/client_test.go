package provider_catalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFetchOpenAIModelsReturnsGeneralModelsOnly(t *testing.T) {
	client := &Client{HTTPClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(`{
					"data": [
                        {"id":"gpt-5"},
                        {"id":"gpt-5.6-Luna"},
                        {"id":"gpt-4.1-mini"},
                        {"id":"gpt-realtime"},
                        {"id":"gpt-realtime-mini"},
                        {"id":"gpt-4o-mini-tts"},
                        {"id":"gpt-4o-transcribe"},
                        {"id":"text-embedding-3-large"},
                        {"id":"moderation-latest"}
                    ]
				}`)),
			}, nil
		}),
	}}

	got, err := client.generalOpenAIModels(context.Background(), "test-api-key")
	if err != nil {
		t.Fatalf("generalOpenAIModels() returned error: %v", err)
	}

	want := []string{"gpt-5", "gpt-5.6-Luna", "gpt-4.1-mini"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generalOpenAIModels() = %v, want %v", got, want)
	}

}

func TestFetchOpenAIModelsReturnsRealtimeModelsOnly(t *testing.T) {
	client := &Client{HTTPClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(`{
					"data": [
						{"id":"gpt-5"},
						{"id":"gpt-realtime"},
						{"id":"gpt-realtime-mini"},
						{"id":"gpt-4o-mini-tts"},
						{"id":"gpt-4o-transcribe"}
					]
				}`)),
			}, nil
		}),
	}}

	got, err := client.realtimeOpenAIModels(context.Background(), "test-api-key")
	if err != nil {
		t.Fatalf("realtimeOpenAIModels() returned error: %v", err)
	}

	want := []string{"gpt-realtime", "gpt-realtime-mini"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("realtimeOpenAIModels() = %v, want %v", got, want)
	}
}

func TestFetchGoogleModelsIntegration(t *testing.T) {
	apiKey := os.Getenv("GOOGLE_API_KEY")
	if apiKey == "" {
		t.Skip("GOOGLE_API_KEY не задан в окружении")
	}

	models, err := NewClient().fetchGoogleModels(context.Background(), apiKey)
	if err != nil {
		t.Fatalf("fetchGoogleModels() returned error: %v", err)
	}

	fmt.Println("ALL models:", models)

	if len(models) == 0 {
		t.Fatal("fetchGoogleModels() returned no models")
	}
}

func TestGeneralGoogleModelsIntegration(t *testing.T) {
	apiKey := os.Getenv("GOOGLE_API_KEY")
	if apiKey == "" {
		t.Skip("GOOGLE_API_KEY не задан в окружении")
	}

	models, err := NewClient().generalGoogleModels(context.Background(), apiKey)
	if err != nil {
		t.Fatalf("generalGoogleModels() returned error: %v", err)
	}

	fmt.Println(models)

	if len(models) == 0 {
		t.Fatal("generalGoogleModels() returned no models")
	}
	for _, model := range models {
		if !isGeneralGoogleModel(model) {
			t.Errorf("generalGoogleModels() returned specialized model %q", model)
		}
	}
}

func TestFetchMistralRealtimeModelsIntegration(t *testing.T) {
	apiKey := os.Getenv("MISTRAL_API_KEY")
	if apiKey == "" {
		t.Skip("MISTRAL_API_KEY не задан в окружении")
	}

	models, err := NewClient().realtimeMistralModels(context.Background(), apiKey)
	if err != nil {
		t.Fatalf("realtimeMistralModels() returned error: %v", err)
	}

	fmt.Println(models)

	if len(models) == 0 {
		t.Fatal("realtimeMistralModels() returned no models")
	}
	for _, model := range models {
		if !isRealtimeModel(model) {
			t.Errorf("realtimeMistralModels() returned specialized model %q", model)
		}
	}
}

func TestGeneralMistralModelsIntegration(t *testing.T) {
	apiKey := os.Getenv("MISTRAL_API_KEY")
	if apiKey == "" {
		t.Skip("MISTRAL_API_KEY не задан в окружении")
	}

	models, err := NewClient().generalMistralModels(context.Background(), apiKey)
	if err != nil {
		t.Fatalf("generalMistralModels() returned error: %v", err)
	}

	fmt.Println("mistral general:", models)

	if len(models) == 0 {
		t.Fatal("generalMistralModels() returned no models")
	}
	for _, model := range models {
		if !isGeneralMistralModel(model) {
			t.Errorf("generalMistralModels() returned specialized model %q", model)
		}
	}
}

func TestMistralAllModelsIntegration(t *testing.T) {
	apiKey := os.Getenv("MISTRAL_API_KEY")
	if apiKey == "" {
		t.Skip("MISTRAL_API_KEY не задан в окружении")
	}

	models, err := NewClient().fetchMistralModels(context.Background(), apiKey)
	if err != nil {
		t.Fatalf("generalMistralModels() returned error: %v", err)
	}

	fmt.Println("all mistral general:", models)

	if len(models) == 0 {
		t.Fatal("generalMistralModels() returned no models")
	}
}
