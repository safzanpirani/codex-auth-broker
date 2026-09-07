package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (p *responsesProxy) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":              "broker.capabilities",
		"authentication":      "broker_bearer_key",
		"entitlement_checked": false,
		"capabilities": []map[string]any{
			{"id": "responses", "status": "supported", "endpoints": []string{"POST /v1/responses", "GET /v1/responses", "POST /v1/chat/completions"}, "models_endpoint": "/v1/models"},
			{"id": "compaction", "status": "experimental", "endpoints": []string{"POST /v1/responses/compact"}, "note": "Native Codex route; availability depends on the upstream rollout."},
			{"id": "images", "status": "supported", "endpoints": []string{"POST /v1/images/generations", "POST /v1/images/edits"}, "default_model": "gpt-image-2", "transport": "responses_image_generation_tool"},
			{"id": "live", "status": "experimental", "endpoints": []string{"POST /v1/realtime/calls", "POST /v1/live", "GET /v1/live/{call_id}", "GET /v1/realtime?call_id=..."}, "default_model": liveModel, "transport": "webrtc", "events": "native_gpt_live", "standalone_websocket": false, "note": "Requires account access to Codex GPT-Live. GA call creation shape; native GPT-Live session fields and events."},
			{"id": "search", "status": "experimental", "endpoints": []string{"POST /v1/alpha/search"}},
			{"id": "embeddings", "status": "unavailable", "reason": "No verified Codex subscription endpoint."},
			{"id": "audio_files", "status": "unavailable", "reason": "No verified subscription endpoint for file transcription, translation, or speech synthesis."},
		},
	})
}

func (p *responsesProxy) handleUnsupportedCapability(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]any{"error": map[string]any{
		"type": "invalid_request_error", "code": "unsupported_endpoint", "param": nil,
		"message": "This endpoint has no verified Codex subscription route. See GET /v1/capabilities; GPT-Live uses POST /v1/realtime/calls with a broker key.",
	}})
}

// Derive sibling Codex routes from the configured Responses endpoint. Never
// accept an upstream URL from a client or follow a response Location as a URL.
func (p *responsesProxy) codexEndpoint(path string) (string, error) {
	u, err := url.Parse(p.cfg.upstreamURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || !strings.HasSuffix(u.Path, "/responses") {
		return "", errors.New("upstream URL must end in /responses to use this capability")
	}
	u.Path = strings.TrimSuffix(u.Path, "/responses") + "/" + path
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u.String(), nil
}

// Rotate only before submission or after an explicit rate-limit rejection.
// A transport failure can leave an already-created call and is never replayed.
func (p *responsesProxy) postSubscription(ctx context.Context, endpoint string, payload []byte, headers http.Header) (*http.Response, *account, *dispatchFailure) {
	n := p.pool.size()
	if n == 0 {
		return nil, nil, &dispatchFailure{status: http.StatusBadGateway, message: "no Codex accounts configured"}
	}
	client := *p.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var lastFailure *dispatchFailure
	for attempt := 0; attempt < n; attempt++ {
		if ctx.Err() != nil {
			return nil, nil, &dispatchFailure{status: http.StatusRequestTimeout, message: "request canceled"}
		}
		acct, err := p.pool.pick(time.Now())
		if err != nil {
			break
		}
		access, err := acct.mgr.current(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, &dispatchFailure{status: http.StatusRequestTimeout, message: "request canceled"}
			}
			acct.cool(time.Now().Add(authErrorCooldown), "auth")
			lastFailure = &dispatchFailure{status: http.StatusBadGateway, message: "Codex authentication failed"}
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, nil, &dispatchFailure{status: http.StatusBadGateway, message: "invalid capability upstream URL"}
		}
		req.Header.Set("Authorization", "Bearer "+access.AccessToken)
		req.Header.Set("ChatGPT-Account-Id", access.AccountID)
		req.Header.Set("Content-Type", "application/json")
		for key, values := range headers {
			req.Header[key] = append([]string(nil), values...)
		}
		p.setClientIdentity(req)
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, &dispatchFailure{status: http.StatusBadGateway, message: "subscription request failed; not retried"}
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, acct, nil
		}
		_, failure := readCapabilityResponse(resp, 64*1024)
		if failure.status != http.StatusTooManyRequests {
			return nil, nil, failure
		}
		acct.cool(failure.retryAfter, "rate limit")
		lastFailure = failure
	}
	if lastFailure != nil {
		return nil, nil, lastFailure
	}
	return nil, nil, &dispatchFailure{status: http.StatusTooManyRequests, message: "no Codex account is available", retryAfter: p.pool.soonestReset(time.Now())}
}

func readCapabilityResponse(resp *http.Response, maxBytes int64) ([]byte, *dispatchFailure) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes {
		return nil, &dispatchFailure{status: http.StatusBadGateway, message: "upstream response incomplete or too large"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Upstream diagnostics may echo SDP or user content. Report status only.
		status := resp.StatusCode
		if status < 400 {
			status = http.StatusBadGateway
		}
		message := "subscription endpoint rejected the request"
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &envelope) == nil && safeCapabilityCode(envelope.Error.Code) {
			message += " (" + envelope.Error.Code + ")"
		} else if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") {
			message += "; upstream returned an HTML access challenge"
		}
		failure := &dispatchFailure{status: status, message: message}
		if resp.StatusCode == http.StatusTooManyRequests {
			failure.retryAfter, _, _ = deriveCooldown(resp, body, time.Now())
		}
		return nil, failure
	}
	return body, nil
}

func safeCapabilityCode(code string) bool {
	switch code {
	case "forbidden", "invalid_offer", "model_not_found", "permission_denied", "insufficient_quota", "rate_limit_exceeded", "invalid_api_key", "unsupported_model", "invalid_request_error":
		return true
	}
	return false
}

func (p *responsesProxy) handleCompact(w http.ResponseWriter, r *http.Request) {
	entry := p.beginRequestLog(r)
	defer entry.finish()
	if !p.authorizedClient(r) {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusUnauthorized, message: "unauthorized"})
		return
	}
	body, err := decodeRequestBody(r.Body, r.Header.Get("Content-Encoding"))
	if err != nil || stringField(body, "model") == "" || body["input"] == nil {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusBadRequest, message: "compaction requires a JSON object with model and input"})
		return
	}
	model, _ := normalizeFactoryModel(stringField(body, "model"))
	body["model"] = model
	if entry != nil {
		entry.Entry.Model, entry.Entry.NormalizedModel = model, model
	}
	endpoint, err := p.codexEndpoint("responses/compact")
	if err != nil {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusBadGateway, message: err.Error()})
		return
	}
	release, fail := p.acquireUpstreamSlot(r.Context())
	if fail != nil {
		p.writeDispatchFailure(w, entry, fail)
		return
	}
	defer release()
	payload, _ := json.Marshal(body)
	headers := make(http.Header)
	headers.Set("OpenAI-Beta", "responses=experimental")
	headers.Set(codexRoutingHintHeader, buildCodexRoutingHint(model, stringField(body, "service_tier")))
	if features := codexBetaFeatures(r, false); features != "" {
		headers.Set(codexBetaFeaturesHeader, features)
	}
	for _, key := range []string{"x-codex-turn-state", "session_id", "x-client-request-id"} {
		if value := r.Header.Get(key); value != "" {
			headers.Set(key, value)
		}
	}
	resp, _, fail := p.postSubscription(r.Context(), endpoint, payload, headers)
	if fail != nil {
		p.writeDispatchFailure(w, entry, fail)
		return
	}
	entry.markUpstreamStatus(resp.StatusCode)
	result, fail := readCapabilityResponse(resp, maxRequestBodyBytes)
	if fail != nil {
		p.writeDispatchFailure(w, entry, fail)
		return
	}
	var envelope struct {
		Output json.RawMessage `json:"output"`
		Usage  map[string]any  `json:"usage"`
	}
	if json.Unmarshal(result, &envelope) != nil || len(envelope.Output) == 0 || envelope.Output[0] != '[' {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusBadGateway, message: "invalid upstream compaction response"})
		return
	}
	// Preserve encrypted compaction items, ordering, usage and future fields.
	entry.markUsage(extractTokenUsage(map[string]any{"usage": envelope.Usage}))
	if state := resp.Header.Get("x-codex-turn-state"); state != "" {
		w.Header().Set("x-codex-turn-state", state)
	}
	entry.markStatus(resp.StatusCode)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(result)
}
