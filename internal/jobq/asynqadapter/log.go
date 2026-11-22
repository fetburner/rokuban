package asynqadapter

import (
	"fmt"
	"log/slog"
	"os"
)

type asynqLogger struct{}

// Debug は asynq のデバッグログを出す。
func (asynqLogger) Debug(args ...any) { slog.Debug(fmt.Sprint(args...)) }

// Info は asynq の情報ログを出す。
func (asynqLogger) Info(args ...any) { slog.Info(fmt.Sprint(args...)) }

// Warn は asynq の警告ログを出す。
func (asynqLogger) Warn(args ...any) { slog.Warn(fmt.Sprint(args...)) }

// Error は asynq のエラーログを出す。
func (asynqLogger) Error(args ...any) { slog.Error(fmt.Sprint(args...)) }

// Fatal は asynq の致命的エラーを記録し、プロセスを終了する。
func (asynqLogger) Fatal(args ...any) {
	slog.Error(fmt.Sprint(args...))
	os.Exit(1)
}
