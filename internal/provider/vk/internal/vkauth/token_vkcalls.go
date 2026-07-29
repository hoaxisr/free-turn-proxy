package vkauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"strings"

	"github.com/samosvalishe/free-turn-proxy/internal/provider/vk/internal/browserprofile"
	"github.com/samosvalishe/free-turn-proxy/internal/provider/vk/internal/namegen"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/google/uuid"
)

const (
	vkConnectClientID     = "8093730"
	vkCallsAPIHost        = "api.vk.me"
	vkCallsAnonAPIVersion = "5.276"
)

type vkCallsFailureKind string

const (
	vkCallsFailureSetup   vkCallsFailureKind = "setup"
	vkCallsFailureNetwork vkCallsFailureKind = "network"
	vkCallsFailureDecode  vkCallsFailureKind = "decode"
	vkCallsFailureVKAPI   vkCallsFailureKind = "vk_api"
	vkCallsFailureCaptcha vkCallsFailureKind = "captcha"
	vkCallsFailureCall    vkCallsFailureKind = "call_unavailable"
	vkCallsFailureOKCDN   vkCallsFailureKind = "okcdn_api"
	vkCallsFailureParse   vkCallsFailureKind = "parse"
)

type vkCallsFailure struct {
	Step string
	Kind vkCallsFailureKind
	Err  error
}

func (e *vkCallsFailure) Error() string {
	if e == nil {
		return "vkcalls failure"
	}
	if e.Err == nil {
		return fmt.Sprintf("step=%s kind=%s", e.Step, e.Kind)
	}
	return fmt.Sprintf("step=%s kind=%s: %v", e.Step, e.Kind, e.Err)
}

func (e *vkCallsFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func newVKCallsFailure(step string, kind vkCallsFailureKind, err error) error {
	if err == nil {
		err = fmt.Errorf("unknown error")
	}
	return &vkCallsFailure{Step: step, Kind: kind, Err: err}
}

func vkCallsAPIErrorKind(err error) vkCallsFailureKind {
	if errors.Is(err, ErrInvalidJoinLink) || errors.Is(err, ErrAnonymousBlocked) || errors.Is(err, ErrCallFull) {
		return vkCallsFailureCall
	}
	var vkErr *vkCallsVKAPIError
	if errors.As(err, &vkErr) && vkErr.Code == 14 {
		return vkCallsFailureCaptcha
	}
	return vkCallsFailureVKAPI
}

func vkCallsTerminalLinkError(err error) error {
	if errors.Is(err, ErrInvalidJoinLink) || errors.Is(err, ErrAnonymousBlocked) || errors.Is(err, ErrCallFull) {
		return err
	}
	var failure *vkCallsFailure
	if errors.As(err, &failure) && failure.Err != nil {
		if errors.Is(failure.Err, ErrInvalidJoinLink) || errors.Is(failure.Err, ErrAnonymousBlocked) || errors.Is(failure.Err, ErrCallFull) {
			return failure.Err
		}
	}
	return nil
}

type vkCallsVKAPIError struct {
	Code    int
	Message string
}

func (e *vkCallsVKAPIError) Error() string {
	if e == nil {
		return "VK API error"
	}
	if e.Message == "" {
		return fmt.Sprintf("error_code=%d", e.Code)
	}
	return fmt.Sprintf("error_code=%d %s", e.Code, e.Message)
}

type vkCallsOKAPIError struct {
	Code    int
	Message string
}

func (e *vkCallsOKAPIError) Error() string {
	if e == nil {
		return "OK CDN API error"
	}
	if e.Message == "" {
		return fmt.Sprintf("error_code=%d", e.Code)
	}
	return fmt.Sprintf("error_code=%d %s", e.Code, e.Message)
}

func vkAuthModeLegacy() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FREETURN_VK_AUTH_MODE"))) {
	case "legacy":
		return true
	default:
		return false
	}
}

// getVKCredsViaVKCallsPath — WDTT-style auth через api.vk.me (обычно без Smart Captcha).
func (c *Client) getVKCredsViaVKCallsPath(ctx context.Context, link string, streamID int) (string, string, []string, error) {
	deviceID := uuid.New().String()
	name := namegen.Generate()
	profile := c.currentPersona()
	profile.AcceptLanguage = "en-GB,en;q=0.9"
	linkURL := neturl.QueryEscape("https://vk.com/call/join/" + link)
	nameEnc := neturl.QueryEscape(name)

	jar := tlsclient.NewCookieJar()
	httpClient, err := c.newTLSClient(profile, jar)
	if err != nil {
		return "", "", nil, newVKCallsFailure("setup", vkCallsFailureSetup, fmt.Errorf("create tls client: %w", err))
	}

	c.log.Infof("[STREAM %d] [VKCalls] Identity - Name: %s | device_id=%s | TLS=Chrome_146 | UA: %s",
		streamID, name, deviceID, profile.UserAgent)

	doRequest := func(step, url string) (map[string]any, error) {
		req, err := fhttp.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(nil))
		if err != nil {
			return nil, newVKCallsFailure(step, vkCallsFailureSetup, fmt.Errorf("create request: %w", err))
		}
		browserprofile.ApplyFhttp(req, profile)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

		httpResp, err := httpClient.Do(req)
		if err != nil {
			return nil, newVKCallsFailure(step, vkCallsFailureNetwork, fmt.Errorf("request failed: %w", err))
		}
		defer func() {
			if closeErr := httpResp.Body.Close(); closeErr != nil {
				c.log.Warnf("[VKCalls] close response body: %s", closeErr)
			}
		}()

		body, err := io.ReadAll(httpResp.Body)
		if err != nil {
			return nil, newVKCallsFailure(step, vkCallsFailureNetwork, fmt.Errorf("read response: %w", err))
		}

		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, newVKCallsFailure(step, vkCallsFailureDecode, fmt.Errorf("unmarshal JSON: %w, body: %s", err, truncateVKCallsLog(string(body), 200)))
		}
		return resp, nil
	}

	step1 := "step1 auth.getAnonymToken"
	step1URL := fmt.Sprintf(
		"https://%s/method/auth.getAnonymToken?v=%s&client_id=%s&link=%s&device_id=%s&anonymName=%s&lang=en",
		vkCallsAPIHost, vkCallsAnonAPIVersion, vkConnectClientID,
		linkURL, deviceID, nameEnc,
	)
	resp1, err := doRequest(step1, step1URL)
	if err != nil {
		return "", "", nil, err
	}
	if apiErr := vkCallsAPIError(resp1); apiErr != nil {
		return "", "", nil, newVKCallsFailure(step1, vkCallsAPIErrorKind(apiErr), apiErr)
	}
	anonymToken, err := extractVKCallsStr(resp1, "response", "token")
	if err != nil {
		return "", "", nil, newVKCallsFailure(step1, vkCallsFailureParse, fmt.Errorf("parse token: %w (resp: %s)", err, truncateVKCallsResp(resp1)))
	}
	anonymTokenEnc := neturl.QueryEscape(anonymToken)
	c.log.Infof("[STREAM %d] [VKCalls] step1 OK, anonymous_token (%d chars)", streamID, len(anonymToken))

	step2 := "step2 messages.getCallPreview"
	step2URL := fmt.Sprintf(
		"https://%s/method/messages.getCallPreview?v=%s&anonymous_token=%s&device_id=%s&extended=1&fields=first_name,last_name,photo_200&lang=en&link=%s",
		vkCallsAPIHost, vkCallsAnonAPIVersion, anonymTokenEnc, deviceID, linkURL,
	)
	resp2, err := doRequest(step2, step2URL)
	if err != nil {
		return "", "", nil, err
	}
	if apiErr := vkCallsAPIError(resp2); apiErr != nil {
		if captchaErr, ok := apiErr.(*vkCallsVKAPIError); ok && captchaErr.Code == 14 {
			c.log.Infof("[STREAM %d] [VKCalls] step2 captcha gate appeared", streamID)
		}
		return "", "", nil, newVKCallsFailure(step2, vkCallsAPIErrorKind(apiErr), apiErr)
	}
	userIDFloat, err := extractVKCallsFloat(resp2, "response", "user_id")
	if err != nil {
		return "", "", nil, newVKCallsFailure(step2, vkCallsFailureParse, fmt.Errorf("parse user_id: %w (resp: %s)", err, truncateVKCallsResp(resp2)))
	}
	userIDStr := fmt.Sprintf("%.0f", userIDFloat)
	secret, err := extractVKCallsStr(resp2, "response", "secret")
	if err != nil {
		return "", "", nil, newVKCallsFailure(step2, vkCallsFailureParse, fmt.Errorf("parse secret: %w", err))
	}
	c.log.Infof("[STREAM %d] [VKCalls] step2 OK, user_id=%s, secret (%d chars)", streamID, userIDStr, len(secret))

	step3 := "step3 messages.getAnonymCallToken"
	step3URL := fmt.Sprintf(
		"https://%s/method/messages.getAnonymCallToken?v=%s&anonymous_token=%s&device_id=%s&link=%s&name=%s&user_id=%s&secret=%s&lang=en",
		vkCallsAPIHost, vkCallsAnonAPIVersion, anonymTokenEnc, deviceID, linkURL,
		nameEnc, userIDStr, neturl.QueryEscape(secret),
	)
	resp3, err := doRequest(step3, step3URL)
	if err != nil {
		return "", "", nil, err
	}
	if apiErr := vkCallsAPIError(resp3); apiErr != nil {
		if captchaErr, ok := apiErr.(*vkCallsVKAPIError); ok && captchaErr.Code == 14 {
			c.log.Infof("[STREAM %d] [VKCalls] step3 captcha gate appeared", streamID)
		}
		return "", "", nil, newVKCallsFailure(step3, vkCallsAPIErrorKind(apiErr), apiErr)
	}
	okAnonymToken, err := extractVKCallsStr(resp3, "response", "token")
	if err != nil {
		return "", "", nil, newVKCallsFailure(step3, vkCallsFailureParse, fmt.Errorf("parse token: %w (resp: %s)", err, truncateVKCallsResp(resp3)))
	}
	c.log.Infof("[STREAM %d] [VKCalls] step3 OK, OK anonymToken (%d chars)", streamID, len(okAnonymToken))

	okDeviceID := uuid.New().String()
	step4 := "step4 auth.anonymLogin"
	step4URL := "https://calls.okcdn.ru/fb.do?session_data=" +
		neturl.QueryEscape(fmt.Sprintf(
			`{"version":2,"device_id":"%s","client_version":"1.0.1"}`, okDeviceID,
		)) +
		"&method=auth.anonymLogin&format=JSON&application_key=CGMMEJLGDIHBABABA"
	resp4, err := doRequest(step4, step4URL)
	if err != nil {
		return "", "", nil, err
	}
	// resp4/resp5 в текст ошибок не включаем: там session_key и TURN-креды.
	sessionKey, err := extractVKCallsStr(resp4, "session_key")
	if err != nil {
		return "", "", nil, newVKCallsFailure(step4, vkCallsFailureParse, fmt.Errorf("parse session_key: %w", err))
	}
	c.log.Infof("[STREAM %d] [VKCalls] step4 OK, OK session_key (%d chars)", streamID, len(sessionKey))

	step5 := "step5 vchat.joinConversationByLink"
	step5URL := fmt.Sprintf(
		"https://calls.okcdn.ru/fb.do?joinLink=%s&isVideo=false&protocolVersion=5&anonymToken=%s&method=vchat.joinConversationByLink&format=JSON&application_key=CGMMEJLGDIHBABABA&session_key=%s",
		neturl.QueryEscape(link), neturl.QueryEscape(okAnonymToken), neturl.QueryEscape(sessionKey),
	)
	resp5, err := doRequest(step5, step5URL)
	if err != nil {
		return "", "", nil, err
	}
	if okErr := vkCallsOKError(resp5); okErr != nil {
		return "", "", nil, newVKCallsFailure(step5, vkCallsFailureOKCDN, okErr)
	}

	user, err := extractVKCallsStr(resp5, "turn_server", "username")
	if err != nil {
		return "", "", nil, newVKCallsFailure(step5, vkCallsFailureParse, fmt.Errorf("parse username: %w", err))
	}
	pass, err := extractVKCallsStr(resp5, "turn_server", "credential")
	if err != nil {
		return "", "", nil, newVKCallsFailure(step5, vkCallsFailureParse, fmt.Errorf("parse credential: %w", err))
	}
	addrs := parseVKCallsTURNAddresses(c.log, resp5)
	if len(addrs) == 0 {
		return "", "", nil, newVKCallsFailure(step5, vkCallsFailureParse, fmt.Errorf("turn_server.urls empty"))
	}

	c.log.Infof("[STREAM %d] [VKCalls] SUCCESS, TURN urls=%d", streamID, len(addrs))
	return user, pass, addrs, nil
}

func extractVKCallsStr(resp map[string]any, keys ...string) (string, error) {
	var cur any = resp
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("expected map at key %q, got %T", k, cur)
		}
		cur = m[k]
	}
	s, ok := cur.(string)
	if !ok {
		return "", fmt.Errorf("expected string at end of path, got %T", cur)
	}
	return s, nil
}

func extractVKCallsFloat(resp map[string]any, keys ...string) (float64, error) {
	var cur any = resp
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("expected map at key %q, got %T", k, cur)
		}
		cur = m[k]
	}
	f, ok := cur.(float64)
	if !ok {
		return 0, fmt.Errorf("expected float64 at end of path, got %T", cur)
	}
	return f, nil
}

func parseVKCallsTURNAddresses(log interface {
	Infof(string, ...any)
}, resp map[string]any) []string {
	turnServer, ok := resp["turn_server"].(map[string]any)
	if !ok {
		return nil
	}
	urls, ok := turnServer["urls"].([]any)
	if !ok {
		return nil
	}
	var addrs []string
	for i, u := range urls {
		s, ok := u.(string)
		if !ok {
			log.Infof("[VKCalls] turn_server.urls[%d]=<non-string %T>, skipping", i, u)
			continue
		}
		clean := strings.Split(s, "?")[0]
		addr := strings.TrimPrefix(strings.TrimPrefix(clean, "turn:"), "turns:")
		log.Infof("[VKCalls] turn_server.urls[%d]=%s", i, addr)
		addrs = append(addrs, addr)
	}
	return addrs
}

func vkCallsAPIError(resp map[string]any) error {
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		return nil
	}
	code := 0
	if f, ok := errObj["error_code"].(float64); ok {
		code = int(f)
	}
	msg, _ := errObj["error_msg"].(string)
	if code == 0 && msg == "" {
		return nil
	}
	if term := classifyLinkError(errObj); term != nil {
		return term
	}
	return &vkCallsVKAPIError{Code: code, Message: msg}
}

func vkCallsOKError(resp map[string]any) error {
	code, ok := resp["error_code"].(float64)
	if !ok || code == 0 {
		return nil
	}
	msg, _ := resp["error_msg"].(string)
	return &vkCallsOKAPIError{Code: int(code), Message: msg}
}

func truncateVKCallsLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func truncateVKCallsResp(resp map[string]any) string {
	b, err := json.Marshal(resp)
	if err != nil {
		return fmt.Sprintf("(unmarshallable: %v)", err)
	}
	return truncateVKCallsLog(string(b), 300)
}
