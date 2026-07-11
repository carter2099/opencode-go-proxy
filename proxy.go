package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// proxyCore wires the picker, upstream, and clients.
type proxyCore struct {
	cfg     Config
	picker  *picker
	upstream string
	client  *http.Client
	scrapeClient *http.Client
}

func newProxyCore(cfg Config) *proxyCore {
	return &proxyCore{
		cfg:     cfg,
		picker:  newPicker(cfg),
		upstream: cfg.Upstream,
		client:  &http.Client{Timeout: cfg.RequestTimeout.Std(), Transport: &http.Transport{}},
		scrapeClient: &http.Client{Timeout: scrapeTimeout, Transport: &http.Transport{}},
	}
}

// handleProxy is the "/" catch-all: picks an account, forwards with auth swapped,
// inspects the cost field on 200s, and applies the 401 cooldown.
func (pc *proxyCore) handleProxy(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	acct, err := pc.picker.choose(now)
	if err != nil {
		writeStructuredError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	// Buffer the request body so we can re-issue transparently.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeStructuredError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	r.Body.Close()

	upstreamURL := pc.upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeStructuredError(w, http.StatusInternalServerError, "build upstream request: "+err.Error())
		return
	}

	// Copy all headers, then swap auth to the chosen account's real key.
	swapAuth(req, r.Header, acct.cfg.APIKey)

	resp, err := pc.client.Do(req)
	if err != nil {
		writeStructuredError(w, http.StatusBadGateway, "upstream request: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Non-200 handling: 401 → cooldown (the single self-healing exception).
	// Everything else passes through with no state mutation.
	if resp.StatusCode == 401 {
		acct.mark401(pc.cfg.Avoid401Cooldown.Std(), time.Now())
		log.Printf("[%s] 401 from upstream — cooldown %v", acct.cfg.Name, pc.cfg.Avoid401Cooldown.Std())
	} else if resp.StatusCode == 200 {
		acct.clear401On200(time.Now())
	}

	if resp.StatusCode == 200 {
		pc.handle200(w, r, resp, acct)
		return
	}

	// Pass through non-200 untouched.
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handle200 forwards a 200 response while extracting the cost signal. For
// non-streaming it reads the full body, parses cost, applies it, and writes.
// For SSE it tees the stream line-by-line forwarding promptly and captures the
// last cost-bearing event.
func (pc *proxyCore) handle200(w http.ResponseWriter, r *http.Request, resp *http.Response, acct *account) {
	ct := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(ct, "text/event-stream")

	copyHeaders(w.Header(), resp.Header)

	if !isSSE {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			// We've already written (or are about to write) the header; best effort.
			log.Printf("[%s] read 200 body failed: %v (partial write)", acct.cfg.Name, err)
			w.WriteHeader(200)
			w.Write(body)
			return
		}
		if cost, ok := extractCostFromBody(body); ok {
			log.Printf("[%s] cost=%s tier=%s", acct.cfg.Name, cost, acct.tier.String())
			acct.applyCost(cost)
		}
		w.WriteHeader(200)
		w.Write(body)
		return
	}

	// SSE streaming: forward bytes promptly while scanning for the trailing cost.
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(respBodyLineReader(resp.Body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lastCost := ""
	lastCostOk := false
	for scanner.Scan() {
		line := scanner.Bytes()
		// Forward the line + newline exactly as received.
		w.Write(line)
		w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
		if c, ok := extractCostFromSSELine(line); ok {
			lastCost = c
			lastCostOk = true
		}
	}
	// A trailing [DONE] marker line carries no cost; we keep the last seen.
	if lastCostOk {
		log.Printf("[%s] stream cost=%s tier=%s", acct.cfg.Name, lastCost, acct.tier.String())
		acct.applyCost(lastCost)
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		log.Printf("[%s] stream scan ended: %v", acct.cfg.Name, err)
	}
}

// respBodyLineReader is a thin wrapper kept as a seam; io.Reader is what we need.
func respBodyLineReader(r io.Reader) io.Reader { return r }

// extractCostFromBody parses the top-level `cost` field from a JSON 200 body.
// cost is emitted as a JSON string ("0", "0.00002260") or a number.
func extractCostFromBody(body []byte) (string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return "", false
	}
	raw, ok := m["cost"]
	if !ok {
		return "", false
	}
	return parseCostRaw(raw)
}

// extractCostFromSSELine parses a `data: {...}` SSE line for the billing-truth
// cost field. It deliberately skips the `inference-cost` telemetry event, which
// carries a *normalised/hypothetical* cost (non-zero even on a go_free account)
// and is NOT the actual charge. The billing summary event has `cost` but no
// `x-opencode-type:"inference-cost"` tag. Relying on event order would be
// fragile, so we disambiguate by the tag instead.
func extractCostFromSSELine(line []byte) (string, bool) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "[DONE]" || payload == "" {
		return "", false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return "", false
	}
	// Skip the normalised-cost telemetry event — its `cost` is hypothetical, not
	// the actual charge. Only the untagged summary event carries billing truth.
	if t, ok := m["x-opencode-type"]; ok {
		var tag string
		if json.Unmarshal(t, &tag) == nil && tag == "inference-cost" {
			return "", false
		}
	}
	raw, ok := m["cost"]
	if !ok {
		return "", false
	}
	return parseCostRaw(raw)
}

// parseCostRaw normalises a json.RawMessage cost into a plain string.
func parseCostRaw(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&n) == nil {
		return n.String(), true
	}
	return "", false
}

// swapAuth replaces the request's auth header(s) with the chosen account's key,
// preserving whichever header shape the client used (Bearer for OpenAI-compat,
// x-api-key for anthropic-messages).
func swapAuth(req *http.Request, src http.Header, apiKey string) {
	// Copy everything else.
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "cookie":
			continue // don't copy client auth (proxy owns keys) or cookies
		case "host":
			continue // http.Request sets Host automatically
		}
		req.Header[k] = vs
	}
	if got := src.Get("X-Api-Key"); got != "" {
		req.Header.Set("X-Api-Key", apiKey)
	} else if got := src.Get("Authorization"); got != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		// Default: shorter path (most pi openai-compat calls hit /v1).
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "content-length", "transfer-encoding", "connection":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func writeStructuredError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}