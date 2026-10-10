package fetcher

import (
	"database/sql"
	"log"
	"math/rand"
	"sync"
	"time"
)

// SourceManager 代理源管理器（断路器）
type SourceManager struct {
	db *sql.DB
	mu sync.RWMutex
}

func NewSourceManager(db *sql.DB) *SourceManager {
	return &SourceManager{db: db}
}

// CanUseSource 判断源是否可用
func (sm *SourceManager) CanUseSource(url string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var status string
	var disabledUntil sql.NullTime
	err := sm.db.QueryRow(
		`SELECT status, disabled_until FROM source_status WHERE url = ?`,
		url,
	).Scan(&status, &disabledUntil)

	// 源不存在，默认可用
	if err != nil {
		return true
	}

	// 检查是否被禁用且还在冷却期
	if status == "disabled" && disabledUntil.Valid {
		if time.Now().Before(disabledUntil.Time) {
			return false
		}
		// 冷却期结束，重置状态
		sm.db.Exec(`UPDATE source_status SET status = 'active', consecutive_fails = 0 WHERE url = ?`, url)
		return true
	}

	return status != "disabled"
}

// RecordSuccess 记录源抓取成功
func (sm *SourceManager) RecordSuccess(url string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.db.Exec(`
		INSERT INTO source_status (url, success_count, consecutive_fails, last_success, status) 
		VALUES (?, 1, 0, CURRENT_TIMESTAMP, 'active')
		ON CONFLICT(url) DO UPDATE SET 
			success_count = success_count + 1,
			consecutive_fails = 0,
			last_success = CURRENT_TIMESTAMP,
			status = 'active'
	`, url)
}

// RecordFail 记录源抓取失败
func (sm *SourceManager) RecordFail(url string, failThreshold, disableThreshold, cooldownMinutes int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 增加失败计数
	sm.db.Exec(`
		INSERT INTO source_status (url, fail_count, consecutive_fails, last_fail) 
		VALUES (?, 1, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(url) DO UPDATE SET 
			fail_count = fail_count + 1,
			consecutive_fails = consecutive_fails + 1,
			last_fail = CURRENT_TIMESTAMP
	`, url)

	// 检查是否需要降级或禁用
	var consecutiveFails int
	sm.db.QueryRow(`SELECT consecutive_fails FROM source_status WHERE url = ?`, url).Scan(&consecutiveFails)

	if consecutiveFails >= disableThreshold {
		// 禁用源。冷却时间加随机抖动（吸收 nfx/slrp 的 sources/try.go:117-121 随机退避）：
		// 固定冷却会让一批同时挂掉的源在同一时刻集体重试，等于周期性自我 DDoS。
		jitter := time.Duration(rand.Int63n(int64(time.Duration(cooldownMinutes) * time.Minute / 2)))
		disabledUntil := time.Now().Add(time.Duration(cooldownMinutes)*time.Minute + jitter)
		sm.db.Exec(
			`UPDATE source_status SET status = 'disabled', disabled_until = ? WHERE url = ?`,
			disabledUntil, url,
		)
		log.Printf("[source] ⛔ 禁用源（连续失败%d次）: %s (冷却%d分钟+抖动%v)",
			consecutiveFails, url, cooldownMinutes, jitter.Round(time.Second))
	} else if consecutiveFails >= failThreshold {
		// 降级源
		sm.db.Exec(`UPDATE source_status SET status = 'degraded' WHERE url = ?`, url)
		log.Printf("[source] ⚠️  降级源（连续失败%d次）: %s", consecutiveFails, url)
	}
}

// GetSourceStats 获取所有源的统计信息
func (sm *SourceManager) GetSourceStats() ([]map[string]interface{}, error) {
	rows, err := sm.db.Query(`
		SELECT url, success_count, fail_count, consecutive_fails, 
		       last_success, last_fail, status 
		FROM source_status 
		ORDER BY success_count DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []map[string]interface{}
	for rows.Next() {
		var url, status string
		var successCount, failCount, consecutiveFails int
		var lastSuccess, lastFail sql.NullTime

		rows.Scan(&url, &successCount, &failCount, &consecutiveFails, &lastSuccess, &lastFail, &status)

		stats = append(stats, map[string]interface{}{
			"url":               url,
			"success_count":     successCount,
			"fail_count":        failCount,
			"consecutive_fails": consecutiveFails,
			"last_success":      lastSuccess,
			"last_fail":         lastFail,
			"status":            status,
		})
	}
	return stats, nil
}
