package store

import (
	"errors"
	"strings"
	"testing"
)

// "目录被另一个进程占用"要变成一条**能照着做的中文说明**，而不是 Badger 英文原文。
//
// 为什么重要：部署者看到 "Cannot acquire directory lock" 时不知道这是**设计**
// （单写者模型）还是故障，更不知道该怎么做。
func TestOpenReportsLockedDirectoryAsActionableError(t *testing.T) {
	for _, raw := range []string{
		`Cannot acquire directory lock on "/tmp/x".  Another process is using this Badger database. err: resource temporarily unavailable`,
		"resource temporarily unavailable",
		"failed to acquire directory lock",
	} {
		if !isDirLocked(errors.New(raw)) {
			t.Fatalf("应识别为目录占用: %q", raw)
		}
	}
	if isDirLocked(errors.New("disk full")) || isDirLocked(nil) {
		t.Fatal("其它错误不该被误判成目录占用")
	}
	e := &ErrLocked{Dir: "/tmp/data", Cause: errors.New("resource temporarily unavailable")}
	msg := e.Error()
	for _, want := range []string{"/tmp/data", "另一个进程", "单写者", "只留一个进程"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误说明应包含 %q: %s", want, msg)
		}
	}
	if !errors.Is(e, e.Cause) {
		t.Fatal("应能 unwrap 到原始错误")
	}
}
