package logger

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelError Level = "error"
)

type Entry struct {
	Seq     int       `json:"seq"`
	Time    time.Time `json:"time"`
	Level   Level     `json:"level"`
	Message string    `json:"message"`
}

type LogPage struct {
	Entries []Entry `json:"entries"`
	LastSeq int     `json:"last_seq"`
}

var (
	mu           sync.RWMutex
	entries      []Entry
	nextSeq      = 1
	maxEntries   = 2000
	currentLevel = LevelInfo

	// 文件 sink 状态
	fileMu     sync.Mutex
	fileDir    string   // 日志目录，空表示未启用文件 sink
	fileHandle *os.File // 当天日志句柄
	fileDate   string   // 当前打开文件对应的 YYYYMMDD，跨日时关闭并新建
	retainDays = 7      // 保留最近 N 天的归档（明文 + .gz 一起算）
)

// InitFile 启动文件 sink。
// dir 是日志目录（推荐 ~/.qoder2api/logs），不存在会自动创建。
// 启动时会做两件事：
//  1. 把昨天及更早的明文 .log 压成 .log.gz（跨日归档）
//  2. 删除超过 retainDays 的归档文件
//
// 返回错误时调用方应仅记 stderr 警告并继续运行（保持内存+stdout 双 sink 不影响业务）。
func InitFile(dir string) error {
	if dir == "" {
		return fmt.Errorf("empty log dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir log dir: %w", err)
	}
	fileMu.Lock()
	fileDir = dir
	fileMu.Unlock()

	// 启动时归档历史明文 + 清理过期
	archiveOldPlainLogs(dir)
	cleanupOldArchives(dir, retainDays)

	// 立即打开当天文件，验证可写
	fileMu.Lock()
	_, err := ensureFile()
	fileMu.Unlock()
	if err != nil {
		fileMu.Lock()
		fileDir = ""
		fileMu.Unlock()
		return err
	}
	return nil
}

// Close 关闭文件 sink，应用退出前调用一次。
func Close() {
	fileMu.Lock()
	defer fileMu.Unlock()
	if fileHandle != nil {
		_ = fileHandle.Close()
		fileHandle = nil
		fileDate = ""
	}
}

// ensureFile 确保 fileHandle 指向今天的日志文件。跨日时会关闭旧句柄、
// 把昨天的明文压成 .gz、再开新文件。**调用方必须已持有 fileMu。**
func ensureFile() (*os.File, error) {
	if fileDir == "" {
		return nil, fmt.Errorf("file sink not initialized")
	}
	today := time.Now().Format("20060102")
	if fileHandle != nil && fileDate == today {
		return fileHandle, nil
	}
	// 关闭旧文件并归档
	if fileHandle != nil {
		_ = fileHandle.Close()
		fileHandle = nil
		// 跨日触发归档（异步即可，归档/清理是 IO 操作，不在持锁内做）
		go func(dir string) {
			archiveOldPlainLogs(dir)
			cleanupOldArchives(dir, retainDays)
		}(fileDir)
	}
	path := filepath.Join(fileDir, "qccg-"+today+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fileHandle = f
	fileDate = today
	return f, nil
}

// archiveOldPlainLogs 把目录里所有非今天的 qccg-YYYYMMDD.log 压成 .log.gz 后删除原文件。
func archiveOldPlainLogs(dir string) {
	today := time.Now().Format("20060102")
	matches, _ := filepath.Glob(filepath.Join(dir, "qccg-*.log"))
	for _, p := range matches {
		base := filepath.Base(p)
		// qccg-YYYYMMDD.log → 取出 YYYYMMDD
		if len(base) < len("qccg-YYYYMMDD.log") {
			continue
		}
		datePart := strings.TrimPrefix(strings.TrimSuffix(base, ".log"), "qccg-")
		if datePart == today {
			continue
		}
		if err := gzipFile(p, p+".gz"); err == nil {
			_ = os.Remove(p)
		}
	}
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		_ = gz.Close()
		return err
	}
	return gz.Close()
}

// cleanupOldArchives 删除 retainDays 之外的 .log 和 .log.gz 文件。
func cleanupOldArchives(dir string, retain int) {
	if retain <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -retain)
	for _, glob := range []string{"qccg-*.log", "qccg-*.log.gz"} {
		matches, _ := filepath.Glob(filepath.Join(dir, glob))
		for _, p := range matches {
			st, err := os.Stat(p)
			if err != nil {
				continue
			}
			if st.ModTime().Before(cutoff) {
				_ = os.Remove(p)
			}
		}
	}
}

func SetLevel(level string) {
	mu.Lock()
	defer mu.Unlock()
	currentLevel = Level(level)
}

func shouldLog(level Level) bool {
	mu.RLock()
	defer mu.RUnlock()
	priority := map[Level]int{LevelDebug: 0, LevelInfo: 1, LevelError: 2}
	return priority[level] >= priority[currentLevel]
}

func log(level Level, format string, args ...interface{}) {
	if !shouldLog(level) {
		return
	}
	entry := Entry{
		Seq:     0, // 在锁内赋值
		Time:    time.Now(),
		Level:   level,
		Message: fmt.Sprintf(format, args...),
	}

	// sink 1: 内存环
	mu.Lock()
	entry.Seq = nextSeq
	nextSeq++
	entries = append(entries, entry)
	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
	mu.Unlock()

	// sink 2: stdout（dev 模式 / 终端启动时可见）
	line := fmt.Sprintf("[%s][%s] %s", entry.Time.Format("15:04:05.000"), level, entry.Message)
	fmt.Println(line)

	// sink 3: 文件（GUI 启动时事后翻日志的唯一入口）
	fileMu.Lock()
	if f, err := ensureFile(); err == nil {
		_, _ = f.WriteString(line + "\n")
	}
	fileMu.Unlock()
}

func Debug(format string, args ...interface{}) { log(LevelDebug, format, args...) }
func Info(format string, args ...interface{})  { log(LevelInfo, format, args...) }
func Error(format string, args ...interface{}) { log(LevelError, format, args...) }

// Warn 告警级日志（与 Error 同级输出，语义上区分「需关注但未失败」）。
// 用于必须醒目、但不构成故障的提示（如 fail-open 放行、指纹漂移告警）。
func Warn(format string, args ...interface{}) { log(LevelError, "WARN: "+fmt.Sprintf(format, args...)) }

// GetLogs 返回最近 limit 条日志（limit<=0 返回全部）。
func GetLogs(limit int) []Entry {
	mu.RLock()
	defer mu.RUnlock()
	if limit <= 0 || limit > len(entries) {
		limit = len(entries)
	}
	start := len(entries) - limit
	result := make([]Entry, limit)
	copy(result, entries[start:])
	return result
}

// GetLogsSince 返回 seq > afterSeq 的增量条目及当前最大 seq。
// afterSeq=0 等价于全量拉取（最多 limit 条）。
func GetLogsSince(afterSeq, limit int) LogPage {
	mu.RLock()
	defer mu.RUnlock()
	var result []Entry
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Seq <= afterSeq {
			break
		}
		result = append(result, entries[i])
	}
	// 反转为正序
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	if limit > 0 && len(result) > limit {
		result = result[len(result)-limit:]
	}
	last := 0
	if len(entries) > 0 {
		last = entries[len(entries)-1].Seq
	}
	return LogPage{Entries: result, LastSeq: last}
}

func Clear() {
	mu.Lock()
	defer mu.Unlock()
	entries = []Entry{}
}
