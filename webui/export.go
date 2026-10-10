package webui

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"goproxy/netguard"
	"goproxy/storage"
)

// apiExport 导出池中可用代理，供下游（opencode2api / clash / 环境变量）直接消费。
//
// GET /api/export
//
//	?protocol=socks5|http|all   默认 all
//	?source=free|custom|all     默认 all
//	?status=active|all          默认 active（排除 pending/degraded/disabled，只给下游能用的）
//	?format=uri|plain|json      默认 uri（uri 形如 socks5://1.2.3.4:1080）
//	?max_latency=3000           只导出延迟不超过该值的代理（毫秒，0=不限）
//	?min_grade=S|A|B|C          只导出该等级及以上（S 最优）
//	?limit=200                  最多导出条数（0=不限）
//
// 鉴权：管理员会话，或 X-Export-Token / ?token= 与 EXPORT_TOKEN 匹配。
func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	protocol := strings.ToLower(strings.TrimSpace(q.Get("protocol")))
	if protocol == "" {
		protocol = "all"
	}
	source := strings.ToLower(strings.TrimSpace(q.Get("source")))
	if source == "" {
		source = "all"
	}
	status := strings.ToLower(strings.TrimSpace(q.Get("status")))
	if status == "" {
		status = "active"
	}
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "" {
		format = "uri"
	}
	maxLatency, _ := strconv.Atoi(q.Get("max_latency"))
	minGrade := strings.ToUpper(strings.TrimSpace(q.Get("min_grade")))
	limit, _ := strconv.Atoi(q.Get("limit"))

	sourceFilter := ""
	switch source {
	case "free", "custom":
		sourceFilter = source
	}

	proxies, err := s.storage.GetAllFiltered(sourceFilter)
	if err != nil {
		jsonError(w, "读取代理失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	gradeRank := map[string]int{"S": 4, "A": 3, "B": 2, "C": 1, "": 0}
	minRank := gradeRank[minGrade]

	out := make([]storage.Proxy, 0, len(proxies))
	for _, p := range proxies {
		if protocol != "all" && p.Protocol != protocol {
			continue
		}
		// 默认只导出 active：pending（出口未验证）/disabled（地理或探测禁用）都不给下游
		if status != "all" {
			if p.Status != status {
				continue
			}
		} else if p.Status != "" && p.Status != "active" && p.Status != "degraded" {
			continue
		}
		if p.FailCount >= 3 {
			continue
		}
		if maxLatency > 0 && p.Latency > maxLatency {
			continue
		}
		if minRank > 0 && gradeRank[strings.ToUpper(p.QualityGrade)] < minRank {
			continue
		}
		out = append(out, p)
	}

	// 低延迟优先，便于下游优先命中快节点
	sort.SliceStable(out, func(i, j int) bool { return out[i].Latency < out[j].Latency })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}

	switch format {
	case "json":
		jsonOK(w, map[string]interface{}{
			"count":    len(out),
			"protocol": protocol,
			"source":   source,
			"status":   status,
			"proxies":  out,
		})
	case "plain":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Proxy-Count", strconv.Itoa(len(out)))
		var b strings.Builder
		for _, p := range out {
			b.WriteString(p.Address)
			b.WriteByte('\n')
		}
		fmt.Fprint(w, b.String())
	default: // uri
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Proxy-Count", strconv.Itoa(len(out)))
		var b strings.Builder
		for _, p := range out {
			b.WriteString(netguard.FormatProxyURI(p.Protocol, p.Address))
			b.WriteByte('\n')
		}
		fmt.Fprint(w, b.String())
	}
}
