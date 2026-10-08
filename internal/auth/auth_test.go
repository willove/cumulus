package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(auth, xkey string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/qa", nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	if xkey != "" {
		r.Header.Set("X-Cumulus-Key", xkey)
	}
	return r
}

// 没配凭证表 = 默认放行（单机开发的老路径一字不变）。
func TestEmptyKeyringAllows(t *testing.T) {
	kr, err := ParseKeyringSpec("")
	if err != nil {
		t.Fatal(err)
	}
	if !kr.Empty() {
		t.Fatal("empty spec must yield an empty keyring")
	}
	realm, err := kr.Authenticate(req("", ""))
	if err != nil || realm != "" {
		t.Fatalf("empty keyring must allow with empty realm: %q %v", realm, err)
	}
}

// realm 由凭证推导：Bearer 与 X-Cumulus-Key 都认。
func TestRealmComesFromCredential(t *testing.T) {
	kr, _ := ParseKeyringSpec("alpha=sk-a,beta=sk-b")
	if realm, err := kr.Authenticate(req("Bearer sk-a", "")); err != nil || realm != "alpha" {
		t.Fatalf("bearer wrong: %q %v", realm, err)
	}
	if realm, err := kr.Authenticate(req("", "sk-b")); err != nil || realm != "beta" {
		t.Fatalf("x-key wrong: %q %v", realm, err)
	}
	// 大小写不敏感（bearer / BEARER）
	if realm, err := kr.Authenticate(req("bearer sk-a", "")); err != nil || realm != "alpha" {
		t.Fatalf("scheme must be case-insensitive: %q %v", realm, err)
	}
}

// 缺凭证 401 / 凭证错 403：前端要能区分"登录"和"没权限"。
func TestMissingVsBadCredential(t *testing.T) {
	kr, _ := ParseKeyringSpec("alpha=sk-a")
	if _, err := kr.Authenticate(req("", "")); err == nil {
		t.Fatal("missing credential must error")
	} else if _, ok := err.(ErrNoCredential); !ok {
		t.Fatalf("missing credential must be ErrNoCredential: %v", err)
	}
	if _, err := kr.Authenticate(req("Bearer sk-wrong", "")); err == nil {
		t.Fatal("bad credential must error")
	} else if _, ok := err.(ErrBadCredential); !ok {
		t.Fatalf("bad credential must be ErrBadCredential: %v", err)
	}

	rec := httptest.NewRecorder()
	WriteAuthError(rec, ErrNoCredential{})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("401 expected: %d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	WriteAuthError(rec2, ErrBadCredential{})
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("403 expected: %d", rec2.Code)
	}
	// 错误体**不许**泄露 realm 列表（那是给攻击者的地图）
	if got := rec2.Body.String(); contains(got, "alpha") {
		t.Fatalf("auth error must not leak realms: %s", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// 一个 realm 一把钥匙：重复 realm 取第一个（不制造"哪把算数"的运维谜题）。
func TestOneKeyPerRealm(t *testing.T) {
	kr, err := NewKeyring([]string{"alpha=sk-1", "alpha=sk-2"})
	if err != nil {
		t.Fatal(err)
	}
	if realm, err := kr.Authenticate(req("Bearer sk-1", "")); err != nil || realm != "alpha" {
		t.Fatalf("first key must win: %q %v", realm, err)
	}
	if _, err := kr.Authenticate(req("Bearer sk-2", "")); err == nil {
		t.Fatal("second key of the same realm must not be accepted")
	}
	if len(kr.Realms()) != 1 {
		t.Fatalf("realms: %v", kr.Realms())
	}
}

// 格式错误要**报错**（配错凭证表是部署事故，不该静默宽容）。
func TestMalformedSpecErrors(t *testing.T) {
	if _, err := ParseKeyringSpec("no-equals"); err == nil {
		t.Fatal("malformed entry must error")
	}
	if _, err := NewKeyring([]string{"=sk"}); err == nil {
		t.Fatal("empty realm must error")
	}
	if _, err := NewKeyring([]string{"alpha="}); err == nil {
		t.Fatal("empty key must error")
	}
	// 逗号分隔 + 空格容忍
	kr, err := ParseKeyringSpec(" alpha = sk-a , beta=sk-b ")
	if err != nil {
		t.Fatal(err)
	}
	if len(kr.Realms()) != 2 {
		t.Fatalf("realms: %v", kr.Realms())
	}
}
