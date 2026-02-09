package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

const googleTTSEndpoint = "https://texttospeech.googleapis.com/v1/text:synthesize"

// TTSService generates text-to-speech audio using the Google Cloud TTS REST API.
type TTSService struct {
	directory  string // directory to store generated MP3 files
	apiKey     string // Google Cloud API key (from GOOGLE_TTS_API_KEY env)
	httpClient *http.Client
}

// NewTTSService creates a new TTS service.
// Reads the Google Cloud API key from GOOGLE_TTS_API_KEY environment variable.
func NewTTSService(directory string) *TTSService {
	return &TTSService{
		directory:  directory,
		apiKey:     os.Getenv("GOOGLE_TTS_API_KEY"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// CreateTTSFile generates a TTS MP3 file from the given text and returns the file ID.
// The file is stored as {directory}/{id}.mp3.
func (s *TTSService) CreateTTSFile(ctx context.Context, message string) (string, error) {
	if message == "" {
		return "", fmt.Errorf("empty message")
	}
	if s.apiKey == "" {
		return "", fmt.Errorf("GOOGLE_TTS_API_KEY not set")
	}

	audioContent, err := s.synthesize(ctx, message)
	if err != nil {
		return "", fmt.Errorf("TTS synthesis failed: %w", err)
	}

	id := uuid.New().String()
	filePath := filepath.Join(s.directory, id+".mp3")

	if err := os.MkdirAll(s.directory, 0755); err != nil {
		return "", fmt.Errorf("creating TTS directory: %w", err)
	}

	if err := os.WriteFile(filePath, audioContent, 0644); err != nil {
		return "", fmt.Errorf("writing TTS file: %w", err)
	}

	slog.Info("TTS file created", "id", id, "message", truncate(message, 50), "path", filePath)
	return id, nil
}

// GetTTSFilePath returns the full path for a TTS file ID.
func (s *TTSService) GetTTSFilePath(id string) string {
	return filepath.Join(s.directory, id+".mp3")
}

// synthesize calls the Google Cloud TTS REST API and returns raw MP3 audio bytes.
func (s *TTSService) synthesize(ctx context.Context, text string) ([]byte, error) {
	// Match the JS voice config exactly.
	reqBody := map[string]any{
		"input": map[string]string{
			"text": text,
		},
		"voice": map[string]any{
			"languageCode": "en-GB",
			"name":         "en-GB-Chirp3-HD-Enceladus",
			"ssmlGender":   "MALE",
		},
		"audioConfig": map[string]any{
			"audioEncoding": "MP3",
			"pitch":         0,
			"speakingRate":  1.0,
		},
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := googleTTSEndpoint + "?key=" + s.apiKey
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Google TTS API returned %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		AudioContent string `json:"audioContent"` // base64-encoded
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing TTS response: %w", err)
	}

	audio, err := base64.StdEncoding.DecodeString(result.AudioContent)
	if err != nil {
		return nil, fmt.Errorf("decoding audio content: %w", err)
	}

	return audio, nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
