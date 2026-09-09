// Package applock 提供跨进程的单实例锁。
//
// 只用于 serve：两个 serve 同时跑意味着重复采样、重复告警、重复 rollup，
// 数据库不会损坏（SQLite 的 WAL + busy_timeout 会串行化写入），但统计会
// 被拉偏。probe / speed 是一次性动作，不加锁 —— 服务在后台跑的时候，
// 用户仍然应该能随手 `isp-probe probe` 看一眼。
//
// 两个平台的实现都依赖内核在进程结束时自动释放锁，因此不存在陈旧锁的问题。
// 这也是不采用「写 PID 文件再判断进程是否存活」那套做法的原因。
package applock

import "errors"

// ErrLocked 表示锁已被另一个进程持有。
var ErrLocked = errors.New("已有实例持有锁")

// Acquire 尝试取得独占锁，失败时返回包装了 ErrLocked 的错误。
func Acquire(path string) (*Lock, error) { return acquire(path) }

// Release 释放锁。重复调用是安全的。
func (l *Lock) Release() error { return l.release() }
