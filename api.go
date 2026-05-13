package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ─── API Request / Response types ────────────────────────────────────────────

// StripeCheckRequest is the JSON body for POST /api/check
type StripeCheckRequest struct {
	Card string `json:"card"` // "num|mm|yyyy|cvv" format

	// Optional overrides (defaults used if empty)
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	Country string `json:"country_code,omitempty"`
	Address string `json:"address1,omitempty"`
	City    string `json:"city,omitempty"`
	Zip     string `json:"zip_code,omitempty"`
	State   string `json:"state,omitempty"`
	Phone   string `json:"phone,omitempty"`
}

// BatchStripeCheckRequest is the JSON body for POST /api/check/batch
type BatchStripeCheckRequest struct {
	Cards      []string `json:"cards"`                 // ["num|mm|yyyy|cvv", ...]
	MaxWorkers int      `json:"max_workers,omitempty"` // default 2

	// Optional shared overrides
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	Country string `json:"country_code,omitempty"`
	Address string `json:"address1,omitempty"`
	City    string `json:"city,omitempty"`
	Zip     string `json:"zip_code,omitempty"`
	State   string `json:"state,omitempty"`
	Phone   string `json:"phone,omitempty"`
}

// BatchCheckResponse wraps results for multiple cards
type BatchCheckResponse struct {
	Results    []CheckResult `json:"results"`
	TotalCards int           `json:"total_cards"`
	Elapsed    float64       `json:"elapsed"`
}

// ─── HTTP Handlers ───────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "stripe-checker-api",
	})
}

func handleCheckSingle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed, use POST"})
		return
	}

	var req StripeCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	if req.Card == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "card is required (format: num|mm|yyyy|cvv)"})
		return
	}

	// Parse card line
	cardCfg, ok := parseCardLine(req.Card)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid card format, expected: number|mm|yyyy|cvv"})
		return
	}

	// Apply optional overrides from request
	if req.Name != "" {
		cardCfg.Name = req.Name
	}
	if req.Email != "" {
		cardCfg.Email = req.Email
	}
	if req.Country != "" {
		cardCfg.Country = req.Country
	}
	if req.Address != "" {
		cardCfg.Address1 = req.Address
	}
	if req.City != "" {
		cardCfg.City = req.City
	}
	if req.Zip != "" {
		cardCfg.Zip = req.Zip
	}
	if req.State != "" {
		cardCfg.State = req.State
	}
	if req.Phone != "" {
		cardCfg.Phone = req.Phone
	}

	// Fill remaining defaults
	cardCfg = fillDefaults(cardCfg)

	// Redirect logs to stderr
	oldStdout := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = oldStdout }()

	result := processCard(cardCfg)
	writeJSON(w, http.StatusOK, result)
}

func handleCheckBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed, use POST"})
		return
	}

	var req BatchStripeCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	if len(req.Cards) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cards array is required and must not be empty"})
		return
	}

	maxWorkers := req.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = 2
	}
	if maxWorkers > 8 {
		maxWorkers = 8
	}

	// Redirect logs to stderr
	oldStdout := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = oldStdout }()

	start := time.Now()
	results := make([]CheckResult, len(req.Cards))
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup

	for i, cardLine := range req.Cards {
		wg.Add(1)
		sem <- struct{}{}

		go func(idx int, card string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					results[idx] = CheckResult{
						Status:  "error",
						Code:    "INTERNAL_ERROR",
						Message: fmt.Sprintf("worker panic: %v", r),
					}
				}
			}()

			cardCfg, ok := parseCardLine(card)
			if !ok {
				results[idx] = CheckResult{
					Status:  "error",
					Code:    "INVALID_CARD",
					Message: "could not parse card line",
					Card:    card,
				}
				return
			}

			// Apply shared overrides
			if req.Name != "" {
				cardCfg.Name = req.Name
			}
			if req.Email != "" {
				cardCfg.Email = req.Email
			}
			if req.Country != "" {
				cardCfg.Country = req.Country
			}
			if req.Address != "" {
				cardCfg.Address1 = req.Address
			}
			if req.City != "" {
				cardCfg.City = req.City
			}
			if req.Zip != "" {
				cardCfg.Zip = req.Zip
			}
			if req.State != "" {
				cardCfg.State = req.State
			}
			if req.Phone != "" {
				cardCfg.Phone = req.Phone
			}

			cardCfg = fillDefaults(cardCfg)
			results[idx] = processCard(cardCfg)
		}(i, cardLine)

		// Stagger starts to avoid rate limiting
		time.Sleep(500 * time.Millisecond)
	}

	wg.Wait()

	writeJSON(w, http.StatusOK, BatchCheckResponse{
		Results:    results,
		TotalCards: len(req.Cards),
		Elapsed:    time.Since(start).Seconds(),
	})
}

// ─── Middleware ───────────────────────────────────────────────────────────────

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("[API] %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
		log.Printf("[API] %s %s completed in %.2fs", r.Method, r.URL.Path, time.Since(start).Seconds())
	})
}

// ─── Server setup ────────────────────────────────────────────────────────────

func runAPIServer() {
	port := os.Getenv("PORT") // Railway sets PORT automatically
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/api/check", handleCheckSingle)
	mux.HandleFunc("/api/check/batch", handleCheckBatch)

	handler := corsMiddleware(loggingMiddleware(mux))

	addr := ":" + port
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("  Stripe Checker API Server")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("  Listening on http://0.0.0.0%s\n", addr)
	fmt.Println()
	fmt.Println("  Endpoints:")
	fmt.Println("    GET  /health           - Health check")
	fmt.Println("    POST /api/check        - Check a single card")
	fmt.Println("    POST /api/check/batch  - Check multiple cards")
	fmt.Println()
	fmt.Println("  Example:")
	fmt.Println(`    curl -X POST http://localhost:` + port + `/api/check \`)
	fmt.Println(`      -H "Content-Type: application/json" \`)
	fmt.Println(`      -d '{"card": "4111111111111111|12|2028|123"}'`)
	fmt.Println()
	fmt.Println(strings.Repeat("=", 60))

	log.Fatal(http.ListenAndServe(addr, handler))
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v) //nolint:errcheck
}
