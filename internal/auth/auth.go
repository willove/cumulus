// Package auth 是 HTTP 面的**鉴权网关**：key → realm。
//
// 为什么 realm 由 key 推导、而不是由客户端声明：声明就是**自己说我是谁**。
// 多项目共用一个实例时（这正是"后续整合"要面对的形态），客户端说自己是谁毫无
// 意义——任何人都能声明别人的 realm。**身份只能来自凭证**。
//
// 三条纪律：
//
//  1. **默认宽容**：没配 key 时放行（本地/单机开发的老路径一字不变）。多租户部署
//     忘了配 key 是**危险**的，所以启动时要打印醒目提示，而不是静默。
//  2. **凭证缺失 ≠ 凭证错误**：401 与 403 分开（前端要能区分"登录"和"没权限"）。
//  3. **常数时间比较**：比较 key 用 subtle（时序侧信道），不用 ==
package auth

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Keyring 是 key → realm 的映射（凭证表）。
type Keyring struct {
	keys   map[string]string // key → realm
	relmns []string          // 有序 realm 名（/v1/status 报给人看）
}

// NewKeyring 装一张凭证表。entries 形如 `realm=key`。
// 重复 realm 取**第一个** key（后写的忽略）——一张表里一个 realm 一个 key，
// 免得"哪把钥匙算数"变成运维谜题。
func NewKeyring(entries []string) (*Keyring, error) {
	kr := &Keyring{keys: map[string]string{}}
	seenRealm := map[string]bool{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		realm, key, ok := strings.Cut(e, "=")
		realm, key = strings.TrimSpace(realm), strings.TrimSpace(key)
		if !ok || realm == "" || key == "" {
			return nil, fmt.Errorf("auth: 凭证格式应为 realm=key（收到 %q）", e)
		}
		if seenRealm[realm] {
			continue // 一个 realm 只留一把钥匙
		}
		seenRealm[realm] = true
		kr.keys[key] = realm
		kr.relmns = append(kr.relmns, realm)
	}
	sort.Strings(kr.relmns)
	return kr, nil
}

// ParseKeyringSpec 解析环境变量里的凭证表（`realm=key,realm2=key2`）。
// 空串 = 无凭证表（默认宽容）。
func ParseKeyringSpec(spec string) (*Keyring, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	return NewKeyring(strings.Split(spec, ","))
}

// Empty 表示没配凭证（默认放行）。
func (k *Keyring) Empty() bool { return k == nil || len(k.keys) == 0 }

// Realms 列出已配置的 realm（排序）。
func (k *Keyring) Realms() []string {
	if k == nil {
		return nil
	}
	return append([]string(nil), k.relmns...)
}

// ErrNoCredential / ErrBadCredential 让 handler 能把 401 与 403 分开。
type ErrNoCredential struct{}
type ErrBadCredential struct{}

func (ErrNoCredential) Error() string  { return "缺少凭证" }
func (ErrBadCredential) Error() string { return "凭证无效" }

// Authenticate 从请求头取凭证并推导 realm。
//
// 支持 `Authorization: Bearer <key>` 与 `X-Cumulus-Key: <key>` 两种写法。
// 返回的 realm 是**唯一可信的身份来源**。
func (k *Keyring) Authenticate(r *http.Request) (realm string, err error) {
	if k.Empty() {
		return "", nil // 默认宽容
	}
	key := bearer(r.Header.Get("Authorization"))
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("X-Cumulus-Key"))
	}
	if key == "" {
		return "", ErrNoCredential{}
	}
	// 不短路：遍历全部 key 做常数时间比较，避免"哪一位先不同"泄露信息。
	found := ""
	for kk, rl := range k.keys {
		if subtle.ConstantTimeCompare([]byte(kk), []byte(key)) == 1 {
			found = rl
		}
	}
	if found == "" {
		return "", ErrBadCredential{}
	}
	return found, nil
}

func bearer(h string) string {
	const prefix = "bearer "
	h = strings.TrimSpace(h)
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// WriteAuthError 把鉴权失败写成 401/403（区分"没凭证"与"凭证不对"）。
func WriteAuthError(w http.ResponseWriter, err error) {
	code := http.StatusUnauthorized
	msg := "缺少凭证"
	if _, ok := err.(ErrBadCredential); ok {
		code, msg = http.StatusForbidden, "凭证无效"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	// 故意不写 realm 列表：那是给攻击者的地图
	_, _ = w.Write([]byte(`{"error":"` + msg + `","hint":"Authorization: Bearer <key> 或 X-Cumulus-Key"}`))
}
