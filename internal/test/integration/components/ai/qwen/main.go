// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
)

func qwenBaseURL() string {
	if v := os.Getenv("QWEN_BASE_URL"); v != "" {
		return v
	}
	return "http://localhost:8085"
}

func doPostJSON(path string, payload any) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest(http.MethodPost, qwenBaseURL()+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return respBody, resp.StatusCode, nil
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok!"))
}

func chatHandler(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"model": "qwen-plus",
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "你是谁？"},
		},
	}

	body, code, err := doPostJSON("/compatible-mode/v1/chat/completions", payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, code, body)
}

func generationHandler(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"model":  "qwen-turbo",
		"prompt": "Explain eBPF in one sentence.",
	}

	body, code, err := doPostJSON("/api/v1/services/aigc/text-generation/generation", payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, code, body)
}

func errorHandler(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"model": "qwen-plus",
		"messages": []map[string]string{
			{"role": "user", "content": "trigger error"},
		},
	}

	body, code, err := doPostJSON("/compatible-mode/v1/chat/completions?error=true", payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, code, body)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/chat", chatHandler)
	mux.HandleFunc("/generation", generationHandler)
	mux.HandleFunc("/error", errorHandler)

	log.Printf("Qwen test server running on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal(err)
	}
}
