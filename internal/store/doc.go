// Package store 实现 SlopGuard 全局持久化层（SQLite，纯 Go）。
//
// 使用 modernc.org/sqlite 以保持零 CGO；数据库默认位于
// ~/.slopguard/slopguard.db，打开时自动完成 schema 迁移并开启 WAL。
package store
