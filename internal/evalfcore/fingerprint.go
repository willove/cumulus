package evalfcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
)

// Fingerprints 是冻结三指纹。运行期间改任何一样都不影响已冻结实验；
// 对比时三者不一致即不可比，只列理由不列差值（cumulus 的口径，照搬）。
type Fingerprints struct {
	ItemsSHA  string `json:"items_sha"`
	CorpusSHA string `json:"corpus_sha"`
	ConfigSHA string `json:"config_sha"`
}

// Equal 三指纹全同。
func (f Fingerprints) Equal(o Fingerprints) bool { return f == o }

// DiffReasons 列出不一致的项；空切片 = 可比。
func (f Fingerprints) DiffReasons(o Fingerprints) []string {
	var reasons []string
	if f.ItemsSHA != o.ItemsSHA {
		reasons = append(reasons, "items_sha differs")
	}
	if f.CorpusSHA != o.CorpusSHA {
		reasons = append(reasons, "corpus_sha differs")
	}
	if f.ConfigSHA != o.ConfigSHA {
		reasons = append(reasons, "config_sha differs")
	}
	return reasons
}

// Config 是运行配置。ConfigSHA 只覆盖生效内容：臂、判官模式、模型名、
// 掩码后的端点主机。**不含密钥与路径**——指纹要可公开对比。
type Config struct {
	Arms         []string `json:"arms"`          // rule / judge / closed-book
	JudgeMode    string   `json:"judge_mode"`    // offline / live / off
	Model        string   `json:"model"`         // 生效模型名
	EndpointHost string   `json:"endpoint_host"` // 已掩码
}

// SHA 计算配置指纹。确定性：键序固定（结构体字段序）。
func (c Config) SHA() string {
	buf, err := json.Marshal(c)
	if err != nil {
		return "" // 结构体只含字符串与字符串切片，不会失败
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// MaskHost 掩码端点：只留主机名，去掉用户信息、路径与查询。
// 指纹可比但不能泄密（cumulus 的同款决定）。
func MaskHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid"
	}
	return u.Host
}
