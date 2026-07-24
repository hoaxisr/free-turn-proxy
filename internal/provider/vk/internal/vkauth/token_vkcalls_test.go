package vkauth

import (
	"context"
	"errors"
	"fmt"
	"testing"

	tlsclient "github.com/bogdanfinn/tls-client"
)

// fakeVKCalls подменяет vkCallsFetch фиксированным результатом.
func fakeVKCalls(user, pass string, addrs []string, err error) func(context.Context, string, int) (string, string, []string, error) {
	return func(context.Context, string, int) (string, string, []string, error) {
		return user, pass, addrs, err
	}
}

// legacyMustNotRun - tokenChain, падающий при вызове (легаси-путь не должен запускаться).
func legacyMustNotRun(t *testing.T) tokenChainFn {
	return func(context.Context, string, int, VKCredentials, tlsclient.CookieJar) (string, string, []string, error) {
		t.Error("legacy tokenChain must not be called")
		return "", "", nil, errors.New("unexpected legacy call")
	}
}

func TestFetchVKCallsSuccessSkipsLegacy(t *testing.T) {
	c := newTestClient(t, legacyMustNotRun(t))
	c.vkCallsFetch = fakeVKCalls("u", "p", []string{"turn.example:3478"}, nil)

	user, pass, addrs, err := c.fetch(context.Background(), "link", 1)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if user != "u" || pass != "p" || len(addrs) != 1 {
		t.Fatalf("unexpected creds: %q %q %v", user, pass, addrs)
	}
}

func TestFetchVKCallsFallbackToLegacy(t *testing.T) {
	legacyCalled := false
	legacy := func(context.Context, string, int, VKCredentials, tlsclient.CookieJar) (string, string, []string, error) {
		legacyCalled = true
		return "lu", "lp", []string{"legacy.example:3478"}, nil
	}
	c := newTestClient(t, legacy)
	c.vkCallsFetch = fakeVKCalls("", "", nil,
		newVKCallsFailure("step2 messages.getCallPreview", vkCallsFailureVKAPI, fmt.Errorf("error_code=5")))

	user, _, _, err := c.fetch(context.Background(), "link", 1)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !legacyCalled || user != "lu" {
		t.Fatalf("expected legacy fallback, legacyCalled=%v user=%q", legacyCalled, user)
	}
}

func TestFetchVKCallsTerminalNoFallback(t *testing.T) {
	c := newTestClient(t, legacyMustNotRun(t))
	c.vkCallsFetch = fakeVKCalls("", "", nil,
		newVKCallsFailure("step2 messages.getCallPreview", vkCallsFailureCall, ErrInvalidJoinLink))

	_, _, _, err := c.fetch(context.Background(), "link", 1)
	if !errors.Is(err, ErrInvalidJoinLink) {
		t.Fatalf("expected ErrInvalidJoinLink, got %v", err)
	}
}

func TestFetchLegacyEnvModeSkipsVKCalls(t *testing.T) {
	t.Setenv("FREETURN_VK_AUTH_MODE", "legacy")
	legacy := func(context.Context, string, int, VKCredentials, tlsclient.CookieJar) (string, string, []string, error) {
		return "lu", "lp", []string{"legacy.example:3478"}, nil
	}
	c := newTestClient(t, legacy)
	c.vkCallsFetch = func(context.Context, string, int) (string, string, []string, error) {
		t.Error("vkCallsFetch must not be called in legacy mode")
		return "", "", nil, errors.New("unexpected vkcalls call")
	}

	user, _, _, err := c.fetch(context.Background(), "link", 1)
	if err != nil || user != "lu" {
		t.Fatalf("expected legacy success, got user=%q err=%v", user, err)
	}
}

func TestVKCallsAPIErrorClassification(t *testing.T) {
	t.Parallel()

	// Терминальный код -> sentinel (терминальность через vkCallsTerminalLinkError).
	err := vkCallsAPIError(map[string]any{"error": map[string]any{"error_code": float64(9008), "error_msg": "Join link is not valid"}})
	if !errors.Is(err, ErrInvalidJoinLink) {
		t.Fatalf("code 9008: expected ErrInvalidJoinLink, got %v", err)
	}
	wrapped := newVKCallsFailure("step1 auth.getAnonymToken", vkCallsAPIErrorKind(err), err)
	if term := vkCallsTerminalLinkError(wrapped); !errors.Is(term, ErrInvalidJoinLink) {
		t.Fatalf("wrapped terminal not detected: %v", term)
	}

	// Captcha-код 14 -> обычная VK-ошибка с kind=captcha (нетерминальная).
	err = vkCallsAPIError(map[string]any{"error": map[string]any{"error_code": float64(14), "error_msg": "Captcha needed"}})
	var vkErr *vkCallsVKAPIError
	if !errors.As(err, &vkErr) || vkErr.Code != 14 {
		t.Fatalf("code 14: expected vkCallsVKAPIError, got %v", err)
	}
	if kind := vkCallsAPIErrorKind(err); kind != vkCallsFailureCaptcha {
		t.Fatalf("code 14: expected captcha kind, got %s", kind)
	}
	if term := vkCallsTerminalLinkError(newVKCallsFailure("step2", vkCallsFailureCaptcha, err)); term != nil {
		t.Fatalf("captcha must not be terminal, got %v", term)
	}

	// Нет error-объекта -> nil.
	if err := vkCallsAPIError(map[string]any{"response": map[string]any{"token": "x"}}); err != nil {
		t.Fatalf("expected nil for ok response, got %v", err)
	}
}
