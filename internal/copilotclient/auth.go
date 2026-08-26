package copilotclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Copilot OAuth 参数对齐 Zed（crates/copilot_chat/src/copilot_oauth.rs）。
// 直接使用 Zed 公开应用的 client ID 和 read:user scope，避免 VSCode 伪装。
const (
	githubCopilotClientID = "6e3a0413e62d19d75ff1"
	deviceCodeScope       = "read:user"
	deviceCodeGrantType   = "urn:ietf:params:oauth:grant-type:device_code"
	defaultDeviceInterval = 5 * time.Second
	slowDownStep          = 5 * time.Second
)

const (
	defaultDeviceCodeURL  = "https://github.com/login/device/code"
	defaultAccessTokenURL = "https://github.com/login/oauth/access_token"
)

// DeviceFlow 是一次进行中的 GitHub device authorization。UserCode 与
// VerificationURI 可安全对外展示；deviceCode 只参与 token 轮询，禁止序列化。
type DeviceFlow struct {
	UserCode        string
	VerificationURI string
	Interval        time.Duration
	deviceCode      string
}

// AuthClient 封装 Zed 式 GitHub Device Flow 的 HTTP 请求，允许注入测试端点。
// 并发安全：每个方法只使用独立请求，不共享可变状态。
type AuthClient struct {
	http           *http.Client
	deviceCodeURL  string
	accessTokenURL string
}

// NewAuthClient 构造 AuthClient。空 deviceCodeURL/accessTokenURL 使用官方
// github.com 地址；nil http client 使用默认 client。
func NewAuthClient(hc *http.Client, deviceCodeURL, accessTokenURL string) *AuthClient {
	if hc == nil {
		hc = &http.Client{}
	}
	if deviceCodeURL == "" {
		deviceCodeURL = defaultDeviceCodeURL
	}
	if accessTokenURL == "" {
		accessTokenURL = defaultAccessTokenURL
	}
	return &AuthClient{
		http:           hc,
		deviceCodeURL:  deviceCodeURL,
		accessTokenURL: accessTokenURL,
	}
}

// StartDeviceFlow 请求一个 device code。返回的 DeviceFlow 包含可展示的
// user code 与验证地址；device code 始终留在服务端。失败时返回不含凭据的
// error，不会把 access token 带入错误文本。
func (c *AuthClient) StartDeviceFlow(ctx context.Context) (*DeviceFlow, error) {
	form := url.Values{}
	form.Set("client_id", githubCopilotClientID)
	form.Set("scope", deviceCodeScope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.deviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("copilot device-code: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot device-code: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot device-code: status %d", resp.StatusCode)
	}

	var parsed struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("copilot device-code: decode: %w", err)
	}
	if parsed.DeviceCode == "" || parsed.UserCode == "" || parsed.VerificationURI == "" {
		return nil, fmt.Errorf("copilot device-code: response missing required fields")
	}

	interval := defaultDeviceInterval
	if parsed.Interval > 0 {
		interval = time.Duration(parsed.Interval) * time.Second
	}
	return &DeviceFlow{
		UserCode:        parsed.UserCode,
		VerificationURI: parsed.VerificationURI,
		Interval:        interval,
		deviceCode:      parsed.DeviceCode,
	}, nil
}

// PollDeviceFlow 发起一次 access_token 请求。返回值的语义：
// token 非空表示授权成功；token 为空且 err 为 nil 表示 authorization_pending，
// nextInterval 保持原节奏（slow_down 时增加 slowDownStep）；其余返回不含凭据
// 的 error。调用方负责在 nextInterval 后再次轮询。
func (c *AuthClient) PollDeviceFlow(ctx context.Context, flow *DeviceFlow) (token string, nextInterval time.Duration, err error) {
	form := url.Values{}
	form.Set("client_id", githubCopilotClientID)
	form.Set("device_code", flow.deviceCode)
	form.Set("grant_type", deviceCodeGrantType)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.accessTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("copilot token: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("copilot token: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("copilot token: status %d", resp.StatusCode)
	}

	var parsed struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return "", 0, fmt.Errorf("copilot token: decode: %w", err)
	}
	if parsed.AccessToken != "" {
		return parsed.AccessToken, 0, nil
	}
	switch parsed.Error {
	case "authorization_pending":
		return "", flow.Interval, nil
	case "slow_down":
		return "", flow.Interval + slowDownStep, nil
	case "expired_token":
		return "", 0, fmt.Errorf("copilot sign-in code expired, please retry")
	case "access_denied":
		return "", 0, fmt.Errorf("copilot sign-in cancelled")
	case "":
		return "", 0, fmt.Errorf("copilot sign-in failed: unexpected response from GitHub")
	default:
		return "", 0, fmt.Errorf("copilot sign-in failed: %s", parsed.Error)
	}
}
