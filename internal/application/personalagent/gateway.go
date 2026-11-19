package personalagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"agentflow/internal/domain"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// HTTPGateway connects a fixed, operator-configured host capability endpoint.
// Neither planner output nor request bodies can choose an arbitrary URL.
type HTTPGateway struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewHTTPGateway(baseURL, token string) (*HTTPGateway, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: gateway URL must be an absolute HTTP(S) URL", ErrInvalid)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: gateway token is required", ErrInvalid)
	}
	return &HTTPGateway{baseURL: u.String(), token: token, client: &http.Client{Timeout: 150 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (g *HTTPGateway) request(ctx context.Context, scope Scope, method, path string, body, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Project-ID", scope.ProjectID)
	req.Header.Set("X-Actor-ID", scope.ActorID)
	res, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: gateway connection failed, timed out or was cancelled", ErrGateway)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		var failure struct {
			Detail string `json:"detail"`
			Error  string `json:"error"`
		}
		_ = json.Unmarshal(raw, &failure)
		detail := failure.Detail
		if detail == "" {
			detail = failure.Error
		}
		detail = strings.ReplaceAll(detail, g.token, "[redacted]")
		detail = gatewayURLPattern.ReplaceAllString(detail, "[endpoint]")
		detail = gatewayBearerPattern.ReplaceAllString(detail, "Bearer [redacted]")
		runes := []rune(detail)
		if len(runes) > 300 {
			detail = string(runes[:300])
		}
		if detail != "" {
			return fmt.Errorf("%w: %s HTTP %d: %s", ErrGateway, path, res.StatusCode, detail)
		}
		return fmt.Errorf("%w: %s returned HTTP %d", ErrGateway, path, res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
	if err != nil {
		return fmt.Errorf("%w: read response", ErrGateway)
	}
	if len(raw) > 2*1024*1024 {
		return fmt.Errorf("%w: response exceeds 2 MiB", ErrGateway)
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: invalid JSON response", ErrGateway)
	}
	return nil
}

var gatewayURLPattern = regexp.MustCompile(`https?://[^\s<>"']+`)
var gatewayBearerPattern = regexp.MustCompile(`(?i)Bearer\s+[^\s,;]+`)

func (g *HTTPGateway) capabilities(ctx context.Context, scope Scope) ([]domain.PersonalAgentTool, error) {
	var result struct {
		Tools []domain.PersonalAgentTool `json:"tools"`
	}
	if err := g.request(ctx, scope, "GET", "/capabilities", nil, &result); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, tool := range result.Tools {
		if !validID(tool.Name) || seen[tool.Name] || (tool.Effect != "read" && tool.Effect != "write") || tool.Description == "" || tool.InputSchema == nil {
			return nil, fmt.Errorf("%w: invalid tool capability", ErrGateway)
		}
		seen[tool.Name] = true
		if tool.RequiredPhase != "" && tool.RequiredPhase != "analysis" && tool.RequiredPhase != "execution" {
			return nil, fmt.Errorf("%w: invalid requiredPhase", ErrGateway)
		}
		if tool.RequiredPhase == "analysis" && tool.Effect != "read" {
			return nil, fmt.Errorf("%w: analysis evidence tool must be read-only", ErrGateway)
		}
	}
	return result.Tools, nil
}
func (g *HTTPGateway) model(ctx context.Context, scope Scope, purpose, system string, messages []Message) (string, error) {
	var result struct {
		Content string `json:"content"`
	}
	err := g.request(ctx, scope, "POST", "/model", map[string]any{"purpose": purpose, "system": system, "messages": messages}, &result)
	if err != nil {
		return "", err
	}
	if len(strings.TrimSpace(result.Content)) == 0 {
		return "", fmt.Errorf("%w: model returned empty content", ErrGateway)
	}
	return result.Content, nil
}
func (g *HTTPGateway) tool(ctx context.Context, scope Scope, name string, input map[string]any) (any, error) {
	var result struct {
		Result any `json:"result"`
	}
	err := g.request(ctx, scope, "POST", "/tools/"+url.PathEscape(name), input, &result)
	return result.Result, err
}
