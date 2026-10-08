package evaldata

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/willove/cumulus/internal/evalfcore"
)

// SplitName 是校准切分的名字。三份的用途固定，不许混用：
//   - calib：拟合曲线与阈值（可以反复看）；
//   - val：调参（可以先看几次，看过就记账）；
//   - lockbox：**只看一次**的终验（看完换一份，反复看等于没有锁箱）。
type SplitName string

const (
	SplitCalib   SplitName = "calib"
	SplitVal     SplitName = "val"
	SplitLockbox SplitName = "lockbox"
)

// DefaultSplit 是默认比例（校准 50% / 验证 25% / 锁箱 25%）。
func DefaultSplit() (calib, val float64) { return 0.50, 0.25 }

// SplitItems 按**题 id 的确定性哈希**切三份：同一份题集切法稳定，且与
// 输入顺序无关——锁箱纪律要求"这一题永远是锁箱题"，否则上一轮看过的
// 题会悄悄滑进校准集。比例是 calib/val 两份的占比，余下进锁箱。
//
// 用 id 而不是位置：题集增删时，已有题的去处不变（新增题按自己的哈希
// 落座），所以校准集可以长大而不污染锁箱。
func SplitItems(items []evalfcore.Item, calibFrac, valFrac float64) map[SplitName][]evalfcore.Item {
	out := map[SplitName][]evalfcore.Item{
		SplitCalib:   {},
		SplitVal:     {},
		SplitLockbox: {},
	}
	if calibFrac <= 0 {
		calibFrac, valFrac = DefaultSplit()
	}
	if valFrac < 0 {
		valFrac = 0
	}
	if calibFrac+valFrac > 1 {
		calibFrac = 1 - valFrac
	}
	for _, it := range items {
		u := splitUnit(it.ID)
		switch {
		case u < calibFrac:
			out[SplitCalib] = append(out[SplitCalib], it)
		case u < calibFrac+valFrac:
			out[SplitVal] = append(out[SplitVal], it)
		default:
			out[SplitLockbox] = append(out[SplitLockbox], it)
		}
	}
	return out
}

// splitUnit 把题 id 映到 [0,1)：sha256 的前 8 字节。固定盐：换算法等于
// 换切分，锁箱的历史会失效——所以盐与算法都要当契约对待。
func splitUnit(id string) float64 {
	sum := sha256.Sum256([]byte("cumulus-split-v1|" + id))
	return float64(binary.BigEndian.Uint64(sum[:8])) / float64(1<<64)
}
