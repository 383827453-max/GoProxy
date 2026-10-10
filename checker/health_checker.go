package checker

import (
	"log"
	"time"

	"goproxy/config"
	"goproxy/pool"
	"goproxy/storage"
	"goproxy/validator"
)

// HealthChecker 健康检查器
type HealthChecker struct {
	storage   *storage.Storage
	validator *validator.Validator
	cfg       *config.Config
	poolMgr   *pool.Manager
}

func NewHealthChecker(s *storage.Storage, v *validator.Validator, cfg *config.Config, pm *pool.Manager) *HealthChecker {
	return &HealthChecker{
		storage:   s,
		validator: v,
		cfg:       cfg,
		poolMgr:   pm,
	}
}

// failureOutcome 描述一次健康检查失败后应如何处理（纯函数，便于单测）。
//
// 三态策略（吸收 tongsq/proxy-collect 的 OK/RECHECK/DEAD 与 nfx/slrp 的熔断复活思路）：
//
//	failCount <  threshold → degraded（降权但仍参与选择，给抖动代理恢复机会）
//	failCount >= threshold → 移除（免费代理删除；订阅代理由调用方改为禁用）
//
// 旧实现是"失败次数 >= 3 就删"，免费代理抖动大时会周期性把池子清空。
func failureOutcome(failCount, threshold int) (status string, remove bool) {
	if threshold <= 0 {
		threshold = 5
	}
	if failCount < threshold {
		return "degraded", false
	}
	return "", true
}

// RunOnce 执行一次健康检查
func (hc *HealthChecker) RunOnce() {
	start := time.Now()
	log.Println("[health] 开始健康检查...")

	// 获取池子状态
	status, err := hc.poolMgr.GetStatus()
	if err != nil {
		log.Printf("[health] 获取状态失败: %v", err)
		return
	}

	// 健康状态且S级占比高时，跳过S级代理检查
	skipSGrade := status.State == "healthy"
	dist, _ := hc.storage.GetQualityDistribution()
	sGradeCount := dist["S"]
	totalCount := status.Total
	if totalCount > 0 && float64(sGradeCount)/float64(totalCount) > 0.3 {
		skipSGrade = true
	}

	// 批量获取需要检查的代理
	proxies, err := hc.storage.GetBatchForHealthCheck(hc.cfg.HealthCheckBatchSize, skipSGrade)
	if err != nil {
		log.Printf("[health] 获取检查批次失败: %v", err)
		return
	}

	if len(proxies) == 0 {
		log.Println("[health] 无需检查的代理")
		hc.reanimateDisabled()
		return
	}

	log.Printf("[health] 检查 %d 个代理（跳过S级=%v）", len(proxies), skipSGrade)

	// 执行验证
	validCount := 0
	removeCount := 0
	degradedCount := 0
	updateCount := 0

	for result := range hc.validator.ValidateStream(proxies) {
		if result.Valid {
			validCount++
			// 探测成功即清失败计数并恢复 active（吸收 easy-proxies 的 forceRelease：
			// 修掉"探针说好、池里还黑着"的不一致）
			hc.storage.ResetFail(result.Proxy.Address)
			if result.Proxy.Status != "" && result.Proxy.Status != "active" {
				hc.storage.SetStatus(result.Proxy.Address, "active")
			}
			latencyMs := int(result.Latency.Milliseconds())
			exitIP, exitLoc := result.ExitIP, result.ExitLocation
			if exitIP == "" {
				exitIP, exitLoc = result.Proxy.ExitIP, result.Proxy.ExitLocation
			}
			if err := hc.storage.UpdateExitInfo(result.Proxy.Address, exitIP, exitLoc, latencyMs); err == nil {
				updateCount++
			}
			continue
		}

		// 失败：三态降级 → 达到阈值才移除
		hc.storage.IncrementFailCount(result.Proxy.Address)
		nextFail := result.Proxy.FailCount + 1
		if _, remove := failureOutcome(nextFail, hc.cfg.FailureThreshold); remove {
			if result.Proxy.Source == "custom" {
				// 订阅代理：禁用而非删除（保留探测唤醒）
				hc.storage.DisableProxy(result.Proxy.Address)
			} else {
				hc.storage.Delete(result.Proxy.Address)
			}
			removeCount++
		} else {
			hc.storage.SetStatus(result.Proxy.Address, "degraded")
			degradedCount++
		}
	}

	// 半开探测：给已禁用的免费代理一次复活机会
	revived := hc.reanimateDisabled()

	elapsed := time.Since(start)
	log.Printf("[health] 完成: 验证%d 有效%d 更新%d 降级%d 移除%d 复活%d 耗时%v",
		len(proxies), validCount, updateCount, degradedCount, removeCount, revived, elapsed)
}

// reanimateDisabled 对已禁用的免费代理做一次半开探测，健康的恢复为 active。
// 返回复活数量。这是"熔断→冷却→半开→复活"里的后半段：只有熔断没有复活，
// 池子会单向萎缩。
func (hc *HealthChecker) reanimateDisabled() int {
	batch := hc.cfg.ReanimateBatch
	if batch <= 0 {
		batch = 10
	}
	candidates, err := hc.storage.GetDisabledFreeProxies(batch)
	if err != nil || len(candidates) == 0 {
		return 0
	}
	revived := 0
	for result := range hc.validator.ValidateStream(candidates) {
		if !result.Valid {
			continue
		}
		if err := hc.storage.EnableProxy(result.Proxy.Address); err != nil {
			continue
		}
		hc.storage.ResetFail(result.Proxy.Address)
		hc.storage.SetStatus(result.Proxy.Address, "active")
		revived++
		log.Printf("[health] ♻️ 复活禁用代理: %s", result.Proxy.Address)
	}
	if revived > 0 {
		log.Printf("[health] 半开探测: 候选%d 复活%d", len(candidates), revived)
	}
	return revived
}

// StartBackground 后台定时健康检查
func (hc *HealthChecker) StartBackground() {
	ticker := time.NewTicker(time.Duration(hc.cfg.HealthCheckInterval) * time.Minute)
	go func() {
		for range ticker.C {
			hc.RunOnce()
		}
	}()
	log.Printf("[health] 健康检查器已启动，间隔 %d 分钟", hc.cfg.HealthCheckInterval)
}
